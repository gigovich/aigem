package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestScheduleToolSetRemoveList(t *testing.T) {
	s, _ := NewScheduler(nil, nil)
	tool := NewScheduleTool(s)
	if tool.Name() != "schedule" || tool.NeedsConfirm() {
		t.Fatalf("name=%q needsConfirm=%v", tool.Name(), tool.NeedsConfirm())
	}
	run := func(args string) (string, error) {
		return tool.Run(context.Background(), json.RawMessage(args))
	}
	if _, err := run(`{"action":"set","id":"morning","expr":"0 9 * * 1-5","prompt":"post a summary"}`); err != nil {
		t.Fatalf("set: %v", err)
	}
	out, err := run(`{"action":"list"}`)
	if err != nil || !strings.Contains(out, "morning") || !strings.Contains(out, "0 9 * * 1-5") {
		t.Fatalf("list = %q, %v", out, err)
	}
	if _, err := run(`{"action":"set","id":"x","expr":"bad","prompt":"p"}`); err == nil {
		t.Fatal("set with bad expr should error")
	}
	if _, err := run(`{"action":"remove","id":"morning"}`); err != nil {
		t.Fatalf("remove: %v", err)
	}
	out, _ = run(`{"action":"list"}`)
	if strings.Contains(out, "morning") {
		t.Fatalf("job not removed: %q", out)
	}
}

func TestScheduleToolValidation(t *testing.T) {
	tool := NewScheduleTool(mustScheduler(t))
	run := func(args string) error {
		_, err := tool.Run(context.Background(), json.RawMessage(args))
		return err
	}
	if run(`{"action":"set","id":"x","expr":"* * * * *"}`) == nil {
		t.Error("set without prompt should error")
	}
	if run(`{"action":"set","expr":"* * * * *","prompt":"p"}`) == nil {
		t.Error("set without id should error")
	}
	if run(`{"action":"remove"}`) == nil {
		t.Error("remove without id should error")
	}
	if run(`{"action":"nope"}`) == nil {
		t.Error("unknown action should error")
	}
}

func mustScheduler(t *testing.T) *Scheduler {
	t.Helper()
	s, _ := NewScheduler(nil, nil)
	return s
}

func TestScheduleToolCannotRemoveBuiltin(t *testing.T) {
	s, _ := NewScheduler(nil, nil)
	if err := s.SetBuiltin(MemoryReviewJob("testbot")); err != nil {
		t.Fatal(err)
	}
	tool := NewScheduleTool(s)
	if _, err := runTool(t, tool, `{"action":"remove","id":"memory-review"}`); err == nil ||
		!strings.Contains(err.Error(), "built-in") {
		t.Fatalf("remove of builtin = %v", err)
	}
	list, err := runTool(t, tool, `{"action":"list"}`)
	if err != nil || !strings.Contains(list, "memory-review") {
		t.Fatalf("list = %q, %v", list, err)
	}
}

func TestScheduleToolCannotReplaceBuiltinAndMarksIt(t *testing.T) {
	s, _ := NewScheduler(nil, nil)
	if err := s.SetBuiltin(MemoryReviewJob("testbot")); err != nil {
		t.Fatal(err)
	}
	tool := NewScheduleTool(s)
	if _, err := runTool(t, tool,
		`{"action":"set","id":"memory-review","expr":"* * * * *","prompt":"hijack"}`); err == nil ||
		!strings.Contains(err.Error(), "built-in") {
		t.Fatalf("set of builtin id = %v", err)
	}
	list, err := runTool(t, tool, `{"action":"list"}`)
	if err != nil || !strings.Contains(list, "memory-review [built-in]:") {
		t.Fatalf("list = %q, %v", list, err)
	}
}
