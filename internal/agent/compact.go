package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/hooks"
	"github.com/gigovich/aigem/internal/llm"
)

// CompactConfig controls automatic context compaction. The zero value disables
// auto-compaction (Auto is false); manual Compact still works regardless.
type CompactConfig struct {
	Auto         bool // enable auto-compaction in Run
	CtxSize      int  // model context window in tokens (pressure denominator)
	CompactAtPct int  // summarization trigger (stage 3)
	EvictAtPct   int  // tool-result eviction trigger (stage 1+2)
	KeepTurns    int  // recent user turns kept verbatim across summarization
	KeepTools    int  // recent tool results kept verbatim during eviction
}

// DefaultCompactConfig returns the built-in policy used when no flags override
// it: aggressive thresholds suited to a slow local model.
func DefaultCompactConfig() CompactConfig {
	return CompactConfig{
		Auto:         true,
		CompactAtPct: 70,
		EvictAtPct:   50,
		KeepTurns:    10,
		KeepTools:    4,
	}
}

// elidedToolResult replaces an evicted tool output. The tool_use stays intact,
// so the model still knows the call happened and can re-run it.
func elidedToolResult(name string) string {
	if name == "" {
		return "[output elided to save context - re-run the tool to retrieve it]"
	}
	return fmt.Sprintf("[output elided to save context - re-run %s to retrieve it]", name)
}

// firstUserIndex returns the index of the original goal (first RoleUser message),
// or -1 if there is none.
func firstUserIndex(msgs []llm.Message) int {
	for i, m := range msgs {
		if m.Role == llm.RoleUser {
			return i
		}
	}
	return -1
}

// safeBoundary returns the index where the verbatim tail starts: keep the last
// keepTurns messages, then pull the boundary back off any RoleTool message so a
// tool result is never stranded from the assistant tool_use that produced it
// (invariants 3 and 4). Counting messages (not user turns) keeps a long
// single-turn session - one prompt, many tool rounds - compactable too.
func safeBoundary(msgs []llm.Message, keepTurns int) int {
	if keepTurns < 1 {
		keepTurns = 1
	}
	b := len(msgs) - keepTurns
	if b < 0 {
		b = 0
	}
	for b > 0 && msgs[b].Role == llm.RoleTool {
		b--
	}
	return b
}

// readPaths maps a tool_call ID to the "path" argument of a read_file call, by
// scanning assistant tool_calls. Used by stage-2 dedup.
func readPaths(msgs []llm.Message) map[string]string {
	out := map[string]string{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.Function.Name != "read_file" {
				continue
			}
			var a struct {
				Path string `json:"path"`
			}
			if json.Unmarshal([]byte(tc.Function.Arguments), &a) == nil && a.Path != "" {
				out[tc.ID] = a.Path
			}
		}
	}
	return out
}

// evictToolResults performs stage 1 (drop tool outputs older than the keepTools
// window) and stage 2 (drop every read_file output but the latest read of each
// path). Results at index >= protectFrom are kept verbatim, so the recent
// verbatim tail is never touched. It returns a new slice and an estimate of the
// characters freed; message structure is preserved (only RoleTool.Content
// changes), so every invariant holds. Already-elided results are left untouched.
func evictToolResults(msgs []llm.Message, keepTools, protectFrom int) ([]llm.Message, int) {
	if keepTools < 0 {
		keepTools = 0
	}
	var toolIdx []int
	for i, m := range msgs {
		if m.Role == llm.RoleTool {
			toolIdx = append(toolIdx, i)
		}
	}
	keepFrom := len(toolIdx) - keepTools

	paths := readPaths(msgs)
	// Latest tool-result index per read_file path (the read to keep).
	latestRead := map[string]int{}
	for _, i := range toolIdx {
		if p := paths[msgs[i].ToolCallID]; p != "" {
			latestRead[p] = i
		}
	}

	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	freed := 0
	for pos, i := range toolIdx {
		if i >= protectFrom {
			continue
		}
		p := paths[out[i].ToolCallID]
		staleRead := p != "" && latestRead[p] != i
		oldResult := pos < keepFrom
		if !staleRead && !oldResult {
			continue
		}
		elided := elidedToolResult(out[i].Name)
		if out[i].Content == elided {
			continue
		}
		freed += len(out[i].Content) - len(elided)
		out[i].Content = elided
	}
	if freed < 0 {
		freed = 0
	}
	return out, freed
}

