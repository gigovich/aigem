package uisession

import (
	"errors"
	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/llm"
)

// SwitchModel points the session at another model. persist records it as the
// default for later launches; restoring a saved session does not, since
// resuming an old conversation should not redefine what a new one starts on.
//
// The backend is swapped inside the shared Ref rather than replaced, so
// everything already holding it - the retrying wrapper, the delegation tool,
// the subagents - keeps working against the new model without being rebuilt.
func (l *Local) SwitchModel(ref string, persist bool) (llm.ModelInfo, error) {
	l.mu.Lock()
	models, backend, maxTokens := l.models, l.backend, l.maxTokens
	l.mu.Unlock()

	// Refused rather than dereferenced: a session built without a registry has
	// no way to resolve a reference, and a nil-pointer panic here would take
	// every other conversation in the process with it.
	if models == nil {
		return llm.ModelInfo{}, errors.New("this session was built without a model registry")
	}
	b, _, info, err := auth.OpenModel(models, ref, maxTokens)
	if err != nil {
		return llm.ModelInfo{}, err
	}
	backend.Set(b)
	if persist {
		// Best-effort: a read-only config directory must not interrupt a switch
		// that has already taken effect.
		_ = config.SaveModelPref(info.Ref())
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	// A model that declares no context window falls back to the startup default,
	// and never silently keeps the window of the model being replaced.
	cw := info.ContextWindow
	if cw <= 0 {
		cw = l.defaultCtx
	}
	l.ctxSize = cw
	l.compact.CtxSize = cw
	if l.ag != nil {
		l.ag.SetCompaction(l.compact)
		l.emitLocked(Event{Kind: KindUsage, Tokens: l.ag.ContextTokens()})
	}
	l.emitLocked(l.metaEventLocked())
	return info, nil
}

// CtxSize is the active model's context window, in tokens.
func (l *Local) CtxSize() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ctxSize
}

// CompactConfig is the compaction policy in force, including the context window
// the thresholds are measured against.
func (l *Local) CompactConfig() agent.CompactConfig {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.compact
}

// MaxTokens is the per-response output cap the session opens models with.
func (l *Local) MaxTokens() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.maxTokens
}

// ContextTokens is the estimated size of the conversation currently in context.
func (l *Local) ContextTokens() int {
	l.mu.Lock()
	ag := l.ag
	l.mu.Unlock()
	if ag == nil {
		return 0
	}
	return ag.ContextTokens()
}
