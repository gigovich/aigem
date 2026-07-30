package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func runTool(t *testing.T, tool interface {
	Run(context.Context, json.RawMessage) (string, error)
}, args string) (string, error) {
	t.Helper()
	return tool.Run(context.Background(), json.RawMessage(args))
}

func TestMemoryToolSaveReadListDelete(t *testing.T) {
	tool := NewMemoryTool(NewStore(t.TempDir()))
	if tool.Name() != "memory" || tool.NeedsConfirm() {
		t.Fatalf("name=%q needsConfirm=%v", tool.Name(), tool.NeedsConfirm())
	}

	if _, err := runTool(t, tool,
		`{"action":"save","name":"deploy","description":"how to deploy","content":"make deploy"}`); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := runTool(t, tool, `{"action":"read","name":"deploy"}`)
	if err != nil || !strings.Contains(got, "make deploy") {
		t.Fatalf("read = %q, %v", got, err)
	}
	list, err := runTool(t, tool, `{"action":"list"}`)
	if err != nil || !strings.Contains(list, "deploy") || !strings.Contains(list, "how to deploy") {
		t.Fatalf("list = %q, %v", list, err)
	}
	if _, err := runTool(t, tool, `{"action":"delete","name":"deploy"}`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := runTool(t, tool, `{"action":"read","name":"deploy"}`); err == nil {
		t.Fatal("read after delete should error")
	}
}

func TestMemoryToolValidation(t *testing.T) {
	tool := NewMemoryTool(NewStore(t.TempDir()))
	if _, err := runTool(t, tool, `{"action":"save","name":"x"}`); err == nil {
		t.Error("save without description/content should error")
	}
	if _, err := runTool(t, tool, `{"action":"read"}`); err == nil {
		t.Error("read without name should error")
	}
	if _, err := runTool(t, tool, `{"action":"frobnicate"}`); err == nil {
		t.Error("unknown action should error")
	}
	if _, err := runTool(t, tool,
		`{"action":"save","name":"x","description":"d","content":"   "}`); err == nil {
		t.Error("save with whitespace-only content should error")
	}
	if _, err := runTool(t, tool,
		`{"action":"save","name":"","description":"d","content":"c"}`); err == nil {
		t.Error("save with empty name should error")
	}
}

func TestMemoryToolEmptyList(t *testing.T) {
	tool := NewMemoryTool(NewStore(t.TempDir()))
	got, err := runTool(t, tool, `{"action":"list"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(got), "empty") {
		t.Fatalf("empty list should say so, got %q", got)
	}
}

func TestMemoryToolArchiveRestoreAudit(t *testing.T) {
	tool := NewMemoryTool(NewStore(t.TempDir()))
	if _, err := runTool(t, tool, `{"action":"archive"}`); err == nil {
		t.Fatal("archive without name should error")
	}
	if _, err := runTool(t, tool, `{"action":"restore"}`); err == nil {
		t.Fatal("restore without name should error")
	}
	if _, err := runTool(t, tool,
		`{"action":"save","name":"stale","description":"old news","content":"x"}`); err != nil {
		t.Fatal(err)
	}
	if out, err := runTool(t, tool, `{"action":"archive","name":"stale"}`); err != nil ||
		!strings.Contains(out, "restore") {
		t.Fatalf("archive = %q, %v", out, err)
	}
	audit, err := runTool(t, tool, `{"action":"audit"}`)
	if err != nil || !strings.Contains(audit, "Archived: stale") {
		t.Fatalf("audit = %q, %v", audit, err)
	}
	if _, err := runTool(t, tool, `{"action":"restore","name":"stale"}`); err != nil {
		t.Fatal(err)
	}
	audit, _ = runTool(t, tool, `{"action":"audit"}`)
	if !strings.Contains(audit, "- stale:") || !strings.Contains(audit, "Archived: (none)") {
		t.Fatalf("audit after restore = %q", audit)
	}
}

func TestMemoryToolInspect(t *testing.T) {
	tool := NewMemoryTool(NewStore(t.TempDir()))
	if _, err := runTool(t, tool, `{"action":"inspect"}`); err == nil {
		t.Fatal("inspect without name should error")
	}
	if _, err := runTool(t, tool,
		`{"action":"save","name":"deep","description":"d","content":"deep body"}`); err != nil {
		t.Fatal(err)
	}
	got, err := runTool(t, tool, `{"action":"inspect","name":"deep"}`)
	if err != nil || !strings.Contains(got, "deep body") {
		t.Fatalf("inspect = %q, %v", got, err)
	}
	audit, _ := runTool(t, tool, `{"action":"audit"}`)
	if !strings.Contains(audit, "uses 0") {
		t.Fatalf("inspect must not count as a use: %q", audit)
	}
}
