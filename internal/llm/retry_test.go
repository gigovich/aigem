package llm

import (
	"context"
	"errors"
	"testing"
	"time"
)

type flakyStreamer struct {
	failures int
	calls    int
	err      error
	emit     bool // emit a content delta before failing
}

func (f *flakyStreamer) Stream(_ context.Context, _ []Message, _ []Tool, _ float64,
	onEvent func(StreamEvent)) (Message, error) {
	f.calls++
	if f.emit && onEvent != nil {
		onEvent(StreamEvent{Content: "partial"})
	}
	if f.calls <= f.failures {
		return Message{}, f.err
	}
	return Message{Role: RoleAssistant, Content: "ok"}, nil
}

func newTestRetrying(inner streamer, attempts int) *Retrying {
	r := NewRetrying(inner, attempts)
	r.base = time.Millisecond
	return r
}

func TestRetryingRecoversFromTransientErrors(t *testing.T) {
	f := &flakyStreamer{failures: 2, err: errors.New("llm: status 503: upstream reset")}
	r := newTestRetrying(f, 3)
	msg, err := r.Stream(context.Background(), nil, nil, 0, nil)
	if err != nil || msg.Content != "ok" {
		t.Fatalf("Stream = %q, %v; want ok, nil", msg.Content, err)
	}
	if f.calls != 3 {
		t.Fatalf("calls = %d, want 3", f.calls)
	}
}

func TestRetryingNotifiesEachBackoff(t *testing.T) {
	f := &flakyStreamer{failures: 2, err: errors.New("llm: status 503: server_is_overloaded")}
	r := newTestRetrying(f, 3)
	var got []RetryNotice
	r.SetOnRetry(func(n RetryNotice) { got = append(got, n) })
	if _, err := r.Stream(context.Background(), nil, nil, 0, nil); err != nil {
		t.Fatal(err)
	}
	// One notice per wait: two failures, two waits, then the third call succeeds.
	if len(got) != 2 {
		t.Fatalf("notices = %d, want 2 (%+v)", len(got), got)
	}
	for i, n := range got {
		if n.Attempt != i+1 || n.Attempts != 3 || n.Err == nil {
			t.Fatalf("notice %d = %+v", i, n)
		}
	}
	if got[1].Delay <= got[0].Delay {
		t.Fatalf("delay did not back off: %v then %v", got[0].Delay, got[1].Delay)
	}
}

func TestRetryingDoesNotNotifyWhenItWillNotRetry(t *testing.T) {
	// A non-transient failure and a stream that already emitted text are both
	// returned as-is; announcing a retry that never happens would be a lie.
	for _, f := range []*flakyStreamer{
		{failures: 10, err: errors.New("llm: status 400: invalid_request_error")},
		{failures: 10, err: errors.New("llm: status 503: server_is_overloaded"), emit: true},
	} {
		r := newTestRetrying(f, 3)
		notified := false
		r.SetOnRetry(func(RetryNotice) { notified = true })
		if _, err := r.Stream(context.Background(), nil, nil, 0, nil); err == nil {
			t.Fatal("expected the error to surface")
		}
		if notified {
			t.Fatalf("notified for %v although no retry was made", f.err)
		}
	}
}

func TestRetryingGivesUpAfterAttempts(t *testing.T) {
	f := &flakyStreamer{failures: 10, err: errors.New("llm: status 429: rate limited")}
	r := newTestRetrying(f, 3)
	if _, err := r.Stream(context.Background(), nil, nil, 0, nil); err == nil {
		t.Fatal("expected the final error to surface")
	}
	if f.calls != 3 {
		t.Fatalf("calls = %d, want 3", f.calls)
	}
}

func TestRetryingDoesNotRetryNonTransient(t *testing.T) {
	f := &flakyStreamer{failures: 10, err: errors.New("llm: status 400: invalid_request_error")}
	r := newTestRetrying(f, 3)
	if _, err := r.Stream(context.Background(), nil, nil, 0, nil); err == nil {
		t.Fatal("expected error")
	}
	if f.calls != 1 {
		t.Fatalf("calls = %d, want 1 (no retry)", f.calls)
	}
}

func TestRetryingDoesNotRetryAfterEmittedContent(t *testing.T) {
	f := &flakyStreamer{failures: 10, emit: true, err: errors.New("llm: read stream: unexpected EOF")}
	r := newTestRetrying(f, 3)
	if _, err := r.Stream(context.Background(), nil, nil, 0, func(StreamEvent) {}); err == nil {
		t.Fatal("expected error")
	}
	if f.calls != 1 {
		t.Fatalf("calls = %d, want 1 (deltas already delivered)", f.calls)
	}
}

