package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/tools"
)

// barrierTool blocks until `n` concurrent calls have arrived, proving the agent
// runs a batch of tool calls in parallel rather than sequentially. It echoes its
// "n" argument so result ordering can be checked.
type barrierTool struct {
	arrived chan string
	release chan struct{}
}

func (b *barrierTool) Name() string            { return "barrier" }
func (b *barrierTool) Description() string     { return "test barrier" }
func (b *barrierTool) NeedsConfirm() bool      { return false }
func (b *barrierTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (b *barrierTool) Run(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		N string `json:"n"`
	}
	_ = json.Unmarshal(args, &a)
	b.arrived <- a.N
	<-b.release
	return a.N, nil
}

func TestRunToolCallsParallel(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bt := &barrierTool{arrived: make(chan string, 2), release: make(chan struct{})}
	reg.Register(bt)
	ag := New(&fakeClient{}, reg, 0.3, nil, "")

	calls := []llm.ToolCall{
		{ID: "1", Function: llm.FunctionCall{Name: "barrier", Arguments: `{"n":"a"}`}},
		{ID: "2", Function: llm.FunctionCall{Name: "barrier", Arguments: `{"n":"b"}`}},
	}

	// Release only after both calls have arrived; if they ran sequentially the
	// second would never arrive and this would block until the test deadline.
	go func() {
		<-bt.arrived
		<-bt.arrived
		close(bt.release)
	}()

	done := make(chan []string, 1)
	go func() { done <- ag.runToolCalls(context.Background(), calls, Events{}) }()

	select {
	case res := <-done:
		if res[0] != "a" || res[1] != "b" {
			t.Fatalf("result order not preserved: %v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tool calls did not run concurrently")
	}
}

// fakeClient emits one tool call on its first tool-enabled request, then final
// text, so Run terminates on its own without any loop guard.
type fakeClient struct {
	calls    int
	toolName string
	args     string
	final    string
}

func (f *fakeClient) Stream(_ context.Context, _ []llm.Message, toolDefs []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	f.calls++
	if toolDefs != nil && f.calls == 1 {
		return llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID: "1", Type: "function",
				Function: llm.FunctionCall{Name: f.toolName, Arguments: f.args},
			}},
		}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: f.final}, nil
}

// contentThenToolClient returns prose plus a tool call on its first tool-enabled
// request, then final text, mimicking a model that narrates before acting.
type contentThenToolClient struct{ calls int }

func (c *contentThenToolClient) Stream(_ context.Context, _ []llm.Message, toolDefs []llm.Tool,
	_ float64, _ func(llm.StreamEvent)) (llm.Message, error) {
	c.calls++
	if toolDefs != nil && c.calls == 1 {
		return llm.Message{
			Role:    llm.RoleAssistant,
			Content: "let me check the directory",
			ToolCalls: []llm.ToolCall{{
				ID: "1", Type: "function",
				Function: llm.FunctionCall{Name: "list_dir", Arguments: `{"path":"."}`},
			}},
		}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: "all done"}, nil
}