// summaryPrompt is the instruction handed to the model for stage-3 summarization.
const summaryPrompt = `You are compacting a coding-agent session so work continues in a fresh context
where the raw history above is discarded and replaced by your summary. Maximize
recall: capture every fact needed to continue without the original. Quote key
phrases, identifiers, and paths directly. Do NOT call tools; output text only.

Produce exactly these sections, wrapped in <summary></summary>:
1. GOAL: the user's original request and refinements, quoted where possible.
2. KEY DECISIONS: design/architecture choices and the reason for each.
3. FILES TOUCHED: each path read/edited, its state (created/edited/pending),
   key symbols.
4. ERRORS & FIXES: each error and how it was resolved, or that it is still open.
5. CURRENT STATE: what works now, verified vs unverified.
6. TODO: outstanding tasks as a priority-ordered checklist.
7. OPEN QUESTIONS: unresolved decisions / needs user input.
8. NEXT STEP: the single concrete next action.`

// summarize runs stage 3: it renders toSummarize (a contiguous slice of the live
// history) into a plain-text transcript and asks the model to summarize it, with
// tools disabled so a local model does not try to act. The history is flattened
// rather than replayed as structured messages because many llama.cpp builds
// reject assistant tool_calls / role:tool messages when the request declares no
// tools - exactly the long, tool-heavy sessions compaction targets. The returned
// message is a RoleUser carrying the <summary> block, ready to insert after the
// goal.
func summarize(ctx context.Context, client streamer, temp float64, toSummarize []llm.Message,
	instructions string, ev Events) (llm.Message, error) {
	sys := summaryPrompt
	if instructions = strings.TrimSpace(instructions); instructions != "" {
		sys += "\n\nUser-requested preservation: " + instructions
	}
	req := []llm.Message{
		{Role: llm.RoleSystem, Content: sys},
		{Role: llm.RoleUser, Content: "Here is the session transcript to compact:\n\n" +
			renderTranscript(toSummarize) +
			"\n\nProduce the <summary> now, following the section structure exactly."},
	}
	body, err := requestSummary(ctx, client, temp, req)
	if err == nil {
		return llm.Message{Role: llm.RoleUser, Content: "[conversation summary]\n" + body}, nil
	}
	notice(ev, "compaction summary malformed - retrying once: "+err.Error())
	req = append(req, llm.Message{Role: llm.RoleUser, Content: "The previous compaction summary was malformed: " + err.Error() + "\n\nRetry once. Output exactly one <summary>...</summary> block with all required section headings and no text outside the tags."})
	body, err = requestSummary(ctx, client, temp, req)
	if err != nil {
		return llm.Message{}, fmt.Errorf("compaction: malformed summary after retry: %w", err)
	}
	return llm.Message{Role: llm.RoleUser, Content: "[conversation summary]\n" + body}, nil
}

func requestSummary(ctx context.Context, client streamer, temp float64, req []llm.Message) (string, error) {
	msg, err := client.Stream(ctx, req, nil, temp, func(llm.StreamEvent) {})
	if err != nil {
		return "", err
	}
	body := strings.TrimSpace(msg.Content)
	if err := validateSummary(body); err != nil {
		return "", err
	}
	return body, nil
}

var requiredSummarySections = []string{
	"GOAL:", "KEY DECISIONS:", "FILES TOUCHED:", "ERRORS & FIXES:",
	"CURRENT STATE:", "TODO:", "OPEN QUESTIONS:", "NEXT STEP:",
}

