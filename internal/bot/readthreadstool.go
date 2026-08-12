package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gigovich/aigem/internal/tools"
)

type readThreadsTool struct{ reader ThreadReader }

// NewReadThreadsTool lets the bot read the conversations it is in but was not
// woken in.
//
// It replaces the ambient channel history the transport used to prepend to
// every turn. That was free and half-seen; this is asked for, scoped to the
// bot's own threads, and searchable by content rather than bounded by a
// twenty-minute ring. The cost is that a bot now has to think to look, which is
// why the description says so twice.
func NewReadThreadsTool(reader ThreadReader) tools.Tool {
	return &readThreadsTool{reader: reader}
}

func (t *readThreadsTool) Name() string       { return "read_threads" }
func (t *readThreadsTool) NeedsConfirm() bool { return false }

func (t *readThreadsTool) Description() string {
	return "Read conversations you are in but were not woken in. Actions: list (your threads, " +
		"newest first, with each one's state and last message; optionally filtered by state: " +
		"needs_you, working, waiting, idle), read (one whole thread, given its id), search " +
		"(full-text over the messages in your threads). Use it before you answer a question " +
		"about another conversation, and before you repeat a handoff or a question - the answer " +
		"you are waiting for may already be there. Never ask a person to quote a message you can " +
		"read yourself."
}

func (t *readThreadsTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"action":{"type":"string","enum":["list","read","search"]},
			"thread":{"type":"string","description":"for read: the thread id"},
			"query":{"type":"string","description":"for search: the words to look for"},
			"state":{"type":"string","description":"for list: only threads in this state"},
			"limit":{"type":"integer","description":"how many to return"}
		},
		"required":["action"]
	}`)
}

func (t *readThreadsTool) Run(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	var a struct {
		Action string `json:"action"`
		Thread string `json:"thread"`
		Query  string `json:"query"`
		State  string `json:"state"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(rawArgs, &a); err != nil {
		return "", err
	}
	switch a.Action {
	case "list":
		return t.reader.Threads(ctx, strings.TrimSpace(a.State), a.Limit)
	case "read":
		thread := strings.TrimSpace(a.Thread)
		if !validThreadID(thread) {
			// Ids come from listing, so say that rather than "not found": a model
			// told only that something is missing will try to work around it.
			return "", fmt.Errorf(
				"%q is not a thread id; run this tool with action \"list\" and use one from there",
				thread)
		}
		return t.reader.ThreadText(ctx, ThreadID(thread))
	case "search":
		query := strings.TrimSpace(a.Query)
		if query == "" {
			return "", fmt.Errorf("query is required for action \"search\"")
		}
		return t.reader.Search(ctx, query, a.Limit)
	default:
		return "", fmt.Errorf("unknown action %q; use list, read or search", a.Action)
	}
}

// validThreadID checks the shape the store hands out, so an invented id is
// caught here rather than looking like an empty thread.
func validThreadID(id string) bool {
	rest, ok := strings.CutPrefix(id, "t_")
	if !ok || len(rest) != 16 {
		return false
	}
	for _, r := range rest {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}
