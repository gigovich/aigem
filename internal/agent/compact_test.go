package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/tools"
)

func usr(s string) llm.Message  { return llm.Message{Role: llm.RoleUser, Content: s} }
func asst(s string) llm.Message { return llm.Message{Role: llm.RoleAssistant, Content: s} }

func validSummary(note string) string {
	return `<summary>
1. GOAL: ` + note + `
2. KEY DECISIONS: none.
3. FILES TOUCHED: none.
4. ERRORS & FIXES: none.
5. CURRENT STATE: verified in test.
6. TODO: none.
7. OPEN QUESTIONS: none.
8. NEXT STEP: continue.
</summary>`
}

func call(id, name, args string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
		{ID: id, Type: "function", Function: llm.FunctionCall{Name: name, Arguments: args}},
	}}
}

func toolRes(id, name, content string) llm.Message {
	return llm.Message{Role: llm.RoleTool, ToolCallID: id, Name: name, Content: content}
}

// sample is a representative session: system, goal, two reads of the same path,
// a list_dir, and a final answer.
func sample() []llm.Message {
	return []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		usr("original goal"),
		call("r1", "read_file", `{"path":"a.go"}`),
		toolRes("r1", "read_file", strings.Repeat("AAAA", 100)),
		call("r2", "read_file", `{"path":"a.go"}`),
		toolRes("r2", "read_file", strings.Repeat("BBBB", 100)),
		call("l1", "list_dir", `{"path":"."}`),
		toolRes("l1", "list_dir", strings.Repeat("CCCC", 100)),
		asst("done"),
	}
}

func TestSafeBoundaryNeverOrphansToolResult(t *testing.T) {
	msgs := sample()
	for _, keep := range []int{1, 2, 3, 10} {
		b := safeBoundary(msgs, keep)
		if b < 0 || b > len(msgs) {
			t.Fatalf("keep=%d: boundary %d out of range", keep, b)
		}
		if b < len(msgs) && msgs[b].Role == llm.RoleTool {
			t.Fatalf("keep=%d: tail starts on an orphan tool result", keep)
		}
	}
}

func TestSafeBoundaryKeepsRecentMessages(t *testing.T) {
	msgs := sample() // 9 messages
	// keep=2 lands on the final tool result, so it snaps back to the owning
	// list_dir tool_use (index 6), keeping the whole group in the tail.
	if b := safeBoundary(msgs, 2); b != 6 {
		t.Fatalf("keep=2 boundary = %d, want 6 (snapped off tool result)", b)
	}
	// keep=1 lands on the trailing assistant text, which needs no snapping.
	if b := safeBoundary(msgs, 1); b != 8 {
		t.Fatalf("keep=1 boundary = %d, want 8", b)
	}
	// keepTurns larger than the history keeps everything (boundary at 0).
	if b := safeBoundary(msgs, 100); b != 0 {
		t.Fatalf("oversized keep boundary = %d, want 0", b)
	}
}

func TestEvictPreservesStructureAndDedups(t *testing.T) {
	msgs := sample()
	out, freed := evictToolResults(msgs, 4, len(msgs)) // window covers all -> only stage-2 dedup
	if len(out) != len(msgs) {
		t.Fatalf("length changed: %d -> %d", len(msgs), len(out))
	}
	for i := range out {
		if out[i].Role != msgs[i].Role {
			t.Fatalf("role at %d changed: %s -> %s", i, msgs[i].Role, out[i].Role)
		}
	}
	if freed <= 0 {
		t.Fatal("expected freed > 0 from deduping the stale read")
	}
	// r1 (older read of a.go) is elided; r2 (latest) and l1 stay verbatim.
	if !strings.HasPrefix(out[3].Content, "[output elided") {
		t.Fatalf("stale read not elided: %q", out[3].Content)
	}
	if out[5].Content != msgs[5].Content {
		t.Fatal("latest read of the path was elided")
	}
	if out[7].Content != msgs[7].Content {
		t.Fatal("list_dir result was elided despite being unique and in-window")
	}
}

func TestEvictHonorsKeepWindow(t *testing.T) {
	msgs := sample()
	out, _ := evictToolResults(msgs, 1, len(msgs)) // keep only the most recent tool result
	if !strings.HasPrefix(out[3].Content, "[output elided") ||
		!strings.HasPrefix(out[5].Content, "[output elided") {
		t.Fatal("old tool results outside the window should be elided")
	}
	if out[7].Content != msgs[7].Content {
		t.Fatal("the most recent tool result must stay verbatim")
	}
}

func TestEvictIsIdempotent(t *testing.T) {
	msgs := sample()
	out1, _ := evictToolResults(msgs, 1, len(msgs))
	out2, freed := evictToolResults(out1, 1, len(msgs))
	if freed != 0 {
		t.Fatalf("second eviction freed %d, want 0 (idempotent)", freed)
	}
	for i := range out1 {
		if out1[i].Content != out2[i].Content {
			t.Fatalf("content at %d changed on re-eviction", i)
		}
	}
}

