package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func runPost(t *testing.T, f *fakeWriter, args string) (string, error) {
	t.Helper()
	return NewPostMessageTool(f, f, nil).Run(context.Background(), json.RawMessage(args))
}

func TestPostMessageWritesIntoAThread(t *testing.T) {
	f := newFakeWriter("t_0102030405060708")

	out, err := runPost(t, f, `{"thread":"t_0102030405060708","text":"reproduced"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "t_0102030405060708") {
		t.Fatalf("result does not say where it went: %q", out)
	}
	got := f.lastSaid()
	if got.Thread != "t_0102030405060708" || got.Text != "reproduced" {
		t.Fatalf("said %+v", got)
	}
	if got.Opts.AwaitReply {
		t.Fatal("a plain post asked for the operator's attention")
	}
}

// The flag is what puts a thread at the top of the inbox with the accent, so it
// has to reach the store rather than be inferred later.
func TestPostMessageCarriesAwaitReply(t *testing.T) {
	f := newFakeWriter("t_0102030405060708")

	if _, err := runPost(t, f,
		`{"thread":"t_0102030405060708","text":"which token?","await_reply":true}`); err != nil {
		t.Fatal(err)
	}
	if !f.lastSaid().Opts.AwaitReply {
		t.Fatal("await_reply did not reach the store")
	}
}

func TestPostMessageOpensAThreadFromParticipants(t *testing.T) {
	f := newFakeWriter()

	out, err := runPost(t, f,
		`{"participants":["demetre","operator"],"title":"QA on the settings pane","text":"ready for you"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.opened) != 1 {
		t.Fatalf("opened %d threads", len(f.opened))
	}
	got := f.opened[0]
	if got.Title != "QA on the settings pane" || got.Text != "ready for you" {
		t.Fatalf("opened %+v", got)
	}
	// Names are resolved to actor ids, so a thread cannot be opened with a
	// participant the store does not know.
	if len(got.Participants) != 2 || got.Participants[0] != "bot:demetre" ||
		got.Participants[1] != "human:operator" {
		t.Fatalf("participants = %v", got.Participants)
	}
	if !strings.Contains(out, "opened thread") {
		t.Fatalf("result does not say a thread was opened: %q", out)
	}
}

// A thread opened with a participant nothing answers to reaches nobody, and the
// model has no way to notice.
func TestPostMessageRefusesAnUnknownParticipant(t *testing.T) {
	f := newFakeWriter()

	_, err := runPost(t, f, `{"participants":["ghost"],"text":"anyone there"}`)
	if err == nil {
		t.Fatal("a thread was opened with a teammate that does not exist")
	}
	if !strings.Contains(err.Error(), "team_status") {
		t.Fatalf("error does not say how to find who there is: %v", err)
	}
	if len(f.opened) != 0 {
		t.Fatal("the thread was opened anyway")
	}
}

// A bare "specify a thread" leaves the model guessing at an id, so the refusal
// carries the answer.
func TestPostMessageWithNoTargetListsTheBotsThreads(t *testing.T) {
	f := newFakeWriter()
	f.digest = "t_0102030405060708  [idle]  retries"

	_, err := runPost(t, f, `{"text":"where do I put this"}`)
	if err == nil {
		t.Fatal("a post with no target was accepted")
	}
	if !strings.Contains(err.Error(), f.digest) {
		t.Fatalf("error does not list the bot's threads: %v", err)
	}
}

func TestPostMessageNeedsText(t *testing.T) {
	f := newFakeWriter("t_0102030405060708")
	if _, err := runPost(t, f, `{"thread":"t_0102030405060708","text":"   "}`); err == nil {
		t.Fatal("an empty message was accepted")
	}
}

// Participation is the boundary, and the tool must not paper over a refusal.
func TestPostMessageSurfacesARefusedThread(t *testing.T) {
	f := newFakeWriter()
	if _, err := runPost(t, f, `{"thread":"t_0102030405060708","text":"not mine"}`); err == nil {
		t.Fatal("posting into a thread the bot is not in reported success")
	}
}

// A new thread with no title still needs one a human can scan.
func TestPostMessageTitlesANewThreadFromItsText(t *testing.T) {
	f := newFakeWriter()
	if _, err := runPost(t, f,
		`{"participants":["demetre"],"text":"the rotation drops sessions\nmore detail below"}`); err != nil {
		t.Fatal(err)
	}
	if f.opened[0].Title != "the rotation drops sessions" {
		t.Fatalf("title = %q, want the first line of the text", f.opened[0].Title)
	}
}
