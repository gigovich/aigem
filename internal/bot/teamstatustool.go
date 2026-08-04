package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gigovich/aigem/internal/tools"
)

// teamStatusTool reports which teammates share this process and which of them
// are working right now.
//
// The reason it exists: a bot deciding whether to chase a teammate has, until
// now, had only silence to read. Silence looks the same whether the teammate is
// halfway through the work or never got the message, so the safe-looking move is
// to ping again - and a fleet of bots pinging each other is the failure this
// whole design is meant to remove. Being able to see "she is mid-turn" turns
// that guess into a fact.
type teamStatusTool struct {
	self  string
	fleet *Fleet
}

// NewTeamStatusTool returns the roster tool, or nil when there is no roster at all. A bot running
// on its own still gets the tool, and it truthfully answers that nobody is alongside it: the tool
// exists to answer "is my teammate working?", and "there is no such teammate" is an answer.
func NewTeamStatusTool(self string, fleet *Fleet) tools.Tool {
	if fleet == nil {
		return nil
	}
	return &teamStatusTool{self: self, fleet: fleet}
}

func (t *teamStatusTool) Name() string       { return "team_status" }
func (t *teamStatusTool) NeedsConfirm() bool { return false }

func (t *teamStatusTool) Description() string {
	return "List your teammates and whether each one is working right now. Takes no arguments. " +
		"Use it before chasing someone who has not answered: a teammate shown as working has your " +
		"message and is on it, so wait instead of pinging again. It only sees bots running " +
		"alongside you, so a name missing from the list is not idle - it is somewhere else, and " +
		"chat is how you reach it."
}

func (t *teamStatusTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t *teamStatusTool) Run(_ context.Context, _ json.RawMessage) (string, error) {
	roster := t.fleet.Roster()
	var b strings.Builder
	for _, m := range roster {
		if strings.EqualFold(m.Name, t.self) {
			continue
		}
		state := "idle"
		if m.Busy {
			state = "working"
		}
		fmt.Fprintf(&b, "%s (%s): %s\n", m.Name, m.Role, state)
	}
	if b.Len() == 0 {
		return "no teammates are running alongside you", nil
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
