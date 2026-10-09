package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gigovich/aigem/internal/tools"
)

// ticketDone is the tool a ticket run calls when its work is complete. It only records the
// summary; the daemon commits, checks and merges once the turn ends.
type ticketDone struct{ record func(summary string) }

func newTicketDone(record func(string)) tools.Tool { return &ticketDone{record: record} }

func (*ticketDone) Name() string       { return "ticket_done" }
func (*ticketDone) NeedsConfirm() bool { return false }

func (*ticketDone) Description() string {
	return "Call this once the ticket's work is complete, with a short summary of what changed. " +
		"When your turn ends the daemon commits the worktree, runs the repository's check and " +
		"merges into main. Do not call it when you are stuck or need a decision."
}

func (*ticketDone) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string",` +
		`"description":"What was done, in a few sentences."}},"required":["summary"]}`)
}

func (d *ticketDone) Run(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		return "", errors.New("a summary is required")
	}
	d.record(summary)
	return "Recorded. End your turn now; the daemon commits, checks and merges.", nil
}
