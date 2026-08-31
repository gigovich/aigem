package runner_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
	"github.com/gigovich/aigem/internal/uisession"
)

// deadBackend points at a closed port, so a turn that reaches the model fails
// fast instead of hitting a real server.
func deadBackend() *llm.Ref { return llm.NewRef(llm.New("http://127.0.0.1:9", "t")) }

func newEnvAndTools(t *testing.T, cwd string) (*runner.Env, *tools.Registry) {
	t.Helper()
	env, _ := runner.Load(runner.Options{Cwd: cwd})
	t.Cleanup(env.Close)
	reg, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	return env, reg
}

func TestNewSessionRegistersTheAgentsOwnTools(t *testing.T) {
	cwd := project(t)
	env, reg := newEnvAndTools(t, cwd)

	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: deadBackend(),
		Agents:  env.Agents,
		Skills:  env.Skills,
		Hooks:   env.Hooks,
		System:  env.SystemPrompt(reg),
	})
	t.Cleanup(s.Local.Close)

	if s.Local.Agent() == nil {
		t.Fatal("the session has no agent")
	}
	if s.Tools != reg {
		t.Fatal("the session reports a registry it was not built with")
	}
	for _, name := range []string{agent.TaskToolName, agent.TodoToolName} {
		if _, ok := reg.Get(name); !ok {
			t.Fatalf("expected %s in the registry, got %v", name, reg.Names())
		}
	}
}

// The delegation tool is what makes subagents possible, so a session built
// without a subagent registry must not advertise one.
func TestNewSessionWithoutSubagentsSkipsTheTaskTool(t *testing.T) {
	cwd := project(t)
	_, reg := newEnvAndTools(t, cwd)

	s := runner.NewSession(runner.Spec{Tools: reg, Backend: deadBackend()})
	t.Cleanup(s.Local.Close)

	if _, ok := reg.Get(agent.TaskToolName); ok {
		t.Fatal("a session with no subagents registered the delegation tool anyway")
	}
}

// A session built where skills already exist advertises them from the first
// turn: nothing re-registers the tool on the ordinary path.
func TestNewSessionRegistersTheSkillToolAtConstruction(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: say hi nicely\n---\nSay hello.\n")
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	env, reg := newEnvAndTools(t, cwd)

	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: deadBackend(),
		Skills:  env.Skills,
		Hooks:   env.Hooks,
	})
	t.Cleanup(s.Local.Close)

	tool, ok := reg.Get(agent.SkillToolName)
	if !ok {
		t.Fatalf("expected the skill tool for a project that has skills, got %v", reg.Names())
	}
	if desc := tool.Description(); !strings.Contains(desc, "say hi nicely") {
		t.Fatalf("the registered tool describes a different catalog: %q", desc)
	}
}

// A project whose only skills are untrusted has none to advertise at launch.
// Approving them mid-session re-runs discovery, and the tool has to be
// registered against the result - that is the whole reason the hook exists.
func TestRegisterSkillToolPicksUpSkillsApprovedMidSession(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: say hi nicely\n---\nSay hello.\n")
	env, reg := newEnvAndTools(t, cwd)

	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: deadBackend(),
		Skills:  env.Skills,
		Hooks:   env.Hooks,
	})
	t.Cleanup(s.Local.Close)

	if _, ok := reg.Get(agent.SkillToolName); ok {
		t.Fatal("withheld skills must not produce a skill tool")
	}
	if s.RegisterSkillTool == nil {
		t.Fatal("no way to register the skill tool once the skills are approved")
	}

	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	approved, errs := skill.Discover(cwd)
	if len(errs) != 0 {
		t.Fatalf("discovery after approval: %v", errs)
	}
	s.RegisterSkillTool(approved)

	tool, ok := reg.Get(agent.SkillToolName)
	if !ok {
		t.Fatalf("expected the skill tool after approval, got %v", reg.Names())
	}
	if desc := tool.Description(); !strings.Contains(desc, "say hi nicely") {
		t.Fatalf("the registered tool describes a different catalog: %q", desc)
	}
}

// A model that declares no context window still needs one to reason about.
func TestNewSessionDefaultsTheContextWindow(t *testing.T) {
	cwd := project(t)
	_, reg := newEnvAndTools(t, cwd)

	s := runner.NewSession(runner.Spec{Tools: reg, Backend: deadBackend(), CtxSize: 0})
	t.Cleanup(s.Local.Close)

	ch, stop, err := s.Local.Subscribe(uisession.Client{ID: "t", Kind: "test"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := s.Local.Submit("hi", nil); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, ch, uisession.KindSessionMeta)
	if ev.Ctx != runner.DefaultCtxSize {
		t.Fatalf("expected the default context window %d, got %d", runner.DefaultCtxSize, ev.Ctx)
	}
}

// OnRetry is what turns a provider backoff into something the person waiting
// can see. Nothing else in the session reports it, so an unwired callback is a
// silent multi-second hang.
func TestNewSessionReportsProviderRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cwd := project(t)
	_, reg := newEnvAndTools(t, cwd)

	notices := make(chan llm.RetryNotice, 4)
	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: llm.NewRef(llm.New(srv.URL, "t")),
		OnRetry: func(n llm.RetryNotice) {
			select {
			case notices <- n:
			default:
			}
		},
	})
	t.Cleanup(s.Local.Close)

	if err := s.Local.Submit("hi", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-notices:
		if n.Attempt < 1 || n.Attempts < 2 {
			t.Fatalf("a retry notice that describes no retry: %+v", n)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a failing provider produced no retry notice")
	}
	// The turn is mid-backoff; let it go rather than paying for the rest.
	s.Local.Interrupt()
}

// waitFor takes events until one matches, so a test can assert on a kind
// without listing every event that legitimately precedes it, and fails rather
// than hanging when none arrives.
func waitFor(t *testing.T, ch <-chan uisession.Event, kind uisession.Kind) uisession.Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("the event channel closed before a %s event", kind)
			}
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %s event", kind)
		}
	}
}
