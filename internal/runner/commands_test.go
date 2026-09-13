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
