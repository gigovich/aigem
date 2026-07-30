package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/tools"
)

func newPlanAgent(t *testing.T, client streamer) *Agent {
	t.Helper()
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag := New(client, reg, 0.3, nil, "")
	reg.Register(NewTodoTool(ag))
	return ag
}

func TestTodoToolValidatesAndStores(t *testing.T) {
	ag := newPlanAgent(t, &fakeClient{})
	tool, ok := ag.tools.Get(TodoToolName)
	if !ok {
		t.Fatal("todo_write not registered")
	}
	res, err := tool.Run(context.Background(), []byte(`{"todos":[
		{"text":"first","status":"in_progress"},
		{"text":"  ","status":"pending"},
		{"text":"second","status":"bogus"},
		{"text":"third","status":"completed"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := ag.Todos()
	if len(got) != 3 {
		t.Fatalf("blank-text item should be dropped; got %d: %+v", len(got), got)
	}
	if got[1].Status != TodoPending {
		t.Fatalf("unknown status should fall back to pending, got %q", got[1].Status)
	}
	if !strings.Contains(res, "1/3 done") {
		t.Fatalf("summary should report completion count, got %q", res)
	}
}

func TestHasOpenPlanAndNextOpen(t *testing.T) {
	ag := newPlanAgent(t, &fakeClient{})
	if ag.hasOpenPlan() {
		t.Fatal("empty plan is not open")
	}
	ag.setTodos([]TodoItem{
		{Text: "a", Status: TodoCompleted},
		{Text: "b", Status: TodoPending},
		{Text: "c", Status: TodoInProgress},
	})
	if !ag.hasOpenPlan() {
		t.Fatal("plan with open items should be open")
	}
	if next := ag.nextOpen(); next != "c" {
		t.Fatalf("in_progress item should win nextOpen, got %q", next)
	}
	ag.setTodos([]TodoItem{{Text: "a", Status: TodoCompleted}})
	if ag.hasOpenPlan() {
		t.Fatal("all-completed plan is not open")
	}
}

func todoCall(args string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
		ID: "t", Type: "function",
		Function: llm.FunctionCall{Name: TodoToolName, Arguments: `{"todos":` + args + `}`},
	}}}
}

func isEvaluatorCall(defs []llm.Tool) bool {
	for _, d := range defs {
		if d.Function.Name == reportStatusTool.Function.Name {
			return true
		}
	}
	return false
}

// planFake drives a full autonomous round: plan, stop, evaluator says continue,
// finish the plan, stop again (now with no open items, so no evaluator).
type planFake struct{ worker, eval int }

func (f *planFake) Stream(_ context.Context, _ []llm.Message, defs []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	if isEvaluatorCall(defs) {
		f.eval++
		return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "e", Type: "function",
			Function: llm.FunctionCall{Name: "report_status", Arguments: `{"intent":"continue"}`},
		}}}, nil
	}
	f.worker++
	switch f.worker {
	case 1:
		return todoCall(`[{"text":"step one","status":"in_progress"}]`), nil
	case 2:
		return llm.Message{Role: llm.RoleAssistant, Content: "partial progress"}, nil
	case 3:
		return todoCall(`[{"text":"step one","status":"completed"}]`), nil
	default:
		return llm.Message{Role: llm.RoleAssistant, Content: "all done"}, nil
	}
}

type evalParseFailFake struct{ worker, eval int }

func (f *evalParseFailFake) Stream(_ context.Context, _ []llm.Message, defs []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	if isEvaluatorCall(defs) {
		f.eval++
		return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "e", Type: "function",
			Function: llm.FunctionCall{Name: "report_status", Arguments: `{not-json`},
		}}}, nil
	}
	f.worker++
	if f.worker == 1 {
		return todoCall(`[{"text":"unfinished","status":"in_progress"}]`), nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: "partial answer"}, nil
}

func TestEvaluatorParseFailureSurfacesOpenPlanStop(t *testing.T) {
	f := &evalParseFailFake{}
	ag := newPlanAgent(t, f)
	var notices []string
	answer, err := ag.Run(context.Background(), "go", Events{OnNotice: func(s string) { notices = append(notices, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if f.eval != 1 {
		t.Fatalf("expected evaluator to run once, ran %d", f.eval)
	}
	if !strings.Contains(answer, "Evaluator unavailable; stopping with open plan") {
		t.Fatalf("final answer should distinguish evaluator failure from normal done, got %q", answer)
	}
	found := false
	for _, n := range notices {
		if strings.Contains(n, "autonomous evaluator unavailable") && strings.Contains(n, "parse report_status") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected evaluator failure notice, got %v", notices)
	}
	if !ag.hasOpenPlan() {
		t.Fatal("open plan should remain open when evaluator parse fails")
	}
}

func TestRunAutoContinuesUntilPlanComplete(t *testing.T) {
	f := &planFake{}
	ag := newPlanAgent(t, f)
	answer, err := ag.Run(context.Background(), "do the work", Events{})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "all done" {
		t.Fatalf("expected final answer after plan completes, got %q", answer)
	}
	if f.eval != 1 {
		t.Fatalf("evaluator should run once (only while plan was open), ran %d", f.eval)
	}
	if f.worker != 4 {
		t.Fatalf("worker should run 4 times, ran %d", f.worker)
	}
}

// stuckFake never changes its plan and never asks the user; the evaluator would
// always say continue. Progress detection must end the turn after one push.
type stuckFake struct{ worker, eval int }

func (f *stuckFake) Stream(_ context.Context, _ []llm.Message, defs []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	if isEvaluatorCall(defs) {
		f.eval++
		return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "e", Type: "function",
			Function: llm.FunctionCall{Name: "report_status", Arguments: `{"intent":"continue"}`},
		}}}, nil
	}
	f.worker++
	return llm.Message{Role: llm.RoleAssistant, Content: "still going"}, nil
}

func TestRunAutoContinueStopsWithoutProgress(t *testing.T) {
	f := &stuckFake{}
	ag := newPlanAgent(t, f)
	ag.setTodos([]TodoItem{{Text: "never done", Status: TodoPending}})
	answer, err := ag.Run(context.Background(), "go", Events{})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "still going" {
		t.Fatalf("stalled turn should return the last answer, got %q", answer)
	}
	if f.eval != 1 {
		t.Fatalf("evaluator should run once then stop on no progress, ran %d", f.eval)
	}
	if f.worker != 2 {
		t.Fatalf("worker should run twice (push, then stall), ran %d", f.worker)
	}
}

// reconcileFake opens a plan of the given shape, then stops reporting the work
// done without a final todo_write; its evaluator returns "done".
type reconcileFake struct {
	worker int
	plan   string
}

func (f *reconcileFake) Stream(_ context.Context, _ []llm.Message, defs []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	if isEvaluatorCall(defs) {
		return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "e", Type: "function",
			Function: llm.FunctionCall{Name: "report_status", Arguments: `{"intent":"done"}`},
		}}}, nil
	}
	f.worker++
	if f.worker == 1 {
		return todoCall(f.plan), nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: "I finished everything."}, nil
}

// When the plan is a "forgot the final mark" shape (no pending behind the active
// step), a "done" verdict closes the active step so the sidebar is not stale.
func TestEvaluatorDoneClosesActiveStep(t *testing.T) {
	f := &reconcileFake{plan: `[{"text":"first","status":"completed"},{"text":"last","status":"in_progress"}]`}
	ag := newPlanAgent(t, f)
	var lastUpdate []TodoItem
	ev := Events{OnTodoUpdate: func(todos []TodoItem) { lastUpdate = todos }}

	answer, err := ag.Run(context.Background(), "go", ev)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "I finished everything." {
		t.Fatalf("expected the model's final answer, got %q", answer)
	}
	if got := ag.Todos(); got[1].Status != TodoCompleted {
		t.Fatalf("active step should be reconciled to completed, got %q", got[1].Status)
	}
	if len(lastUpdate) != 2 || lastUpdate[1].Status != TodoCompleted {
		t.Fatalf("OnTodoUpdate should fire with the reconciled plan, got %+v", lastUpdate)
	}
}

// A "done" verdict is authoritative: it closes the whole plan, including a
// trailing pending step the model finished but never marked.
func TestEvaluatorDoneClosesPendingTail(t *testing.T) {
	f := &reconcileFake{plan: `[{"text":"do it","status":"in_progress"},{"text":"later","status":"pending"}]`}
	ag := newPlanAgent(t, f)
	var lastUpdate []TodoItem
	ev := Events{OnTodoUpdate: func(todos []TodoItem) { lastUpdate = todos }}

	if _, err := ag.Run(context.Background(), "go", ev); err != nil {
		t.Fatal(err)
	}
	for i, it := range ag.Todos() {
		if it.Status != TodoCompleted {
			t.Fatalf("step %d not closed on done verdict: %+v", i, it)
		}
	}
	if len(lastUpdate) != 2 || lastUpdate[1].Status != TodoCompleted {
		t.Fatalf("OnTodoUpdate should fire with the fully reconciled plan, got %+v", lastUpdate)
	}
}

// A plan the model never advanced (every step still pending) is also closed on a
// "done" verdict - the loose-discipline shape that otherwise sticks at 0/N.
func TestEvaluatorDoneClosesAllPending(t *testing.T) {
	f := &reconcileFake{plan: `[{"text":"a","status":"pending"},{"text":"b","status":"pending"}]`}
	ag := newPlanAgent(t, f)

	if _, err := ag.Run(context.Background(), "go", Events{}); err != nil {
		t.Fatal(err)
	}
	for i, it := range ag.Todos() {
		if it.Status != TodoCompleted {
			t.Fatalf("pending step %d not closed on done verdict: %+v", i, it)
		}
	}
}

// stopFake immediately produces a final answer with no tool calls.
type stopFake struct{}

func (stopFake) Stream(_ context.Context, _ []llm.Message, _ []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	return llm.Message{Role: llm.RoleAssistant, Content: "ok"}, nil
}

func TestCompletedPlanClearsOnNewTurn(t *testing.T) {
	ag := newPlanAgent(t, stopFake{})
	ag.setTodos([]TodoItem{
		{Text: "a", Status: TodoCompleted},
		{Text: "b", Status: TodoCompleted},
	})
	var last []TodoItem
	fired := false
	ev := Events{OnTodoUpdate: func(td []TodoItem) { last = td; fired = true }}

	if _, err := ag.Run(context.Background(), "a brand new task", ev); err != nil {
		t.Fatal(err)
	}
	if got := ag.Todos(); len(got) != 0 {
		t.Fatalf("a fully-completed plan should clear on a new turn, got %+v", got)
	}
	if !fired || len(last) != 0 {
		t.Fatalf("OnTodoUpdate should fire with an empty plan, fired=%v last=%+v", fired, last)
	}
}

func TestPartialPlanSurvivesNewTurn(t *testing.T) {
	ag := newPlanAgent(t, stopFake{})
	ag.setTodos([]TodoItem{
		{Text: "a", Status: TodoCompleted},
		{Text: "b", Status: TodoInProgress},
	})
	if _, err := ag.Run(context.Background(), "follow-up", Events{}); err != nil {
		t.Fatal(err)
	}
	if got := ag.Todos(); len(got) != 2 {
		t.Fatalf("an unfinished plan must persist across a new turn, got %+v", got)
	}
}

// churnFake keeps changing its plan (so progress is always detected) but never
// completes it, exercising the hard autoContinue cap.
type churnFake struct{ worker, eval int }

func (f *churnFake) Stream(_ context.Context, _ []llm.Message, defs []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	if isEvaluatorCall(defs) {
		f.eval++
		return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "e", Type: "function",
			Function: llm.FunctionCall{Name: "report_status", Arguments: `{"intent":"continue"}`},
		}}}, nil
	}
	f.worker++
	if f.worker%2 == 1 {
		return todoCall(fmt.Sprintf(`[{"text":"step %d","status":"pending"}]`, f.worker)), nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: "more"}, nil
}

func TestRunAutoContinueIsCapped(t *testing.T) {
	f := &churnFake{}
	ag := newPlanAgent(t, f)
	answer, err := ag.Run(context.Background(), "go", Events{})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "more" {
		t.Fatalf("capped turn should return the last answer, got %q", answer)
	}
	if f.eval != maxAutoContinue {
		t.Fatalf("evaluator should run the cap (%d) times, ran %d", maxAutoContinue, f.eval)
	}
}
