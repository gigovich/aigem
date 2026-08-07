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

// The per-call events cannot distinguish two task calls batched into one
// assistant message from two emitted in consecutive rounds, and that difference
// IS parallel delegation. OnToolBatch is what carries it.
func TestToolBatchReportsCallsGroupedByRound(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fc := &parallelFake{}
	reg.Register(NewTaskTool(fc, reg, 0.3, nil, DefaultSubagents(), ""))

	var mu sync.Mutex
	var batches [][]ToolCallRef
	var rounds []int
	ev := Events{OnToolBatch: func(round int, calls []ToolCallRef) {
		mu.Lock()
		defer mu.Unlock()
		batches = append(batches, calls)
		rounds = append(rounds, round)
	}}
	// The ids the batch reports have to be the ones the nested runs are tagged
	// with, or nothing downstream can tie a subagent back to the call that
	// started it.
	var startIDs []string
	ev.OnAgentStart = func(id, _, _ string) {
		mu.Lock()
		defer mu.Unlock()
		startIDs = append(startIDs, id)
	}

	if _, err := New(fc, reg, 0.3, nil, "").Run(context.Background(), "go", ev); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 {
		t.Fatalf("expected one batch, got %d: %v", len(batches), batches)
	}
	if len(batches[0]) != 2 || batches[0][0].Name != TaskToolName || batches[0][1].Name != TaskToolName {
		t.Fatalf("expected both task calls in one batch, got %v", batches[0])
	}
	if rounds[0] != 1 {
		t.Fatalf("expected batch in model round 1, got %d", rounds[0])
	}
	inBatch := map[string]bool{}
	for _, c := range batches[0] {
		if c.ID == "" {
			t.Fatal("a batched call was reported without an id")
		}
		inBatch[c.ID] = true
	}
	for _, id := range startIDs {
		if !inBatch[id] {
			t.Fatalf("subagent started under id %q, which the batch never reported: %v", id, batches[0])
		}
	}
	if len(startIDs) != 2 {
		t.Fatalf("expected two subagent starts, got %d", len(startIDs))
	}
}

// A provider that omits call ids must still produce ids the two sides agree on,
// or matching a nested run to its call silently fails.
func TestCallRefsFillInMissingIDs(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag := New(&fakeClient{final: "done"}, reg, 0.3, nil, "")
	refs := ag.callRefs([]llm.ToolCall{
		{Function: llm.FunctionCall{Name: "grep"}},
		{ID: "given", Function: llm.FunctionCall{Name: "task"}},
		{Function: llm.FunctionCall{Name: "task"}},
	})
	if refs[1].ID != "given" {
		t.Errorf("an id the provider supplied must be kept, got %q", refs[1].ID)
	}
	if refs[0].ID == "" || refs[2].ID == "" || refs[0].ID == refs[2].ID {
		t.Errorf("missing ids must be filled in, and distinctly: %+v", refs)
	}
}

// The block used to live in the base system prompt, where a user's custom
// SYSTEM.md replaced it wholesale while the task tool stayed registered. Built
// from the registry, it also describes custom agents, which the old hardcoded
// list could not.
func TestDelegationPromptDescribesTheRegisteredAgents(t *testing.T) {
	agents := DefaultSubagents()
	agents.Add(SubagentDef{Name: "researcher", Description: "digs through the docs", Tools: readOnlyTools})

	got := DelegationPrompt(agents)
	for _, want := range []string{"scout", "code-writer", "simplifier", "reviewer",
		"researcher: digs through the docs"} {
		if !strings.Contains(got, want) {
			t.Errorf("delegation prompt never mentions %q", want)
		}
	}
	// The two rules the harness scores, plus the one that keeps an explicit
	// request from being reasoned away.
	for _, want := range []string{"SINGLE response", "PER piece", "user ASKS"} {
		if !strings.Contains(got, want) {
			t.Errorf("delegation prompt lost the %q rule", want)
		}
	}
	// A "do not over-delegate" rule was tried and measured to cost delegation
	// without improving precision; the base prompt carries that advice already.
	// Re-adding one here is a decision to make against fresh numbers, not by
	// reflex, so the absence is pinned.
	for _, unwanted := range []string{"Do NOT delegate", "as wrong as delegating nothing"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("delegation prompt regained the %q guard - see evals/ before keeping it", unwanted)
		}
	}
	if DelegationPrompt(NewSubagentRegistry()) != "" {
		t.Error("with no agents registered the block must be empty, not a promise of nothing")
	}
	if DelegationPrompt(nil) != "" {
		t.Error("a nil registry must yield no block")
	}
}

// ctxProbe delegates once, then answers, and records whether the subagent's own
// stream was marked retryable after emitting.
type ctxProbe struct {
	mu        sync.Mutex
	mainTurns int
	sawMark   bool
	toolSeen  bool
}

func (p *ctxProbe) Stream(ctx context.Context, _ []llm.Message, defs []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	hasTask := false
	for _, d := range defs {
		if d.Function.Name == TaskToolName {
			hasTask = true
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !hasTask {
		// The subagent's own call: no task tool is offered to it.
		if llm.RetryAfterEmit(ctx) {
			p.sawMark = true
		}
		return llm.Message{Role: llm.RoleAssistant, Content: "sub done"}, nil
	}
	p.toolSeen = true
	if p.mainTurns > 0 {
		return llm.Message{Role: llm.RoleAssistant, Content: "all done"}, nil
	}
	p.mainTurns++
	return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
		ID: "a", Type: "function",
		Function: llm.FunctionCall{Name: TaskToolName, Arguments: `{"agent_type":"scout","prompt":"A"}`},
	}}}, nil
}

// The retry-after-emit fix in internal/llm only bites if delegation actually
// marks the context; without this the two halves can drift apart silently and a
// transient provider error keeps killing whole delegations.
func TestDelegatedRunMayBeRetriedAfterEmitting(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &ctxProbe{}
	reg.Register(NewTaskTool(p, reg, 0.3, nil, DefaultSubagents(), ""))

	if _, err := New(p, reg, 0.3, nil, "").Run(context.Background(), "go", Events{}); err != nil {
		t.Fatal(err)
	}
	if !p.toolSeen {
		t.Fatal("the main agent never got the task tool; the test proves nothing")
	}
	if !p.sawMark {
		t.Fatal("a subagent's stream was not marked retryable after emitting - " +
			"its deltas reach no one, so a transient failure should cost a retry, not the delegation")
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
