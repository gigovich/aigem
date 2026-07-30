package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gigovich/aigem/internal/tools"
)

// defaultHandoffChannel is where a handoff lands when the caller names none. It must be a channel
// every bot is invited to, so a scheduled run with no live thread can still reach the teammate it
// depends on; provisioning that membership is a deployment responsibility. If a bot is not a member,
// ResolveChannel returns an error that surfaces to the model instead of the handoff landing silently.
const defaultHandoffChannel = "Tasks"

// handoffTool hands an owned piece of work to a named teammate by waking them in chat. It exists
// as a distinct tool, separate from post_message, because a handoff has one non-negotiable
// requirement a free-form post does not: it MUST @mention the teammate in a channel they belong
// to, since that chat mention is the only thing that wakes another bot. Recording the same request
// on a tracker (a Gitea issue, a ticket file) notifies no one and is not a handoff.
type handoffTool struct {
	poster   Poster
	resolver ChannelResolver
}

// NewHandoffTool lets the bot delegate a dependency to a teammate. Not confirm-gated; Mattermost
// membership is the safety boundary, same as post_message.
func NewHandoffTool(poster Poster, resolver ChannelResolver) tools.Tool {
	return &handoffTool{poster: poster, resolver: resolver}
}

func (t *handoffTool) Name() string       { return "handoff" }
func (t *handoffTool) NeedsConfirm() bool { return false }

func (t *handoffTool) Description() string {
	return "Hand an owned piece of work to a teammate by waking them in chat. A chat mention is " +
		"the ONLY thing that wakes another bot; a tracker/issue comment notifies no one, so use " +
		"this - not an issue comment - whenever your progress depends on a teammate acting next. " +
		"It @mentions them so they are pulled in to act now. Args: to (required - the teammate's " +
		"chat username), summary (required - what you need them to do and what is ready), " +
		"ticket (optional - the tracker id/link for context), channel (optional - a channel you " +
		"both belong to, or \"@<to>\" to hand off in your direct conversation with them; " +
		"defaults to \"" + defaultHandoffChannel + "\"), thread (optional - a " +
		"thread root post id to hand off inside an existing thread). Works from a scheduled run, " +
		"which has no live thread: the default channel still reaches the teammate. Hand off once, " +
		"then wait for their reply instead of re-pinging."
}

func (t *handoffTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"to":{"type":"string","description":"chat username of the teammate to wake (mentioned as @username)"},
			"summary":{"type":"string","description":"what you need them to do and what is ready for them"},
			"ticket":{"type":"string","description":"optional tracker id or link for context"},
			"channel":{"type":"string","description":"optional channel you both belong to; defaults to the coordination channel"},
			"thread":{"type":"string","description":"optional thread root post id to hand off inside an existing thread"}
		},
		"required":["to","summary"]
	}`)
}

func (t *handoffTool) Run(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	var a struct {
		To      string `json:"to"`
		Summary string `json:"summary"`
		Ticket  string `json:"ticket"`
		Channel string `json:"channel"`
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
	channel := strings.TrimSpace(a.Channel)
	if channel == "" {
		channel = defaultHandoffChannel
	}
	// A DM target is only a handoff if it is the DM with the teammate themselves;
	// any other DM is a channel the teammate cannot see - a silent handoff failure.
	if strings.HasPrefix(channel, "@") && !strings.EqualFold(channel, "@"+to) {
		return "", fmt.Errorf(
			"channel %q is a direct message %s cannot see; use a shared channel, or %q to DM them",
			channel, to, "@"+to)
	}
	id, err := t.resolver.ResolveChannel(ctx, channel)
	if err != nil {
		return "", err
	}
	text := fmt.Sprintf("@%s **Handoff**", to)
	if ticket := strings.TrimSpace(a.Ticket); ticket != "" {
		text += " [" + ticket + "]"
	}
	text += ": " + summary
	if root := strings.TrimSpace(a.Thread); root != "" {
		if err := t.poster.PostToThread(id, root, text); err != nil {
			return "", err
		}
		return fmt.Sprintf("handed off to @%s in %q thread %s", to, channel, root), nil
	}
	if err := t.poster.Post(id, text); err != nil {
		return "", err
	}
	return fmt.Sprintf("handed off to @%s in %q", to, channel), nil
}
