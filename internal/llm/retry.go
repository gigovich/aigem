package llm

import (
	"context"
	"strings"
	"sync"
	"time"
)

// isRefreshCollision reports whether an error is OpenAI's "refresh_token_reused":
// several bot processes share one auth.json, and OpenAI's refresh tokens are
// one-time use, so when a peer rotates the token first this process's snapshot is
// rejected as reused. It is NOT a dead credential - the peer has persisted a valid
// token - so it is retryable (persistSource reloads the peer's token on retry),
// not a terminal auth failure. Kept distinct from IsAuthErr and IsRequestShapeErr,
// whose substrings the same error also matches.
func isRefreshCollision(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "refresh_token_reused")
}

// IsAuthErr reports whether an LLM call error is a terminal provider
// authentication or authorization failure - a revoked or expired OAuth token, a
// bad API key, or a 401/403. The operator must re-authenticate; neither a retry
// nor a smaller request clears it. A reused-refresh-token collision is excluded:
// that one is transient (see isRefreshCollision). Classified before request-shape
// because a provider OAuth error carries a "type":"invalid_request_error" body
// that would otherwise be mistaken for a context-window overflow.
func IsAuthErr(err error) bool {
	if err == nil || isRefreshCollision(err) {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, p := range []string{
		"oauth", "refresh_token", "cannot fetch token", "invalid_api_key",
		"unauthorized", "status 401", "status 403", "authentication",
	} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// IsRequestShapeErr reports whether an LLM call error was caused by the shape
// of the request itself - a context-window overflow or a malformed request -
// which neither a retry nor a later resume can clear; only changing the input
// can. Classification is by message text because the errors cross several
// adapters as formatted strings.
func IsRequestShapeErr(err error) bool {
	if err == nil {
		return false
	}
	// An auth failure or a retryable refresh collision can carry an
	// "invalid_request_error" body; neither is a request-shape problem, so rule
	// them out before the shape matches below.
	if IsAuthErr(err) || isRefreshCollision(err) {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, p := range []string{
		"context_length_exceeded", "context window", "maximum context", "invalid_request_error",
	} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// retryAfterEmitKey marks a stream whose deltas reach no one.
type retryAfterEmitKey struct{}

// WithRetryAfterEmit lets a stream be retried even after it has emitted text.
// The no-retry-after-emit rule exists so a caller is not shown the same deltas
// twice, which presumes the caller is showing them. A subagent's are not shown
// anywhere - its Events carry no content callback - and its partial answer is
// discarded when the run fails, so refusing the retry protects nothing and
// turns a transient provider hiccup into a failed delegation. On a reasoning
// model, where deltas start immediately, that is nearly every hiccup.
func WithRetryAfterEmit(ctx context.Context) context.Context {
	return context.WithValue(ctx, retryAfterEmitKey{}, true)
}

// RetryAfterEmit reports whether ctx was marked by WithRetryAfterEmit.
func RetryAfterEmit(ctx context.Context) bool {
	on, _ := ctx.Value(retryAfterEmitKey{}).(bool)
	return on
}

// Retrying re-issues a Stream that failed with a transient provider error
// (429/5xx, an aborted stream, a network hiccup) with exponential backoff, so
// an unattended bot rides out a hiccup instead of surfacing it into chat. A
// call that already streamed content to the caller is not retried - the deltas
// were delivered and a retry would duplicate them - unless the context says
// nobody received them (see WithRetryAfterEmit). A cancelled context or a
// non-transient error is never retried.
type Retrying struct {
	inner    streamer
	attempts int           // total tries, including the first
	base     time.Duration // first retry delay; doubles per retry up to maxRetryDelay

	mu      sync.RWMutex
	onRetry func(RetryNotice)
}

// RetryNotice describes one retry about to be waited out. An interactive
// front-end shows it so a backoff reads as a wait rather than as a hang; an
// unattended bot has no one watching and leaves it unset.
type RetryNotice struct {
	Attempt  int           // the attempt that just failed, 1-based
	Attempts int           // total attempts allowed
	Delay    time.Duration // wait before the next attempt
	Err      error         // the failure being retried
}

const (
	defaultRetryBase = 2 * time.Second
	maxRetryDelay    = 30 * time.Second
)

// NewRetrying wraps inner with up to attempts tries per Stream. attempts < 2
// makes it a passthrough.
func NewRetrying(inner streamer, attempts int) *Retrying {
	return &Retrying{inner: inner, attempts: attempts, base: defaultRetryBase}
}

// SetOnRetry installs a callback invoked before each backoff wait. It runs on
// the streaming goroutine, so it must not block.
func (r *Retrying) SetOnRetry(fn func(RetryNotice)) {
	r.mu.Lock()
	r.onRetry = fn
	r.mu.Unlock()
}

func (r *Retrying) notify(n RetryNotice) {
	r.mu.RLock()
	fn := r.onRetry
	r.mu.RUnlock()
	if fn != nil {
		fn(n)
	}
}

func (r *Retrying) Stream(ctx context.Context, messages []Message, tools []Tool, temperature float64,
	onEvent func(StreamEvent)) (Message, error) {
	delay := r.base
	for attempt := 1; ; attempt++ {
		emitted := false
		guard := func(e StreamEvent) {
			if e.Content != "" || e.Reasoning != "" {
				emitted = true
			}
			if onEvent != nil {
				onEvent(e)
			}
		}
		msg, err := r.inner.Stream(ctx, messages, tools, temperature, guard)
		if err == nil {
			return msg, nil
		}
		if attempt >= r.attempts || (emitted && !RetryAfterEmit(ctx)) ||
			ctx.Err() != nil || !IsTransientErr(err) {
			return msg, err
		}
		r.notify(RetryNotice{Attempt: attempt, Attempts: r.attempts, Delay: delay, Err: err})
		sleepCtx(ctx, delay)
		if ctx.Err() != nil {
			return msg, err
		}
		if delay *= 2; delay > maxRetryDelay {
			delay = maxRetryDelay
		}
	}
}

// IsTransientErr reports whether an LLM call error is a transient provider or
// network failure that a later retry can plausibly clear: rate limits, 5xx
// statuses, provider "overloaded"/"server_error" stream events, or a dropped
// connection. Cancellation and request-shape errors (4xx other than 429) are
// not transient. Classification is by message text because the errors cross
// several adapters (chat-completions, Responses) as formatted strings.
func IsTransientErr(err error) bool {
	if err == nil {
		return false
	}
	// A reused-refresh-token collision is retryable: the peer that won the race
	// has persisted a valid token, so a retry reloads it and succeeds. Match it
	// before the auth/shape exclusions, whose substrings it also contains.
	if isRefreshCollision(err) {
		return true
	}
	// Request-shape and auth failures surface through the same stream/status
	// paths but no retry can clear them; rule them out before the positive
	// matches.
	if IsRequestShapeErr(err) || IsAuthErr(err) {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "context canceled") {
		return false
	}
	for _, p := range []string{
		"status 429", "status 500", "status 502", "status 503", "status 504", "status 529",
		"server_error", "service_unavailable", "server_is_overloaded", "overloaded",
		"usage_limit_reached", "rate_limit",
		"read stream", "stream error",
		"connection reset", "connection refused", "broken pipe", "unexpected eof",
		"deadline exceeded", "timeout", "temporary failure", "no such host",
	} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