func TestEvictProtectsTail(t *testing.T) {
	msgs := sample()
	// Protect everything from index 3 on: the only evictable tool result is the
	// first read (index 3 is protected too here), so nothing should be elided.
	out, freed := evictToolResults(msgs, 1, 3)
	if freed != 0 {
		t.Fatalf("expected nothing freed with a protected tail, freed %d", freed)
	}
	for i := range out {
		if out[i].Content != msgs[i].Content {
			t.Fatalf("protected tail mutated at %d", i)
		}
	}
}

func newCompactAgent(t *testing.T, fc *fakeClient) *Agent {
	t.Helper()
	// Compaction writes a pre-compaction backup under the state directory, so a
	// test that does not point it somewhere of its own writes into the
	// developer's real ~/.local/state/aigem.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(fc, reg, 0.3, nil, "sys")
}

func TestCompactProducesSummaryAndValidRoles(t *testing.T) {
	ag := newCompactAgent(t, &fakeClient{final: validSummary("recap")})
	ag.SetMessages(sample()[1:]) // drop the sample system; New keeps its own "sys"
	ag.SetCompaction(CompactConfig{KeepTurns: 2})

	status, err := ag.Compact(context.Background(), "keep the API shape", Events{})
	if err != nil {
		t.Fatal(err)
	}
	if status == "" {
		t.Fatal("expected a status line")
	}
	msgs := ag.Messages()
	if msgs[0].Role != llm.RoleSystem {
		t.Fatalf("system[0] not preserved: %+v", msgs[0])
	}
	if msgs[1].Role != llm.RoleUser || msgs[1].Content != "original goal" {
		t.Fatalf("goal not preserved verbatim: %+v", msgs[1])
	}
	if !strings.Contains(msgs[2].Content, "GOAL: recap") {
		t.Fatalf("summary message missing: %+v", msgs[2])
	}
	// No RoleTool may appear without a preceding assistant tool_calls.
	for i, m := range msgs {
		if m.Role == llm.RoleTool && (i == 0 || len(msgs[i-1].ToolCalls) == 0) {
			t.Fatalf("orphan tool result at %d after compaction", i)
		}
	}
}

type summaryScriptClient struct {
	outputs []string
	calls   int
}

func (c *summaryScriptClient) Stream(_ context.Context, _ []llm.Message, _ []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	if c.calls >= len(c.outputs) {
		return llm.Message{Role: llm.RoleAssistant, Content: validSummary("fallback")}, nil
	}
	out := c.outputs[c.calls]
	c.calls++
	return llm.Message{Role: llm.RoleAssistant, Content: out}, nil
}

