package llm

import (
	"context"
	"io"
	"slices"
	"sync"
)

// errBody reads an error response body, capped so a misbehaving endpoint cannot
// dump megabytes (or binary) into an error string.
func errBody(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 2048))
	return string(b)
}

// ModelInfo describes a single model: its identity and the limits that drive
// the context gauge and compaction. It is provider-neutral so the agent and TUI
// never branch on which transport is active.
type ModelInfo struct {
	Provider      string `json:"provider"`
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	MaxTokens     int    `json:"max_tokens,omitempty"`
	Reasoning     bool   `json:"reasoning,omitempty"`
	// Temperature pins the sampling temperature for models whose API rejects
	// any other value (e.g. kimi-for-coding only accepts 1). When set it
	// overrides whatever the caller asks for. Settable via models.json.
	Temperature *float64 `json:"temperature,omitempty"`
}

// Ref returns "provider/id", the canonical model reference.
func (m ModelInfo) Ref() string {
	if m.Provider == "" {
		return m.ID
	}
	return m.Provider + "/" + m.ID
}

// Backend is everything the agent and TUI need from an LLM transport. Both the
// chat-completions Client and the Responses adapter implement it, so switching
// providers is a pointer swap, not a code path.
type Backend interface {
	Stream(ctx context.Context, messages []Message, tools []Tool, temperature float64,
		onEvent func(StreamEvent)) (Message, error)
	// Tokenize returns an accurate token count when the backend can, else a
	// chars/4 estimate. Compaction tolerates the estimate.
	Tokenize(ctx context.Context, text string) (int, error)
	Model() ModelInfo
	// Endpoint is the base URL actually being called (for display), e.g. the Codex
	// backend on the subscription path rather than api.openai.com.
	Endpoint() string
}

// Ref is a swappable Backend holder shared by the agent and its tools. A live
// /model switch calls Set once and every component - main agent, subagents, the
// skill tool - redirects to the new backend on its next call. In-flight streams
// keep the backend they started on.
type Ref struct {
	mu       sync.RWMutex
	b        Backend
	onCall   []func(context.Context, Usage, UsageReport)
	onLimits []func(Limits)
}

// NewRef wraps an initial backend.
func NewRef(b Backend) *Ref { return &Ref{b: b} }

// Set swaps the active backend. Usage totals start over, because they belong to
// the backend that spent them; observers are re-registered so a model switch does
// not silently stop the reporting. b is assumed to be a freshly opened backend
// (what auth.OpenModel returns) - re-Setting one that was already active would
// register the observers on it twice.
func (r *Ref) Set(b Backend) {
	r.mu.Lock()
	r.b = b
	calls := slices.Clone(r.onCall)
	limits := slices.Clone(r.onLimits)
	r.mu.Unlock()
	u, ok := b.(UsageReporter)
	if !ok {
		return
	}
	for _, f := range calls {
		u.OnCallCtx(f)
	}
	for _, f := range limits {
		u.OnLimits(f)
	}
}

// UsageReport implements UsageReporter for the live backend, zero when that
// backend counts nothing.
func (r *Ref) UsageReport() UsageReport {
	if u, ok := r.Get().(UsageReporter); ok {
		return u.UsageReport()
	}
	return UsageReport{}
}

// OnCall implements UsageReporter, remembering the callback so it survives Set.
func (r *Ref) OnCall(f func(Usage, UsageReport)) {
	if f == nil {
		return
	}
	r.OnCallCtx(func(_ context.Context, u Usage, rep UsageReport) { f(u, rep) })
}

// OnCallCtx implements UsageReporter, remembering the callback so it survives Set.
func (r *Ref) OnCallCtx(f func(context.Context, Usage, UsageReport)) {
	if f == nil {
		return
	}
	r.mu.Lock()
	r.onCall = append(r.onCall, f)
	b := r.b
	r.mu.Unlock()
	if u, ok := b.(UsageReporter); ok {
		u.OnCallCtx(f)
	}
}

// OnLimits implements UsageReporter, remembering the callback so it survives Set.
func (r *Ref) OnLimits(f func(Limits)) {
	if f == nil {
		return
	}
	r.mu.Lock()
	r.onLimits = append(r.onLimits, f)
	b := r.b
	r.mu.Unlock()
	if u, ok := b.(UsageReporter); ok {
		u.OnLimits(f)
	}
}

// usable reports whether the live backend counts usage at all.
func (r *Ref) usable() bool {
	_, ok := r.Get().(UsageReporter)
	return ok
}

// Get returns the active backend.
func (r *Ref) Get() Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.b
}

func (r *Ref) Stream(ctx context.Context, messages []Message, tools []Tool, temperature float64,
	onEvent func(StreamEvent)) (Message, error) {
	return r.Get().Stream(ctx, messages, tools, temperature, onEvent)
}

func (r *Ref) Tokenize(ctx context.Context, text string) (int, error) {
	return r.Get().Tokenize(ctx, text)
}

func (r *Ref) Model() ModelInfo { return r.Get().Model() }

func (r *Ref) Endpoint() string { return r.Get().Endpoint() }