// The no-retry-after-emit rule protects a caller from seeing the same deltas
// twice, which presumes a caller that shows them. A subagent's are shown
// nowhere and its partial answer is dropped on failure, so there the rule only
// converted a transient provider error into a failed delegation - and on a
// reasoning model, whose deltas start at once, into nearly every one.
func TestRetryingRetriesAfterEmitWhenNobodySawIt(t *testing.T) {
	f := &flakyStreamer{failures: 1, emit: true, err: errors.New("llm: status 500: server_error")}
	r := newTestRetrying(f, 3)

	msg, err := r.Stream(WithRetryAfterEmit(context.Background()), nil, nil, 0, func(StreamEvent) {})
	if err != nil {
		t.Fatalf("expected the retry to succeed: %v", err)
	}
	if msg.Content != "ok" {
		t.Fatalf("content = %q, want the retried answer", msg.Content)
	}
	if f.calls != 2 {
		t.Fatalf("calls = %d, want 2", f.calls)
	}
}

// The mark does not make anything else retryable.
func TestRetryAfterEmitStillRespectsTheOtherLimits(t *testing.T) {
	fatal := &flakyStreamer{failures: 10, emit: true, err: errors.New("llm: status 401: unauthorized")}
	r := newTestRetrying(fatal, 3)
	if _, err := r.Stream(WithRetryAfterEmit(context.Background()), nil, nil, 0, nil); err == nil {
		t.Fatal("expected error")
	}
	if fatal.calls != 1 {
		t.Fatalf("calls = %d, want 1 - an auth failure is still terminal", fatal.calls)
	}

	transient := &flakyStreamer{failures: 10, emit: true, err: errors.New("llm: status 503")}
	r = newTestRetrying(transient, 3)
	if _, err := r.Stream(WithRetryAfterEmit(context.Background()), nil, nil, 0, nil); err == nil {
		t.Fatal("expected error")
	}
	if transient.calls != 3 {
		t.Fatalf("calls = %d, want the attempt cap of 3", transient.calls)
	}
}

func TestRetryAfterEmitDefaultsOff(t *testing.T) {
	if RetryAfterEmit(context.Background()) {
		t.Fatal("a plain context must not enable retry-after-emit")
	}
	if !RetryAfterEmit(WithRetryAfterEmit(context.Background())) {
		t.Fatal("the mark did not take")
	}
}

func TestIsAuthErr(t *testing.T) {
	// Terminal auth failures: not retryable, not a context overflow.
	auth := []string{
		"llm: status 401: unauthorized",
		`{"error":{"code":"invalid_api_key"}}`,
		"responses: auth: oauth2: cannot fetch token: 403 Forbidden",
	}
	for _, s := range auth {
		if !IsAuthErr(errors.New(s)) {
			t.Errorf("expected auth: %s", s)
		}
		if IsRequestShapeErr(errors.New(s)) {
			t.Errorf("auth error must not be request-shape: %s", s)
		}
		if IsTransientErr(errors.New(s)) {
			t.Errorf("auth error must not be transient: %s", s)
		}
	}
	notAuth := []string{
		`responses: stream error: {"code":"context_length_exceeded"}`,
		"tool bash failed: exit status 1",
	}
	for _, s := range notAuth {
		if IsAuthErr(errors.New(s)) {
			t.Errorf("expected not auth: %s", s)
		}
	}
	if IsAuthErr(nil) {
		t.Error("nil must not be auth")
	}
}

// A reused-refresh-token collision is a self-healing cross-process race, not a
// dead credential: it must be retryable (a retry reloads the peer's token) and
// must never read as terminal auth or as a context-window overflow, even though
// its error text contains the substrings all three classifiers key on.
func TestRefreshCollisionIsRetryable(t *testing.T) {
	reused := errors.New(`responses: auth: oauth2: cannot fetch token: 401 Unauthorized: ` +
		`{"error":{"type":"invalid_request_error","code":"refresh_token_reused"}}`)
	if !IsTransientErr(reused) {
		t.Error("reused refresh token must be transient (retryable)")
	}
	if IsAuthErr(reused) {
		t.Error("reused refresh token must not be terminal auth")
	}
	if IsRequestShapeErr(reused) {
		t.Error("reused refresh token must not be request-shape")
	}
}

func TestIsTransientErr(t *testing.T) {
	transient := []string{
		"llm: status 429: rate limited",
		"responses: status 503: overloaded",
		`responses: stream error: {"error":{"type":"server_error"}}`,
		"llm: read stream: unexpected EOF",
		`Post "https://x": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`,
		`responses: status 429: {"error":{"type":"usage_limit_reached"}}`,
	}
	for _, s := range transient {
		if !IsTransientErr(errors.New(s)) {
			t.Errorf("expected transient: %s", s)
		}
	}
	notTransient := []string{
		"llm: status 400: bad request",
		`responses: stream error: {"code":"context_length_exceeded"}`,
		"read stream: context canceled",
		"tool bash failed: exit status 1",
	}
	for _, s := range notTransient {
		if IsTransientErr(errors.New(s)) {
			t.Errorf("expected not transient: %s", s)
		}
	}
	if IsTransientErr(nil) {
		t.Error("nil must not be transient")
	}
}
