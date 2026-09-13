package runner_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/uisession"
)

func TestHandleCommandsServesTheConversationsOwn(t *testing.T) {
	cwd := project(t)
	env, reg := newEnvAndTools(t, cwd)
	s := runner.NewSession(runner.Spec{Tools: reg, Backend: deadBackend(), Skills: env.Skills})
	t.Cleanup(s.Local.Close)
	s.HandleCommands(env.Skills, env.MCP)

	err := s.Local.Command("skill:nothing-here", "")
	if err == nil || !strings.Contains(err.Error(), "no such skill") {
		t.Errorf("an unknown skill = %v, want to be told it does not exist", err)
	}
	// A navigation command belongs to the front-end, and the daemon says so
	// rather than doing something else under the same name.
	if err := s.Local.Command("new", ""); !errors.Is(err, uisession.ErrUnknownCommand) {
		t.Errorf("/new = %v, want ErrUnknownCommand", err)
	}
	// Compaction is a turn: it starts, and a second one is refused as busy.
	if err := s.Local.Command("compact", "keep the plan"); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if err := s.Local.Command("compact", ""); !errors.Is(err, uisession.ErrBusy) {
		t.Errorf("a compaction under a running turn = %v, want ErrBusy", err)
	}
}

// "skill:greet" is not registered by name; it reaches the family handler,
// which looks the skill up when invoked and starts a turn shown as the
// command the person typed.
func TestASkillCommandStartsATurnUnderItsOwnName(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: greet\n---\nsay hello\n")
	env, _ := load(t, runner.Options{Cwd: cwd, TrustProjectSkills: true})
	reg, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	s := runner.NewSession(runner.Spec{Tools: reg, Backend: deadBackend(), Skills: env.Skills})
	t.Cleanup(s.Local.Close)
	s.HandleCommands(env.Skills, env.MCP)

	if err := s.Local.Command("skill:greet", "politely"); err != nil {
		t.Fatalf("skill:greet: %v", err)
	}
	evs, err := s.Local.Replay(0)
	if err != nil {
		t.Fatal(err)
	}
	var shown bool
	for _, ev := range evs {
		if ev.Kind == uisession.KindUserMessage && ev.Text == "/skill:greet politely" {
			shown = true
		}
	}
	if !shown {
		t.Errorf("no user line carrying the command, events: %+v", evs)
	}
}

// A skill only the model may invoke is not a command a person can run.
func TestAModelOnlySkillIsNotACommand(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "quiet",
		"---\nname: quiet\ndescription: quiet\nuser-invocable: false\n---\nnothing\n")
	env, _ := load(t, runner.Options{Cwd: cwd, TrustProjectSkills: true})
	reg, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	s := runner.NewSession(runner.Spec{Tools: reg, Backend: deadBackend(), Skills: env.Skills})
	t.Cleanup(s.Local.Close)
	s.HandleCommands(env.Skills, env.MCP)

	err = s.Local.Command("skill:quiet", "")
	if err == nil || !strings.Contains(err.Error(), "no such skill") {
		t.Errorf("a model-only skill = %v, want to be refused as no such skill", err)
	}
}
