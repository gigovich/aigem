package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/tools"
)

type captureSink struct {
	starts    []string
	ends      int
	agentEnds int
}

func (c *captureSink) AgentStart(agent, prompt string)   {}
func (c *captureSink) AgentEnd(result string, err error) { c.agentEnds++ }
func (c *captureSink) SubToolStart(agent, tool string, _ json.RawMessage) {
	c.starts = append(c.starts, agent+":"+tool)
}
func (c *captureSink) SubToolEnd(agent, tool, result string, err error) { c.ends++ }
func (c *captureSink) SubNotice(agent, text string)                     {}

func TestTaskToolDelegates(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The fake makes the subagent call a tool once, then produce a final answer.
	fc := &fakeClient{toolName: "list_dir", args: `{"path":"."}`, final: "scout done"}
	agents := DefaultSubagents()
	tt := NewTaskTool(fc, reg, 0.3, nil, agents, "")

	sink := &captureSink{}
	ctx := WithSink(context.Background(), sink)
	got, err := tt.Run(ctx, json.RawMessage(`{"agent_type":"scout","prompt":"find X"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "scout done" {
		t.Fatalf("expected subagent answer, got %q", got)
	}
	if len(sink.starts) == 0 || sink.starts[0] != "scout:list_dir" {
		t.Fatalf("expected nested list_dir activity forwarded, got %v", sink.starts)
	}
	if sink.ends != len(sink.starts) {
		t.Fatalf("tool start/end mismatch: %d starts, %d ends", len(sink.starts), sink.ends)
	}
	if sink.agentEnds != 1 {
		t.Fatalf("expected one AgentEnd, got %d", sink.agentEnds)
	}
}

// parallelFake drives a main agent to emit two task calls at once, then a final
// answer; subagents (no task tool) answer immediately. It is concurrency-safe so
// the test is meaningful under -race.
type parallelFake struct {
	mu        sync.Mutex
	mainTurns int
}

func (f *parallelFake) Stream(_ context.Context, _ []llm.Message, defs []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	hasTask := false
	for _, d := range defs {
		if d.Function.Name == TaskToolName {
			hasTask = true
		}
	}
	if !hasTask {
		return llm.Message{Role: llm.RoleAssistant, Content: "sub done"}, nil
	}
	f.mu.Lock()
	turn := f.mainTurns
	f.mainTurns++
	f.mu.Unlock()
	if turn > 0 {
		return llm.Message{Role: llm.RoleAssistant, Content: "all done"}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
		{ID: "a", Type: "function", Function: llm.FunctionCall{Name: TaskToolName, Arguments: `{"agent_type":"scout","prompt":"A"}`}},
		{ID: "b", Type: "function", Function: llm.FunctionCall{Name: TaskToolName, Arguments: `{"agent_type":"reviewer","prompt":"B"}`}},
	}}, nil
}

// safeSink counts agent-lifecycle callbacks from concurrent subagents safely.
type safeSink struct {
	mu     sync.Mutex
	starts int
	ends   int
}

func (s *safeSink) AgentStart() { s.mu.Lock(); s.starts++; s.mu.Unlock() }
func (s *safeSink) AgentEnd()   { s.mu.Lock(); s.ends++; s.mu.Unlock() }

func TestParallelDelegationRun(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fc := &parallelFake{}
	agents := DefaultSubagents()
	reg.Register(NewTaskTool(fc, reg, 0.3, nil, agents, ""))

	sink := &safeSink{}
	ev := Events{
		OnAgentStart: func(id, ag, prompt string) { sink.AgentStart() },
		OnAgentEnd:   func(id, result string, err error) { sink.AgentEnd() },
	}
	ag := New(fc, reg, 0.3, nil, "")

	got, err := ag.Run(context.Background(), "go", ev)
	if err != nil {
		t.Fatal(err)
	}
	if got != "all done" {
		t.Fatalf("expected final answer, got %q", got)
	}
	if sink.starts != 2 || sink.ends != 2 {
		t.Fatalf("expected 2 agent starts/ends, got %d/%d", sink.starts, sink.ends)
	}
}

func TestTaskToolScopesTools(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// scout is read-only: write_file must not be in its scoped registry.
	def, _ := DefaultSubagents().Get("scout")
	sub := reg.Subset(excluding(def.Tools, TaskToolName))
	if _, ok := sub.Get("write_file"); ok {
		t.Fatal("scout must not have write_file")
	}
	if _, ok := sub.Get("read_file"); !ok {
		t.Fatal("scout must have read_file")
	}
}

func TestTaskToolRejectsUnknownAgent(t *testing.T) {
	reg, _ := tools.NewRegistry(t.TempDir())
	tt := NewTaskTool(&fakeClient{}, reg, 0.3, nil, DefaultSubagents(), "")
	if _, err := tt.Run(context.Background(), json.RawMessage(`{"agent_type":"nope","prompt":"x"}`)); err == nil {
		t.Fatal("expected error for unknown agent_type")
	}
	if _, err := tt.Run(context.Background(), json.RawMessage(`{"agent_type":"scout","prompt":""}`)); err == nil {
		t.Fatal("expected error for empty prompt")
	}
}

func TestParseSubagentAndLoad(t *testing.T) {
	dir := t.TempDir()
	md := "---\nname: custom\ndescription: does a thing\ntools: read_file, grep\n---\nYou are custom.\n"
	if err := os.WriteFile(filepath.Join(dir, "custom.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	r := DefaultSubagents()
	if err := LoadSubagentsInto(r, dir); err != nil {
		t.Fatal(err)
	}
	def, ok := r.Get("custom")
	if !ok {
		t.Fatal("custom agent not loaded")
	}
	if def.Description != "does a thing" || len(def.Tools) != 2 || def.Tools[0] != "read_file" {
		t.Fatalf("frontmatter not parsed: %+v", def)
	}
	if !strings.Contains(def.Prompt, "You are custom.") {
		t.Fatalf("body not used as prompt: %q", def.Prompt)
	}
}

func TestLoadSubagentsMissingDirOK(t *testing.T) {
	r := DefaultSubagents()
	if err := LoadSubagentsInto(r, filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("missing dir should be fine, got %v", err)
	}
}