func validateSummary(body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return fmt.Errorf("model returned an empty summary")
	}
	if !strings.HasPrefix(body, "<summary>") || !strings.HasSuffix(body, "</summary>") {
		return fmt.Errorf("summary must be wrapped in exactly one <summary>...</summary> block")
	}
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(body, "<summary>"), "</summary>"))
	if inner == "" {
		return fmt.Errorf("summary block is empty")
	}
	if strings.Contains(inner, "<summary>") || strings.Contains(inner, "</summary>") {
		return fmt.Errorf("summary must contain exactly one <summary> block")
	}
	for _, section := range requiredSummarySections {
		if !strings.Contains(inner, section) {
			return fmt.Errorf("summary missing required section %q", section)
		}
	}
	return nil
}

// renderTranscript flattens a message slice into a readable plain-text transcript
// (role-tagged turns, with tool calls and their results inlined) so it can be fed
// to the summarizer as a single user message.
func renderTranscript(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleUser:
			content := m.Content
			if len(m.Images) > 0 {
				content += fmt.Sprintf("\n[attached images: %d]", len(m.Images))
			}
			fmt.Fprintf(&b, "USER:\n%s\n\n", content)
		case llm.RoleAssistant:
			if c := strings.TrimSpace(m.Content); c != "" {
				fmt.Fprintf(&b, "ASSISTANT:\n%s\n\n", c)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "ASSISTANT called %s(%s)\n\n", tc.Function.Name, tc.Function.Arguments)
			}
		case llm.RoleTool:
			fmt.Fprintf(&b, "TOOL RESULT (%s):\n%s\n\n", m.Name, m.Content)
		case llm.RoleSystem:
			fmt.Fprintf(&b, "SYSTEM:\n%s\n\n", m.Content)
		}
	}
	return strings.TrimSpace(b.String())
}

