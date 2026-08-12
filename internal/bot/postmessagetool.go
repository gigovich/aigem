package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/gigovich/aigem/internal/tools"
)

type postMessageTool struct {
	w      ThreadWriter
	reader ThreadReader
	local  *LocalDelivery
}

// NewPostMessageTool lets the bot write into a thread it is in, or open a new
// one. Not confirm-gated: participation is the safety boundary - the bot can
// only write where it belongs, and opening a thread names who will see it.
func NewPostMessageTool(w ThreadWriter, reader ThreadReader, local *LocalDelivery) tools.Tool {
	return &postMessageTool{w: w, reader: reader, local: local}
}

func (t *postMessageTool) Name() string       { return "post_message" }
func (t *postMessageTool) NeedsConfirm() bool { return false }

func (t *postMessageTool) Description() string {
	return "Post a message into a thread, or open a new one. A thread is a task or a " +
		"conversation with an explicit set of participants; posting into it wakes every bot in " +
		"it. Args: thread (the thread id to post into), or - to start a conversation - " +
		"participants (names: bot names and/or \"operator\") plus title. text is required. " +
		"Set await_reply when your message is a question or a decision the operator must " +
		"answer: it puts the thread at the top of their inbox and marks it as waiting for them. " +
		"Do not set it for progress notes; a thread that asks for attention it does not need " +
		"trains them to ignore the marker. Use this to deliver a scheduled run's result: pass " +
		"the thread id you recorded in the job's prompt. Pass mentions (names) to address the " +
		"message to particular people: they are woken now, and everyone else in the thread reads " +
		"it when they next look. When you are answering someone live, do not use this at all - " +
		"your turn's answer is posted into the thread you were woken in."
}

func (t *postMessageTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"thread":{"type":"string","description":"id of a thread you are a participant in"},
			"participants":{"type":"array","items":{"type":"string"},
				"description":"to open a new thread: bot names and/or \"operator\""},
			"title":{"type":"string","description":"short title for a new thread"},
			"text":{"type":"string","description":"the message to post"},
			"await_reply":{"type":"boolean","description":"true when the operator must answer"},
			"mentions":{"type":"array","items":{"type":"string"},
				"description":"names to address the message to; they are woken now"}
		},
		"required":["text"]
	}`)
}

func (t *postMessageTool) Run(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	var a struct {
		Thread       string   `json:"thread"`
		Participants []string `json:"participants"`
		Title        string   `json:"title"`
		Text         string   `json:"text"`
		AwaitReply   bool     `json:"await_reply"`
		Mentions     []string `json:"mentions"`
	}
	if err := json.Unmarshal(rawArgs, &a); err != nil {
		return "", err
	}
	text := strings.TrimSpace(a.Text)
	if text == "" {
		return "", fmt.Errorf("text is required")
	}
	mentions, err := t.actors(a.Mentions)
	if err != nil {
		return "", err
	}
	opts := SayOpts{AwaitReply: a.AwaitReply, Mentions: mentions}

	if thread := strings.TrimSpace(a.Thread); thread != "" {
		if _, err := t.w.Say(ctx, ThreadID(thread), text, opts); err != nil {
			return "", err
		}
		return "posted in thread " + thread, nil
	}
	if len(a.Participants) == 0 {
		// A bare "specify a thread" leaves the model guessing at an id. Its own
		// threads are the answer, so hand them over.
		return "", fmt.Errorf("%s\n\nyour threads:\n%s",
			"name a thread to post into, or participants to open a new one",
			t.threadList(ctx))
	}
	actors, err := t.actors(a.Participants)
	if err != nil {
		return "", err
	}
	// A thread with no person in it is one the operator will never see. handoff
	// puts them in for exactly this reason, and there is no case where a bot
	// should open a conversation nobody can read.
	if op := t.w.ActorFor("operator"); op != "" && !slices.Contains(actors, op) {
		actors = append(actors, op)
	}
	title := strings.TrimSpace(a.Title)
	if title == "" {
		title = firstLine(text)
	}
	thread, err := t.w.Open(ctx, title, actors, text)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("opened thread %s with %s", thread, strings.Join(a.Participants, ", ")), nil
}

// actors resolves names to ids, refusing a name nothing answers to rather than
// opening a thread that reaches nobody.
func (t *postMessageTool) actors(names []string) ([]ThreadActor, error) {
	out := make([]ThreadActor, 0, len(names))
	for _, name := range names {
		id := t.w.ActorFor(name)
		if id == "" {
			return nil, fmt.Errorf("no such teammate %q; use team_status to see who there is", name)
		}
		out = append(out, id)
	}
	return out, nil
}

func (t *postMessageTool) threadList(ctx context.Context) string {
	if t.reader == nil {
		return "(unavailable)"
	}
	list, err := t.reader.Threads(ctx, "", 20)
	if err != nil {
		return "(unavailable: " + err.Error() + ")"
	}
	return list
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 80
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}