// A step that carries both prose and tool calls must surface its prose through
// OnAssistantMessage (so the UI commits it above the tool output), while the
// final answer is delivered only as Run's return value, never as a step.
func TestRunEmitsIntermediateContentNotFinal(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag := New(&contentThenToolClient{}, reg, 0.3, nil, "")

	var steps []string
	answer, err := ag.Run(context.Background(), "hi", Events{
		OnAssistantMessage: func(c string) { steps = append(steps, c) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "all done" {
		t.Fatalf("unexpected final answer: %q", answer)
	}
	if len(steps) != 1 || steps[0] != "let me check the directory" {
		t.Fatalf("expected one intermediate step, got %v", steps)
	}
}

func TestClipToolResult(t *testing.T) {
	if got := clipToolResult("short"); got != "short" {
		t.Fatalf("small result must pass through unchanged, got %q", got)
	}
	big := strings.Repeat("A", maxToolResultBytes*3)
	got := clipToolResult(big)
	if len(got) >= len(big) {
		t.Fatalf("oversized result not truncated: %d bytes", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("truncation marker missing: %q", got[len(got)-80:])
	}
	if len(got) > maxToolResultBytes+256 {
		t.Fatalf("clipped result still too large: %d bytes", len(got))
	}
}

func TestSetMessagesKeepsSystemPrompt(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag := New(&fakeClient{}, reg, 0.3, nil, "LIVE PROMPT")

	// A resumed session carries its own (stale) system message plus history.
	ag.SetMessages([]llm.Message{
		{Role: llm.RoleSystem, Content: "OLD PROMPT"},
		{Role: llm.RoleUser, Content: "hi"},
	})

	got := ag.Messages()
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d: %+v", len(got), got)
	}
	if got[0].Role != llm.RoleSystem || got[0].Content != "LIVE PROMPT" {
		t.Fatalf("system prompt not preserved: %+v", got[0])
	}
	if got[1].Content != "hi" {
		t.Fatalf("history not restored: %+v", got[1])
	}
}

func TestSetSystemReplacesPrompt(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag := New(&fakeClient{}, reg, 0.3, nil, "OLD PROMPT")
	ag.SetMessages([]llm.Message{{Role: llm.RoleUser, Content: "hi"}})

	ag.SetSystem("NEW PROMPT")

	got := ag.Messages()
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d: %+v", len(got), got)
	}
	if got[0].Role != llm.RoleSystem || got[0].Content != "NEW PROMPT" {
		t.Fatalf("system prompt not replaced: %+v", got[0])
	}
	if got[1].Content != "hi" {
		t.Fatalf("history should be preserved: %+v", got[1])
	}
}

func TestRunStopsOnCancelledContext(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeClient{toolName: "list_dir", args: `{"path":"."}`, final: "x"}
	ag := New(fc, reg, 0.3, nil, "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ag.Run(ctx, "hi", Events{}); err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if fc.calls != 0 {
		t.Fatalf("expected no model calls on a cancelled context, got %d", fc.calls)
	}
}

// alwaysFailEditTool stands in for edit_file: it always errors and counts how
// many times it actually executed, so a test can prove the repeat guard refuses
// re-runs instead of letting them reach the tool.
type alwaysFailEditTool struct{ runs int }

func (e *alwaysFailEditTool) Name() string            { return "edit_file" }
func (e *alwaysFailEditTool) Description() string     { return "test edit" }
func (e *alwaysFailEditTool) NeedsConfirm() bool      { return false }
func (e *alwaysFailEditTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (e *alwaysFailEditTool) Run(_ context.Context, _ json.RawMessage) (string, error) {
	e.runs++
	return "", errEditFail
}

var errEditFail = errEdit("old_string not found")

type errEdit string

func (e errEdit) Error() string { return string(e) }

// repeatEditClient always re-emits the SAME edit_file call on every tool-enabled
// turn, mimicking the local model's runaway loop, then yields final text once
// tools are withheld (the forceAnswer path).
type repeatEditClient struct{ calls int }

func (c *repeatEditClient) Stream(_ context.Context, _ []llm.Message, toolDefs []llm.Tool,
	_ float64, _ func(llm.StreamEvent)) (llm.Message, error) {
	c.calls++
	if toolDefs != nil {
		return llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID: "1", Type: "function",
				Function: llm.FunctionCall{Name: "edit_file", Arguments: `{"path":"x.go","old_string":"a","new_string":"b"}`},
			}},
		}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: "gave up cleanly"}, nil
}

// The repeat guard must bound a runaway: an identical edit_file that already
// failed is refused (not re-executed), and after maxGuardTrips refusals the turn
// is forced to a final answer instead of looping forever.
func TestRepeatedFailingEditIsBounded(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	et := &alwaysFailEditTool{}
	reg.Register(et)
	ag := New(&repeatEditClient{}, reg, 0.3, nil, "")

	done := make(chan string, 1)
	go func() {
		ans, _ := ag.Run(context.Background(), "edit it", Events{})
		done <- ans
	}()
	select {
	case ans := <-done:
		if ans != "gave up cleanly" {
			t.Fatalf("expected forced final answer, got %q", ans)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("repeat guard did not bound the loop")
	}
	if et.runs != 1 {
		t.Fatalf("edit_file should execute once then be refused; ran %d times", et.runs)
	}
}

// statefulEditTool fails when old_string is "FAIL" and succeeds when it is "OK",
// counting how many times it actually executed a failing call - so a test can
// tell whether the repeat guard refused a retry or let it through.
type statefulEditTool struct {
	mu       sync.Mutex
	failRuns int
}

func (e *statefulEditTool) Name() string            { return "edit_file" }
func (e *statefulEditTool) Description() string     { return "test edit" }
func (e *statefulEditTool) NeedsConfirm() bool      { return false }
func (e *statefulEditTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (e *statefulEditTool) Run(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		OldString string `json:"old_string"`
	}
	_ = json.Unmarshal(args, &a)
	if a.OldString == "FAIL" {
		e.mu.Lock()
		e.failRuns++
		e.mu.Unlock()
		return "", errEdit("old_string not found")
	}
	return "ok", nil
}

// scriptedClient emits one preset tool call per tool-enabled turn, then final
// text, so a test can drive an exact sequence of tool calls.
type scriptedClient struct {
	step  int
	calls []llm.ToolCall
}

func (c *scriptedClient) Stream(_ context.Context, _ []llm.Message, toolDefs []llm.Tool,
	_ float64, _ func(llm.StreamEvent)) (llm.Message, error) {
	if toolDefs != nil && c.step < len(c.calls) {
		tc := c.calls[c.step]
		c.step++
		return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: "done"}, nil
}

// A failing edit's signature must be cleared by a later SUCCESSFUL edit to the
// same file, so a subsequent identical-args call is executed again rather than
// refused (the file changed in between, so the old failure may now be valid).
type repeatToolClient struct{ calls int }

func (c *repeatToolClient) Stream(_ context.Context, _ []llm.Message, toolDefs []llm.Tool,
	_ float64, _ func(llm.StreamEvent)) (llm.Message, error) {
	c.calls++
	if toolDefs != nil {
		return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "1", Type: "function",
			Function: llm.FunctionCall{Name: "list_dir", Arguments: `{"path":"."}`},
		}}}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: "done"}, nil
}