// backupMessages writes the pre-compaction history to
// <state>/sessions/<id>.precompact-<n>.json so detail stays recoverable. A
// missing session id falls back to "session". Failures are returned, not fatal.
func backupMessages(sessionID string, n int, msgs []llm.Message) error {
	base, err := config.StateDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(base, "sessions")
	// 0700, matching internal/session, which creates this same directory:
	// whichever runs first sets the mode, so disagreeing would make the
	// permissions depend on the order of two unrelated code paths.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Guard against a crafted session id escaping the backups directory: only the
	// final path element is used, and any separators leave it as "session".
	if sessionID = filepath.Base(sessionID); sessionID == "" || sessionID == "." ||
		sessionID == string(filepath.Separator) {
		sessionID = "session"
	}
	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%s.precompact-%d.json", sessionID, n)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// SetCompaction installs the auto-compaction policy. Call before Run.
func (a *Agent) SetCompaction(cfg CompactConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.compact = cfg
}

// SetSessionID records the session id used to name pre-compaction backups.
func (a *Agent) SetSessionID(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionID = id
}

// tokenize returns an accurate token count for text, using the llama-server
// tokenizer when the client supports it and caching per-text counts (message
// content is immutable). It falls back to chars/4 when no tokenizer is wired.
func (a *Agent) tokenize(ctx context.Context, text string) int {
	if text == "" {
		return 0
	}
	tok, ok := a.client.(tokenizer)
	if !ok {
		return len(text) / 4
	}
	a.mu.Lock()
	if a.tokCache == nil {
		a.tokCache = map[string]int{}
	}
	if n, hit := a.tokCache[text]; hit {
		a.mu.Unlock()
		return n
	}
	a.mu.Unlock()
	n, err := tok.Tokenize(ctx, text)
	if err != nil {
		return len(text) / 4
	}
	a.mu.Lock()
	// Bound the cache: a very long, repeatedly-compacted session would otherwise
	// retain token counts for messages no longer in context. Clearing wholesale
	// is fine - the live messages are re-tokenized cheaply on the next pass.
	if len(a.tokCache) >= maxTokCacheEntries {
		a.tokCache = map[string]int{}
	}
	a.tokCache[text] = n
	a.mu.Unlock()
	return n
}

// maxTokCacheEntries bounds the per-message token-count cache.
const maxTokCacheEntries = 4096

// tokenizer is the optional accurate-counting capability of the LLM client.
type tokenizer interface {
	Tokenize(ctx context.Context, text string) (int, error)
}

// ImageTokenEstimate is the flat per-image token cost used everywhere the
// conversation size is estimated. Vision models bill an image at roughly this
// many tokens regardless of file size; counting its base64 payload as text
// (megabytes -> "millions of tokens") would put any thread with a photo
// permanently over budget and shred its history through eviction.
const ImageTokenEstimate = 1600

// accurateTokens sums the token count of the whole conversation at the decision
// point, where precision matters more than the live gauge's chars/4 estimate.
func (a *Agent) accurateTokens(ctx context.Context) int {
	total := 0
	for _, m := range a.messages {
		total += a.tokenize(ctx, messageText(m))
		total += len(m.Images) * ImageTokenEstimate
	}
	return total
}

// messageText flattens a message to the text that contributes to its token
// count: content, name, and any tool-call name+arguments. Images are counted
// separately at ImageTokenEstimate each, never by their base64 size.
func messageText(m llm.Message) string {
	var b strings.Builder
	b.WriteString(m.Content)
	b.WriteString(m.Name)
	for _, tc := range m.ToolCalls {
		b.WriteString(tc.Function.Name)
		b.WriteString(tc.Function.Arguments)
	}
	return b.String()
}

// maybeCompact runs the auto-compaction cascade at a clean turn boundary. It is
// a no-op when auto-compaction is off, on a subagent, or below the eviction
// threshold. Eviction (stage 1+2) runs at EvictAtPct; summarization (stage 3)
// runs at CompactAtPct.
func (a *Agent) maybeCompact(ctx context.Context, ev Events) {
	a.mu.Lock()
	cfg := a.compact
	a.mu.Unlock()
	if !cfg.Auto || a.subagentType != "" || cfg.CtxSize <= 0 {
		return
	}
	pct := a.accurateTokens(ctx) * 100 / cfg.CtxSize
	switch {
	case cfg.CompactAtPct > 0 && pct >= cfg.CompactAtPct:
		if _, err := a.compactNow(ctx, "", "auto", ev); err != nil {
			notice(ev, "auto-compaction failed: "+err.Error())
		}
	case cfg.EvictAtPct > 0 && pct >= cfg.EvictAtPct:
		// Protect the recent verbatim tail: only evict results before the boundary.
		boundary := safeBoundary(a.messages, cfg.KeepTurns)
		newMsgs, freed := evictToolResults(a.messages, cfg.KeepTools, boundary)
		if freed > 0 {
			a.messages = newMsgs
			notice(ev, fmt.Sprintf("compacting context: evicted ~%d tokens of old tool output", freed/4))
			a.reportUsage(ev)
		}
	}
}

// fitContextPct caps the share of the context window the conversation may
// occupy right before a model call; the remainder is headroom for the reply.
const fitContextPct = 85

// fitContext is the hard backstop that guarantees the next request fits the
// model context window. maybeCompact's percentage cascade protects a recent
// verbatim tail, so a single oversized tool result landing in that tail can
// still push the request past the window. This runs immediately before every
// model call and will evict, then as a last resort truncate, until the
// conversation fits. It is a no-op when no context size is configured (e.g. a
// subagent), matching maybeCompact.
func (a *Agent) fitContext(ctx context.Context, ev Events) {
	a.mu.Lock()
	ctxSize := a.compact.CtxSize
	a.mu.Unlock()
	if ctxSize <= 0 {
		return
	}
	budget := ctxSize * fitContextPct / 100
	if a.accurateTokens(ctx) <= budget {
		return
	}
	// Stage 1: evict every tool result but the most recent, across the whole
	// history - ignoring the keep-recent window maybeCompact would protect.
	a.messages, _ = evictToolResults(a.messages, 1, len(a.messages))
	if a.accurateTokens(ctx) <= budget {
		notice(ev, "trimmed old tool output to fit the context window")
		a.reportUsage(ev)
		return
	}
	// Stage 2: still over - hard-truncate the largest message contents until the
	// conversation fits. Lossy, but it prevents a guaranteed server rejection.
	if a.truncateLargest(ctx, budget) {
		notice(ev, "truncated oversized content to fit the context window")
		a.reportUsage(ev)
	}
}

// truncateLargest repeatedly halves the largest message Content (never the
// system prompt at index 0) until the conversation fits budget tokens or no
// message is large enough to be worth cutting. It returns whether it changed
// anything.
func (a *Agent) truncateLargest(ctx context.Context, budget int) bool {
	const marker = "\n... [truncated to fit context]"
	changed := false
	for a.accurateTokens(ctx) > budget {
		idx, max := -1, 0
		for i := 1; i < len(a.messages); i++ {
			if n := len(a.messages[i].Content); n > max {
				idx, max = i, n
			}
		}
		if idx < 0 || max < 256 {
			break
		}
		a.messages[idx].Content = strings.ToValidUTF8(a.messages[idx].Content[:max/2], "") + marker
		changed = true
	}
	return changed
}

// Compact is the manual entry point for the /compact command: it always
// summarizes the closed prefix, optionally honoring user instructions, and
// returns a short status line. Auto-compaction uses compactNow directly.
func (a *Agent) Compact(ctx context.Context, instructions string, ev Events) (string, error) {
	return a.compactNow(ctx, instructions, "manual", ev)
}

// compactNow performs stage 3: back up the history, fire PreCompact, summarize
// the prefix between the goal and the verbatim tail, and splice the summary in
// after the goal. It preserves system[0] and the original goal verbatim.
func (a *Agent) compactNow(ctx context.Context, instructions, trigger string, ev Events) (string, error) {
	a.mu.Lock()
	cfg := a.compact
	sessionID := a.sessionID
	a.compactSeq++
	seq := a.compactSeq
	a.mu.Unlock()

	keepTurns := cfg.KeepTurns
	if keepTurns < 1 {
		keepTurns = DefaultCompactConfig().KeepTurns
	}
	goal := firstUserIndex(a.messages)
	if goal < 0 {
		return "nothing to compact", nil
	}
	boundary := safeBoundary(a.messages, keepTurns)
	if boundary <= goal+1 {
		return "nothing to compact yet", nil
	}
	toSummarize := a.messages[goal+1 : boundary]

	if err := backupMessages(sessionID, seq, a.Messages()); err != nil {
		notice(ev, "could not back up history before compaction: "+err.Error())
	}
	a.fireHooks(ctx, hooks.EventPreCompact, hooks.Input{ToolName: trigger, Source: trigger}, ev)

	notice(ev, "compacting context...")
	summary, err := summarize(ctx, a.client, a.temp, toSummarize, instructions, ev)
	if err != nil {
		return "", err
	}

	before := a.ContextTokens()
	newMsgs := make([]llm.Message, 0, 2+1+len(a.messages)-boundary)
	newMsgs = append(newMsgs, a.messages[0], a.messages[goal])
	newMsgs = append(newMsgs, summary)
	newMsgs = append(newMsgs, a.messages[boundary:]...)
	a.messages = newMsgs

	a.reportUsage(ev)
	dropped := before - a.ContextTokens()
	if dropped < 0 {
		dropped = 0
	}
	status := fmt.Sprintf("compacted context: summarized %d messages, freed ~%d tokens",
		len(toSummarize), dropped)
	notice(ev, status)
	return status, nil
}
