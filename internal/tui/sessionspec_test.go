package tui

import (
	"testing"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/hooks"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
)

// Every argument New forwards into a session, given a value that tells it apart
// from every other, so a field assigned the wrong argument - or none - is not
// something only the model would notice.
func TestSessionSpecCarriesEveryArgument(t *testing.T) {
	client := llm.NewRef(llm.New("http://127.0.0.1:9", "the-model"))
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agents := agent.DefaultSubagents()
	skills, _ := skill.Discover(t.TempDir())
	hookRunner, _ := hooks.Load(t.TempDir())
	modelReg, _ := llm.NewRegistry(t.TempDir(), llm.LocalProvider("http://127.0.0.1:9", "m", 1, 1))
	compact := agent.CompactConfig{CtxSize: 4242, KeepTurns: 7}
	retried := false

	spec := sessionSpec(client, reg, 0.71, "THE SYSTEM PROMPT", 1234, 567,
		agents, "THE PROJECT TEXT", skills, hookRunner, "THE TITLE", compact, modelReg,
		func(llm.RetryNotice) { retried = true })

	if spec.Backend != client {
		t.Error("Backend is not the client New was given")
	}
	if spec.Tools != reg {
		t.Error("Tools is not the registry New was given")
	}
	if spec.Models != modelReg {
		t.Error("Models is not the model registry New was given")
	}
	if spec.Agents != agents {
		t.Error("Agents is not the subagent registry New was given: the delegation tool disappears")
	}
	if spec.Skills != skills {
		t.Error("Skills is not the skill registry New was given")
	}
	if spec.Hooks != hookRunner {
		t.Error("Hooks is not the hooks runner New was given: hooks stop firing")
	}
	if spec.Project != "THE PROJECT TEXT" {
		t.Errorf("Project = %q", spec.Project)
	}
	if spec.System != "THE SYSTEM PROMPT" {
		t.Errorf("System = %q", spec.System)
	}
	if spec.Title != "THE TITLE" {
		t.Errorf("Title = %q", spec.Title)
	}
	if spec.Temp != 0.71 {
		t.Errorf("Temp = %v, want the temperature New was given", spec.Temp)
	}
	if spec.MaxTokens != 567 {
		t.Errorf("MaxTokens = %d", spec.MaxTokens)
	}
	if spec.CtxSize != 1234 {
		t.Errorf("CtxSize = %d", spec.CtxSize)
	}
	if spec.Compact != compact {
		t.Errorf("Compact = %+v, want %+v", spec.Compact, compact)
	}
	if spec.OnRetry == nil {
		t.Fatal("OnRetry is nil: a provider backoff would be a silent multi-second hang")
	}
	spec.OnRetry(llm.RetryNotice{})
	if !retried {
		t.Error("OnRetry is not the callback New was given")
	}
}

// The test above pins the body of sessionSpec; this one pins the call. New
// passes fourteen positional arguments, several of them the same type, so a
// swap compiles and runs - and the pair nothing else catches is the project
// text against the session title, which would name the conversation after the
// whole of AGENTS.md and hand every subagent the title instead of the project's
// conventions.
func TestNewNamesTheConversationWithTheTitleItWasGiven(t *testing.T) {
	m := wiredModel(t, "THE PROJECT TEXT", "THE TITLE", 4096)

	if got := m.local.Meta().Title; got != "THE TITLE" {
		t.Fatalf("conversation title = %q, want THE TITLE", got)
	}
}

// A model that declares no context window falls back to a real one. The
// fallback lives in New, not in the session, because New keeps the value too.
func TestNewFallsBackToADefaultContextWindow(t *testing.T) {
	m := wiredModel(t, "project", "title", 0)

	if m.ctxSize != runner.DefaultCtxSize {
		t.Fatalf("ctxSize = %d, want the default %d", m.ctxSize, runner.DefaultCtxSize)
	}
}

func wiredModel(t *testing.T, project, title string, ctxSize int) Model {
	t.Helper()
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	skills, _ := skill.Discover(t.TempDir())
	// The token cap is deliberately not DefaultCtxSize: with both 8192 the
	// fallback assertion below could not tell the default from the cap having
	// been assigned to the window.
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "m")), reg, 0.3, "m", "u", "SYSTEM",
		ctxSize, 4321, agent.DefaultSubagents(), project, skills, nil, nil, title,
		agent.CompactConfig{}, nil)
	t.Cleanup(m.local.Close)
	return m
}