// roundSummaryClient keeps requesting a tool while tools are offered, and returns a distinctive
// wrap-up when tools are disabled (the budgetStop summary call).
type roundSummaryClient struct{ wrapUps int }

func (c *roundSummaryClient) Stream(_ context.Context, _ []llm.Message, toolDefs []llm.Tool,
	_ float64, _ func(llm.StreamEvent)) (llm.Message, error) {
	if toolDefs != nil {
		return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "1", Type: "function",
			Function: llm.FunctionCall{Name: "list_dir", Arguments: `{"path":"."}`},
		}}}, nil
	}
	c.wrapUps++
	return llm.Message{Role: llm.RoleAssistant, Content: "SUMMARY: changed A, next do B"}, nil
}

func TestModelRoundBudgetWrapsUpWithSummary(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fc := &roundSummaryClient{}
	ag := New(fc, reg, 0.3, nil, "")
	ag.SetTurnBudget(TurnBudget{MaxModelRounds: 2})
	ans, err := ag.Run(context.Background(), "implement the ticket", Events{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ans, "Budget exhausted") {
		t.Fatalf("round cap should wrap up with a summary, not the canned line: %q", ans)
	}
	if !strings.Contains(ans, "SUMMARY") || fc.wrapUps != 1 {
		t.Fatalf("expected exactly one wrap-up summary, got ans=%q wrapUps=%d", ans, fc.wrapUps)
	}
}

// roundWrapFailClient loops on tools but errors when tools are disabled, so budgetStop's wrap-up
// call fails and must fall back to the canned message.
type roundWrapFailClient struct{}

func (roundWrapFailClient) Stream(_ context.Context, _ []llm.Message, toolDefs []llm.Tool,
	_ float64, _ func(llm.StreamEvent)) (llm.Message, error) {
	if toolDefs != nil {
		return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "1", Type: "function",
			Function: llm.FunctionCall{Name: "list_dir", Arguments: `{"path":"."}`},
		}}}, nil
	}
	return llm.Message{}, fmt.Errorf("no capacity for a wrap-up")
}

