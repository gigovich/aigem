package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gigovich/aigem/internal/tools"
)

// handoffTool hands an owned piece of work to a named teammate by waking them.
//
// It is a distinct tool rather than a shape of post_message because a handoff
// has one non-negotiable requirement a free-form post does not: it must reach
// the teammate. Recording the same request on a tracker - a Gitea issue, a
// ticket file - notifies no one and is not a handoff.
type handoffTool struct {
	w     ThreadWriter
	local *LocalDelivery
	self  string
}

// NewHandoffTool lets the bot delegate a dependency to a teammate. Not
// confirm-gated: participation is the safety boundary, same as post_message.
// local may be nil.
func NewHandoffTool(w ThreadWriter, local *LocalDelivery, self string) tools.Tool {
	return &handoffTool{w: w, local: local, self: self}
}

func (t *handoffTool) Name() string       { return "handoff" }
func (t *handoffTool) NeedsConfirm() bool { return false }

func (t *handoffTool) Description() string {
	return "Hand an owned piece of work to a teammate. Waking them is the only thing that makes " +
		"a handoff real - a tracker comment notifies nobody - so use this, not an issue comment, " +
		"whenever your progress depends on a teammate acting next. Args: to (required - the " +
		"teammate's name), summary (required - what you need them to do and what is ready), " +
		"ticket (optional - the tracker id or link for context), thread (optional - hand off " +
		"inside this thread; the teammate is added to it if they are not already in it). With no " +
		"thread a new thread is opened with you, the teammate and the operator in it, so the work " +
		"is visible from the start. Works from a scheduled run, which has no live thread. Hand " +
		"off once, then wait for their reply instead of re-pinging."
}

func (t *handoffTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"to":{"type":"string","description":"name of the teammate to wake"},
			"summary":{"type":"string","description":"what you need them to do and what is ready"},
			"ticket":{"type":"string","description":"optional tracker id or link for context"},
			"thread":{"type":"string","description":"optional thread to hand off inside"}
		},
		"required":["to","summary"]
	}`)
}

func (t *handoffTool) Run(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	var a struct {
		To      string `json:"to"`
		Summary string `json:"summary"`
		Ticket  string `json:"ticket"`
		Thread  string `json:"thread"`
	}
	if err := json.Unmarshal(rawArgs, &a); err != nil {
		return "", err
	}
	to := strings.TrimLeft(strings.TrimSpace(a.To), "@")
	if to == "" {
		return "", fmt.Errorf("to is required: name the teammate to hand off to")
	}
	summary := strings.TrimSpace(a.Summary)
	if summary == "" {
		return "", fmt.Errorf("summary is required: state what the teammate needs to do")
	}
	actor := t.w.ActorFor(to)
	if actor == "" {
		return "", fmt.Errorf("no such teammate %q; use team_status to see who there is", to)
	}

	text := "**Handoff**"
	if ticket := strings.TrimSpace(a.Ticket); ticket != "" {
		text += " [" + ticket + "]"
	}
	text += ": " + summary

	// Read busy before delivering: afterwards the teammate may be busy precisely
	// because of this handoff, which is not what the caller needs to be told.
	note := t.local.busyNote(t.local.target(to))

	thread := ThreadID(strings.TrimSpace(a.Thread))
	if thread == "" {
		// A handoff with no thread is by definition new work. The operator is in
		// it from the start, so no bot-to-bot conversation happens out of sight.
		participants := []ThreadActor{actor}
		if me := t.w.ActorFor(t.self); me != "" {
			participants = append(participants, me)
		}
		if op := t.w.ActorFor("operator"); op != "" {
			participants = append(participants, op)
		}
		opened, err := t.w.Open(ctx, firstLine(summary), participants, "")
		if err != nil {
			return "", err
		}
		thread = opened
	} else if err := t.w.Join(ctx, thread, actor); err != nil {
		return "", err
	}

	delivered, err := sayAndDeliver(ctx, t.w, t.local, to, thread, text,
		SayOpts{Mentions: []ThreadActor{actor}})
	if err != nil {
		return "", err
	}
	if delivered {
		return fmt.Sprintf("handed off to %s in thread %s and woke them directly%s",
			to, thread, note), nil
	}
	return fmt.Sprintf("handed off to %s in thread %s%s", to, thread, note), nil
}
