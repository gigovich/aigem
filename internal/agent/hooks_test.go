package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/hooks"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/tools"
)

// echoClient returns a final answer immediately (no tool calls), so a turn ends
// after one model call unless a Stop hook forces more work.
type echoClient struct{ calls int }

func (e *echoClient) Stream(_ context.Context, _ []llm.Message, _ []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	e.calls++
	return llm.Message{Role: llm.RoleAssistant, Content: "done"}, nil
}

// loadHooks drops a .aigem/settings.json under dir, loads it, and trusts the
// project (project-local hooks are gated on trust). The trust store is isolated
// to a temp XDG_STATE_HOME so tests do not touch the user's state.
func loadHooks(t *testing.T, dir, cfg string) *hooks.Runner {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	path := filepath.Join(dir, ".aigem", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	r, warns := hooks.Load(dir)
	if len(warns) != 0 {
		t.Fatalf("unexpected hook warnings: %v", warns)
	}
	if r.HasUntrustedProjectHooks() {
		if err := r.TrustProject(); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func toolMessages(ag *Agent) string {
	var b strings.Builder
	for _, m := range ag.Messages() {
		if m.Role == llm.RoleTool {
			b.WriteString(m.Content)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func TestPreToolUseBlocks(t *testing.T) {
	dir := t.TempDir()
	reg, err := tools.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	runner := loadHooks(t, dir, `{"hooks":{"PreToolUse":[{"matcher":"list_dir",`+
		`"hooks":[{"type":"command","command":"echo nope >&2; exit 2"}]}]}}`)

	fc := &fakeClient{toolName: "list_dir", args: `{"path":"."}`, final: "done"}
	ag := New(fc, reg, 0.3, nil, "")
	ag.SetHooks(runner)

	if _, err := ag.Run(context.Background(), "list please", Events{}); err != nil {
		t.Fatal(err)
	}
	if got := toolMessages(ag); !strings.Contains(got, "blocked by a PreToolUse hook: nope") {
		t.Fatalf("expected blocked tool result, got: %q", got)
	}
}

func TestPreToolUseUpdatesInput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The model asks to read missing.txt; the hook rewrites the path to real.txt.
	runner := loadHooks(t, dir, `{"hooks":{"PreToolUse":[{"matcher":"read_file","hooks":[{"type":"command",`+
		`"command":"echo '{\"hookSpecificOutput\":{\"updatedInput\":{\"path\":\"real.txt\"}}}'"}]}]}}`)

	fc := &fakeClient{toolName: "read_file", args: `{"path":"missing.txt"}`, final: "done"}
	ag := New(fc, reg, 0.3, nil, "")
	ag.SetHooks(runner)

	if _, err := ag.Run(context.Background(), "read it", Events{}); err != nil {
		t.Fatal(err)
	}
	if got := toolMessages(ag); !strings.Contains(got, "hello") {
		t.Fatalf("expected rewritten read to return file contents, got: %q", got)
	}
}

func TestUserPromptSubmitAddsContext(t *testing.T) {
	dir := t.TempDir()
	reg, err := tools.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	runner := loadHooks(t, dir, `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command",`+
		`"command":"echo '{\"hookSpecificOutput\":{\"additionalContext\":\"INJECTED\"}}'"}]}]}}`)

	ag := New(&echoClient{}, reg, 0.3, nil, "")
	ag.SetHooks(runner)
	if _, err := ag.Run(context.Background(), "hi", Events{}); err != nil {
		t.Fatal(err)
	}
	msgs := ag.Messages()
	if len(msgs) < 2 || msgs[1].Role != llm.RoleUser {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
	if !strings.Contains(msgs[1].Content, "INJECTED") {
		t.Fatalf("UserPromptSubmit context not injected: %q", msgs[1].Content)
	}
}

func TestUserPromptSubmitBlocks(t *testing.T) {
	dir := t.TempDir()
	reg, err := tools.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	runner := loadHooks(t, dir, `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command",`+
		`"command":"echo blocked-prompt >&2; exit 2"}]}]}}`)

	ec := &echoClient{}
	ag := New(ec, reg, 0.3, nil, "")
	ag.SetHooks(runner)
	answer, err := ag.Run(context.Background(), "hi", Events{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, "blocked-prompt") {
		t.Fatalf("expected block reason as answer, got: %q", answer)
	}
	if ec.calls != 0 {
		t.Fatalf("model should not be called when the prompt is blocked, got %d", ec.calls)
	}
}

func TestStopHookCapped(t *testing.T) {
	dir := t.TempDir()
	reg, err := tools.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A Stop hook that always blocks must still terminate after maxStopBlocks.
	runner := loadHooks(t, dir, `{"hooks":{"Stop":[{"hooks":[{"type":"command",`+
		`"command":"echo '{\"decision\":\"block\",\"reason\":\"more\"}'"}]}]}}`)
	ec := &echoClient{}
	ag := New(ec, reg, 0.3, nil, "")
	ag.SetHooks(runner)
	if _, err := ag.Run(context.Background(), "hi", Events{}); err != nil {
		t.Fatal(err)
	}
	if ec.calls != 1+maxStopBlocks {
		t.Fatalf("expected %d calls (initial + cap), got %d", 1+maxStopBlocks, ec.calls)
	}
}

func TestPostToolUseReplacesOutput(t *testing.T) {
	dir := t.TempDir()
	reg, err := tools.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	runner := loadHooks(t, dir, `{"hooks":{"PostToolUse":[{"matcher":"list_dir","hooks":[{"type":"command",`+
		`"command":"echo '{\"hookSpecificOutput\":{\"updatedToolOutput\":\"REPLACED\"}}'"}]}]}}`)
	fc := &fakeClient{toolName: "list_dir", args: `{"path":"."}`, final: "done"}
	ag := New(fc, reg, 0.3, nil, "")
	ag.SetHooks(runner)
	if _, err := ag.Run(context.Background(), "list", Events{}); err != nil {
		t.Fatal(err)
	}
	if got := toolMessages(ag); !strings.Contains(got, "REPLACED") {
		t.Fatalf("expected PostToolUse to replace output, got: %q", got)
	}
}

func TestPreToolUseAllowSkipsConfirm(t *testing.T) {
	dir := t.TempDir()
	reg, err := tools.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	runner := loadHooks(t, dir, `{"hooks":{"PreToolUse":[{"matcher":"bash","hooks":[{"type":"command",`+
		`"command":"echo '{\"hookSpecificOutput\":{\"permissionDecision\":\"allow\"}}'"}]}]}}`)
	confirmCalled := false
	confirm := func(string, json.RawMessage) bool { confirmCalled = true; return false }
	fc := &fakeClient{toolName: "bash", args: `{"cmd":"echo hi"}`, final: "done"}
	ag := New(fc, reg, 0.3, confirm, "")
	ag.SetHooks(runner)
	if _, err := ag.Run(context.Background(), "run", Events{}); err != nil {
		t.Fatal(err)
	}
	if confirmCalled {
		t.Fatal("PreToolUse allow should skip the confirmation prompt")
	}
	if got := toolMessages(ag); !strings.Contains(got, "hi") {
		t.Fatalf("expected bash to have run, got: %q", got)
	}
}

func TestPreToolUseAskForcesConfirm(t *testing.T) {
	dir := t.TempDir()
	reg, err := tools.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	// list_dir does not normally need confirmation; "ask" must force the prompt.
	runner := loadHooks(t, dir, `{"hooks":{"PreToolUse":[{"matcher":"list_dir","hooks":[{"type":"command",`+
		`"command":"echo '{\"hookSpecificOutput\":{\"permissionDecision\":\"ask\"}}'"}]}]}}`)
	confirmCalled := false
	confirm := func(string, json.RawMessage) bool { confirmCalled = true; return true }
	fc := &fakeClient{toolName: "list_dir", args: `{"path":"."}`, final: "done"}
	ag := New(fc, reg, 0.3, confirm, "")
	ag.SetHooks(runner)
	if _, err := ag.Run(context.Background(), "list", Events{}); err != nil {
		t.Fatal(err)
	}
	if !confirmCalled {
		t.Fatal("PreToolUse ask should force a confirmation prompt")
	}
}

func TestSkillHooksAreAgentScoped(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a1 := New(&echoClient{}, reg, 0.3, nil, "")
	a2 := New(&echoClient{}, reg, 0.3, nil, "")
	cfg := map[string][]hooks.Matcher{
		"PreToolUse": {{Matcher: "*", Hooks: []hooks.Hook{{Type: "command", Command: "true"}}}},
	}
	agentActivation{a1}.AddHooks(cfg)
	if len(a1.skillHooks["PreToolUse"]) != 1 {
		t.Fatalf("activating agent should hold the skill hook, got %d", len(a1.skillHooks["PreToolUse"]))
	}
	if len(a2.skillHooks["PreToolUse"]) != 0 {
		t.Fatal("a sibling agent must not see another agent's skill hooks")
	}
}

func TestSubagentSeededHooksSurviveReset(t *testing.T) {
	dir := t.TempDir()
	reg, err := tools.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Empty base settings; a forked skill seeds its own hooks onto the subagent.
	runner := loadHooks(t, dir, `{"hooks":{}}`)
	fc := &fakeClient{toolName: "list_dir", args: `{"path":"."}`, final: "done"}
	sub := New(fc, reg, 0.3, nil, "")
	sub.SetHooks(runner)
	sub.subagentType = "myskill"
	sub.skillHooks = map[string][]hooks.Matcher{
		"PreToolUse": {{Matcher: "list_dir", Hooks: []hooks.Hook{
			{Type: "command", Command: "echo blocked-by-skill >&2; exit 2"},
		}}},
	}
	if _, err := sub.Run(context.Background(), "do it", Events{}); err != nil {
		t.Fatal(err)
	}
	if got := toolMessages(sub); !strings.Contains(got, "blocked-by-skill") {
		t.Fatalf("a subagent's seeded skill hook should fire, got: %q", got)
	}
}

func TestStopHookForcesMoreWork(t *testing.T) {
	dir := t.TempDir()
	reg, err := tools.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Block on the first Stop (no flag yet), allow once the flag exists.
	runner := loadHooks(t, dir, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":`+
		`"f=\"$CLAUDE_PROJECT_DIR/.stop\"; if [ -f \"$f\" ]; then exit 0; else touch \"$f\"; `+
		`echo '{\"decision\":\"block\",\"reason\":\"keep going\"}'; fi"}]}]}}`)

	ec := &echoClient{}
	ag := New(ec, reg, 0.3, nil, "")
	ag.SetHooks(runner)
	if _, err := ag.Run(context.Background(), "hi", Events{}); err != nil {
		t.Fatal(err)
	}
	if ec.calls < 2 {
		t.Fatalf("Stop block should have forced a second model call, got %d", ec.calls)
	}
	var injected bool
	for _, m := range ag.Messages() {
		if m.Role == llm.RoleUser && strings.Contains(m.Content, "keep going") {
			injected = true
		}
	}
	if !injected {
		t.Fatal("expected the Stop reason to be injected as a user message")
	}
}