func TestModelRoundBudgetFallsBackWhenWrapUpFails(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag := New(roundWrapFailClient{}, reg, 0.3, nil, "")
	ag.SetTurnBudget(TurnBudget{MaxModelRounds: 2})
	ans, err := ag.Run(context.Background(), "implement the ticket", Events{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ans, "Budget exhausted") || !strings.Contains(ans, "model round budget reached") {
		t.Fatalf("expected canned fallback on failed wrap-up, got %q", ans)
	}
}

func TestRepeatedReadToolLoopHitsBudget(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fc := &repeatToolClient{}
	ag := New(fc, reg, 0.3, nil, "")
	ag.SetTurnBudget(TurnBudget{MaxRepeatedToolCalls: 2})
	var notices []string
	ans, err := ag.Run(context.Background(), "loop", Events{OnNotice: func(s string) { notices = append(notices, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ans, "Budget exhausted") || !strings.Contains(ans, "repeated tool-call") {
		t.Fatalf("expected budget exhausted answer, got %q", ans)
	}
	if len(notices) == 0 || !strings.Contains(notices[len(notices)-1], "budget exhausted") {
		t.Fatalf("expected budget notice, got %v", notices)
	}
}

func TestToolCallBudget(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fc := &repeatToolClient{}
	ag := New(fc, reg, 0.3, nil, "")
	ag.SetTurnBudget(TurnBudget{MaxToolCalls: 1})
	ans, err := ag.Run(context.Background(), "loop", Events{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ans, "Budget exhausted") || !strings.Contains(ans, "tool-call budget") {
		t.Fatalf("expected tool-call budget answer, got %q", ans)
	}
}

func TestWallClockBudget(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag := New(&repeatToolClient{}, reg, 0.3, nil, "")
	ag.SetTurnBudget(TurnBudget{MaxDuration: time.Nanosecond})
	ans, err := ag.Run(context.Background(), "timeout", Events{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ans, "Budget exhausted") || !strings.Contains(ans, "wall-clock") {
		t.Fatalf("expected wall-clock budget answer, got %q", ans)
	}
}

func TestSuccessfulEditClearsFailedSig(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	et := &statefulEditTool{}
	reg.Register(et)
	edit := func(old string) llm.ToolCall {
		return llm.ToolCall{ID: "1", Type: "function", Function: llm.FunctionCall{
			Name: "edit_file", Arguments: `{"path":"f.go","old_string":"` + old + `","new_string":"x"}`,
		}}
	}
	ag := New(&scriptedClient{calls: []llm.ToolCall{
		edit("FAIL"), // fails, sig recorded
		edit("OK"),   // succeeds on f.go, clears the recorded sig
		edit("FAIL"), // identical to the first; must run again, not be refused
	}}, reg, 0.3, nil, "")

	if _, err := ag.Run(context.Background(), "go", Events{}); err != nil {
		t.Fatal(err)
	}
	if et.failRuns != 2 {
		t.Fatalf("expected the failing edit to execute twice (cleared by the success), ran %d", et.failRuns)
	}
}

// injectClient answers with a tool call on the first round so the turn keeps
// looping, and echoes back what the conversation holds on the second.
type injectClient struct {
	agent  *Agent
	rounds int
	saw    []string
}

func (c *injectClient) Stream(_ context.Context, msgs []llm.Message, _ []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	c.rounds++
	if c.rounds == 1 {
		// A message arrives while the model is "working" on this round.
		c.agent.Inject("the operator says: stop")
		return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "1", Type: "function", Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"nope"}`},
		}}}, nil
	}
	for _, m := range msgs {
		if m.Role == llm.RoleUser {
			c.saw = append(c.saw, m.Content)
		}
	}
	return llm.Message{Role: llm.RoleAssistant, Content: "ok"}, nil
}

func (c *injectClient) Tokenize(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}
func (c *injectClient) Model() llm.ModelInfo { return llm.ModelInfo{} }
func (c *injectClient) Endpoint() string     { return "" }

func TestInjectReachesTheRunningTurn(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &injectClient{}
	a := New(c, reg, 0.3, nil, "sys")
	c.agent = a

	if _, err := a.Run(context.Background(), "implement #5", Events{}); err != nil {
		t.Fatal(err)
	}
	// The injected text must be in the conversation the second round saw, after
	// the original input - not dropped, and not ahead of the turn's own prompt.
	var got string
	for _, s := range c.saw {
		if strings.Contains(s, "the operator says: stop") {
			got = s
		}
	}
	if got == "" {
		t.Fatalf("injected message never reached the model: %v", c.saw)
	}
}

func TestInjectIsRefusedWhenNoTurnIsRunning(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := New(&fakeClient{final: "done"}, reg, 0.3, nil, "sys")
	if a.Inject("anything") {
		t.Fatal("Inject must report false with no turn in flight, so the caller runs it normally")
	}
	if a.Inject("") {
		t.Fatal("an empty message is not worth delivering")
	}
}

// A message accepted in the instant a turn ends must still be answered, not
// dropped: the runtime was told it was taken and queues no turn for it.
func TestInjectAtTheEndOfATurnIsStillAnswered(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &lateInjectClient{}
	a := New(c, reg, 0.3, nil, "sys")
	c.agent = a

	if _, err := a.Run(context.Background(), "implement #5", Events{}); err != nil {
		t.Fatal(err)
	}
	if c.rounds < 2 {
		t.Fatalf("turn ended after %d round(s); the late message was never read", c.rounds)
	}
	if !c.sawLate {
		t.Fatal("the model never saw the message injected as the turn was ending")
	}
}

// lateInjectClient injects while producing what would otherwise be the final
// answer of the turn.
type lateInjectClient struct {
	agent   *Agent
	rounds  int
	sawLate bool
}

func (c *lateInjectClient) Stream(_ context.Context, msgs []llm.Message, _ []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	c.rounds++
	if c.rounds == 1 {
		c.agent.Inject("stop right there")
		return llm.Message{Role: llm.RoleAssistant, Content: "all done"}, nil
	}
	for _, m := range msgs {
		if m.Role == llm.RoleUser && strings.Contains(m.Content, "stop right there") {
			c.sawLate = true
		}
	}
	return llm.Message{Role: llm.RoleAssistant, Content: "stopped"}, nil
}

func (c *lateInjectClient) Tokenize(_ context.Context, t string) (int, error) { return len(t) / 4, nil }
func (c *lateInjectClient) Model() llm.ModelInfo                              { return llm.ModelInfo{} }
func (c *lateInjectClient) Endpoint() string                                  { return "" }