func TestCompactRetriesMalformedSummaryOnce(t *testing.T) {
	fc := &summaryScriptClient{outputs: []string{"not wrapped", validSummary("retry ok")}}
	ag := newCompactAgent(t, (*fakeClient)(nil))
	ag.client = fc
	ag.SetMessages(sample()[1:])
	ag.SetCompaction(CompactConfig{KeepTurns: 2})
	var notices []string
	status, err := ag.Compact(context.Background(), "", Events{OnNotice: func(s string) { notices = append(notices, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if status == "" || fc.calls != 2 {
		t.Fatalf("expected one retry and status, calls=%d status=%q", fc.calls, status)
	}
	foundRetryNotice := false
	for _, n := range notices {
		if strings.Contains(n, "malformed") && strings.Contains(n, "retrying once") {
			foundRetryNotice = true
		}
	}
	if !foundRetryNotice {
		t.Fatalf("retry was not observable in notices: %v", notices)
	}
	if !strings.Contains(ag.Messages()[2].Content, "GOAL: retry ok") {
		t.Fatalf("retry summary not installed: %+v", ag.Messages()[2])
	}
}

func TestCompactDoesNotReplaceContextAfterMalformedRetry(t *testing.T) {
	fc := &summaryScriptClient{outputs: []string{"not wrapped", "<summary>missing sections</summary>"}}
	ag := newCompactAgent(t, (*fakeClient)(nil))
	ag.client = fc
	ag.SetMessages(sample()[1:])
	ag.SetCompaction(CompactConfig{KeepTurns: 2})
	before := ag.Messages()
	if _, err := ag.Compact(context.Background(), "", Events{}); err == nil {
		t.Fatal("expected malformed summary error")
	}
	after := ag.Messages()
	if len(after) != len(before) {
		t.Fatalf("conversation was replaced despite malformed summary: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Role != after[i].Role || before[i].Content != after[i].Content {
			t.Fatalf("message %d changed despite malformed summary\nbefore=%+v\nafter=%+v", i, before[i], after[i])
		}
	}
}

func TestBackupMessagesPrivateMode(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if err := backupMessages("abc", 1, sample()); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(state, "aigem", "sessions", "abc.precompact-1.json")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("backup mode = %o, want 600", got)
	}
}

func TestBackupMessagesChmodsExistingBackup(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	p := filepath.Join(state, "aigem", "sessions", "abc.precompact-1.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := backupMessages("abc", 1, sample()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("overwritten backup mode = %o, want 600", got)
	}
}

func TestMaybeCompactTriggersByPressure(t *testing.T) {
	ag := newCompactAgent(t, &fakeClient{final: validSummary("x")})
	ag.SetMessages(sample()[1:])

	// Below threshold: no-op (no summary spliced in).
	ag.SetCompaction(CompactConfig{Auto: true, CtxSize: 1 << 20, CompactAtPct: 70, EvictAtPct: 50, KeepTurns: 10, KeepTools: 4})
	before := len(ag.Messages())
	ag.maybeCompact(context.Background(), Events{})
	if got := len(ag.Messages()); got != before {
		t.Fatalf("low-pressure maybeCompact changed history: %d -> %d", before, got)
	}

	// Tiny window forces high pressure -> summarization runs.
	ag.SetCompaction(CompactConfig{Auto: true, CtxSize: 10, CompactAtPct: 70, EvictAtPct: 50, KeepTurns: 2, KeepTools: 4})
	ag.maybeCompact(context.Background(), Events{})
	found := false
	for _, m := range ag.Messages() {
		if strings.Contains(m.Content, "GOAL: x") {
			found = true
		}
	}
	if !found {
		t.Fatal("high-pressure maybeCompact did not summarize")
	}
}

func TestFitContextEvictsOversizedTail(t *testing.T) {
	ag := newCompactAgent(t, &fakeClient{})
	ag.SetMessages([]llm.Message{
		usr("goal"),
		call("a", "grep", `{"pattern":"x"}`),
		toolRes("a", "grep", strings.Repeat("O", 400000)), // old, evictable
		call("b", "grep", `{"pattern":"y"}`),
		toolRes("b", "grep", strings.Repeat("N", 4000)), // recent, must survive
	})
	const ctxSize = 50000
	ag.SetCompaction(CompactConfig{CtxSize: ctxSize})

	ag.fitContext(context.Background(), Events{})

	msgs := ag.Messages()
	if !strings.HasPrefix(msgs[3].Content, "[output elided") {
		t.Fatalf("old oversized tool result not evicted: %q", msgs[3].Content[:40])
	}
	if msgs[5].Content != strings.Repeat("N", 4000) {
		t.Fatal("the most recent tool result must be kept verbatim")
	}
	if got, budget := ag.accurateTokens(context.Background()), ctxSize*fitContextPct/100; got > budget {
		t.Fatalf("still over budget after fit: %d > %d", got, budget)
	}
}

func TestFitContextTruncatesAsLastResort(t *testing.T) {
	ag := newCompactAgent(t, &fakeClient{})
	// A single giant non-tool message cannot be evicted; it must be truncated.
	ag.SetMessages([]llm.Message{usr(strings.Repeat("Z", 400000))})
	const ctxSize = 50000
	ag.SetCompaction(CompactConfig{CtxSize: ctxSize})

	ag.fitContext(context.Background(), Events{})

	msgs := ag.Messages()
	if msgs[0].Role != llm.RoleSystem || msgs[0].Content != "sys" {
		t.Fatalf("system prompt must never be truncated: %+v", msgs[0])
	}
	if !strings.Contains(msgs[1].Content, "[truncated to fit context]") {
		t.Fatal("oversized message was not truncated")
	}
	if got, budget := ag.accurateTokens(context.Background()), ctxSize*fitContextPct/100; got > budget {
		t.Fatalf("still over budget after truncation: %d > %d", got, budget)
	}
}

func TestFitContextNoopWithoutCtxSize(t *testing.T) {
	ag := newCompactAgent(t, &fakeClient{})
	ag.SetMessages([]llm.Message{usr(strings.Repeat("Z", 400000))})
	ag.SetCompaction(CompactConfig{CtxSize: 0}) // unconfigured (e.g. subagent)

	before := ag.Messages()
	ag.fitContext(context.Background(), Events{})
	if after := ag.Messages(); after[1].Content != before[1].Content {
		t.Fatal("fitContext must be a no-op when no context size is configured")
	}
}

func TestMaybeCompactSkipsSubagents(t *testing.T) {
	ag := newCompactAgent(t, &fakeClient{final: validSummary("x")})
	ag.subagentType = "scout"
	ag.SetMessages(sample()[1:])
	ag.SetCompaction(CompactConfig{Auto: true, CtxSize: 10, CompactAtPct: 70, EvictAtPct: 50, KeepTurns: 2, KeepTools: 4})
	before := len(ag.Messages())
	ag.maybeCompact(context.Background(), Events{})
	if got := len(ag.Messages()); got != before {
		t.Fatalf("subagent should not auto-compact: %d -> %d", before, got)
	}
}
