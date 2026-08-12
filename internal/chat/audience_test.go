package chat

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// Entitlement travels on the frame, so a frame published with no audience is
// invisible to everyone - silently. This walks every write that produces one.
func TestEveryPublishedFrameCarriesItsAudience(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()

	var got []Frame
	s.AddPublisher(func(f []Frame) { got = append(got, f...) })

	th, err := s.NewThread(ctx, "retries", Operator, []string{amiran})
	if err != nil {
		t.Fatal(err)
	}
	att := mustAttach(t, s, th.ID, "shot.png")
	if _, err := s.Say(ctx, th.ID, Draft{
		Author: Operator, Body: "look please", Attachments: []string{att.ID},
	}); err != nil {
		t.Fatal(err)
	}
	turn, err := s.BeginTurn(ctx, th.ID, amiran)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, EventRecord{
		Thread: th.ID, Actor: amiran, TurnSeq: turn, Kind: "tool_end",
		Payload: []byte(`{"kind":"tool_end"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.EndTurn(ctx, amiran, turn, ""); err != nil {
		t.Fatal(err)
	}
	for _, call := range []func() error{
		func() error { return s.SetTitle(ctx, Operator, th.ID, "renamed") },
		func() error { return s.SetArchived(ctx, Operator, th.ID, true) },
		func() error { return s.MarkRead(ctx, Operator, th.ID, 2) },
		func() error { return s.AddParticipant(ctx, Operator, th.ID, demetre) },
		func() error { return s.RemoveParticipant(ctx, Operator, th.ID, demetre) },
	} {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.BeginTurn(ctx, th.ID, amiran); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseStaleTurns(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread(ctx, Operator, th.ID); err != nil {
		t.Fatal(err)
	}

	if len(got) < 15 {
		t.Fatalf("only %d frames were published; the walk did not reach every write", len(got))
	}
	for i, f := range got {
		if len(f.To) == 0 {
			t.Fatalf("frame %d (%s, seq %d) has no audience and is invisible to everyone",
				i, f.Stream, f.Seq)
		}
		if !slices.Contains(f.To, Operator) {
			t.Fatalf("frame %d (%s, seq %d) is not addressed to the operator: %v",
				i, f.Stream, f.Seq, f.To)
		}
	}
}

// The actor who was just removed has to hear that they were: their client would
// otherwise keep rendering a thread it no longer has access to.
func TestARemovedParticipantHearsAboutIt(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran, demetre)

	var got []Frame
	s.AddPublisher(func(f []Frame) { got = append(got, f...) })
	if err := s.RemoveParticipant(ctx, Operator, th.ID, demetre); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("removing a participant published nothing")
	}
	for _, f := range got {
		if !slices.Contains(f.To, demetre) {
			t.Fatalf("the %s frame was not addressed to the actor it removed: %v", f.Stream, f.To)
		}
	}
}

// The operator has no way back into a thread they left, since adding a
// participant requires being one.
func TestTheOperatorCannotBeRemovedFromTheirOwnThread(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)

	if err := s.RemoveParticipant(ctx, Operator, th.ID, Operator); err == nil {
		t.Fatal("the operator removed themselves from a thread")
	}
	if _, err := s.ThreadFor(ctx, Operator, th.ID); err != nil {
		t.Fatalf("the operator lost their thread anyway: %v", err)
	}
}

// Removing someone who is not there is not a success.
func TestRemovingANonParticipantIsAnError(t *testing.T) {
	s := newStore(t)
	th := mustThread(t, s, "retries", amiran)

	if err := s.RemoveParticipant(t.Context(), Operator, th.ID, jane); err == nil {
		t.Fatal("removing an actor that is not in the thread reported success")
	}
}

// A rename moves changed_seq and not last_seq, so a frame stamped with the
// latter would sort below the cursor a client is about to resume from - and be
// dropped by any client that dedupes on it.
func TestTailThreadFramesAreStampedWithTheChange(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	mustSay(t, s, th.ID, amiran, "reproduced")

	mark, err := s.Seq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTitle(ctx, Operator, th.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	frames, cursor, _, err := s.Tail(ctx, Operator, mark, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("the tail has %d frames, want the one rename", len(frames))
	}
	f := frames[0]
	if f.Seq <= mark {
		t.Fatalf("the rename is stamped %d, at or below the cursor %d it followed", f.Seq, mark)
	}
	if f.Seq > cursor {
		t.Fatalf("the rename is stamped %d, past the cursor %d", f.Seq, cursor)
	}
	if f.Thread == nil || f.Thread.Title != "renamed" {
		t.Fatalf("the frame does not carry the new title: %+v", f.Thread)
	}
}

// A brand-new thread has no messages, so ordering the inbox by last_seq would
// sort it to the bottom of the list it was just created from.
func TestANewThreadSortsToTheTopOfTheInbox(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	old := mustThread(t, s, "older", amiran)
	mustSay(t, s, old.ID, amiran, "with a message in it")
	fresh := mustThread(t, s, "just opened", demetre)

	got, err := s.Inbox(ctx, Operator, "", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != fresh.ID {
		t.Fatalf("the inbox leads with %q, want the thread just opened", got[0].Title)
	}
}

// Closing the hub is what ends the goroutines behind a hijacked connection,
// which http.Server.Close does not touch.
func TestHubCloseDetachesEveryone(t *testing.T) {
	hub := NewHub()
	a := attachClient(t, hub, Operator)
	b := attachClient(t, hub, amiran)

	hub.Close()
	for i, ch := range []<-chan Frame{a.Frames(), b.Frames()} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Fatalf("client %d still received a frame after the hub closed", i)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("client %d's stream did not close with the hub", i)
		}
	}
	if n := hub.Attached(); n != 0 {
		t.Fatalf("%d clients still attached after Close", n)
	}
	// And it is safe to call twice, because shutdown paths overlap.
	hub.Close()
}

// A client registered before its history is read must not lose a write that
// commits in between. The hub owns the fetch so it can order the two.
func TestAttachLosesNothingWrittenWhileTheBacklogIsRead(t *testing.T) {
	s := newStore(t)
	hub := NewHub()
	s.AddPublisher(hub.Publish)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	first := mustSay(t, s, th.ID, amiran, "before the fetch")

	// The backlog function writes while it is being called, which is the window
	// a fetch-then-register order would drop.
	var racy Message
	client, err := hub.Attach(Operator, first.Seq-1, func(from uint64) ([]Frame, error) {
		racy = mustSay(t, s, th.ID, amiran, "during the fetch")
		frames, _, _, err := s.Tail(ctx, Operator, from, 0)
		return frames, err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Detach()

	seen := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for len(seen) < 2 {
		select {
		case f := <-client.Frames():
			if f.Stream == StreamMessage && f.Message != nil {
				seen[f.Message.Body] = true
			}
		case <-deadline:
			t.Fatalf("saw %v; the write during the fetch was lost", seen)
		}
	}
	if !seen["during the fetch"] {
		t.Fatalf("the message written at seq %d during the fetch never arrived", racy.Seq)
	}
}

// The truncated signal means "there is more history below", the opposite of a
// desync's "you fell behind". A client that confused them would throw away the
// backlog it had just been given, so they are separate streams and the signal
// comes after the frames it describes.
func TestTruncatedIsNotADesyncAndComesAfterTheBacklog(t *testing.T) {
	if StreamTruncated == StreamDesync {
		t.Fatal("the two signals share a stream name")
	}
	c := &wsConn{conn: newFakeConn()}
	frames := make(chan Frame, 4)
	frames <- Frame{Seq: 1, Stream: StreamMessage}
	frames <- Frame{Seq: 2, Stream: StreamMessage}
	frames <- Frame{Seq: 9, Stream: StreamMessage}
	close(frames)
	c.pump(frames, 2)

	sent := c.conn.(*fakeConn).texts()
	var order []string
	for _, s := range sent {
		switch {
		case strings.Contains(s, `"stream":"truncated"`):
			order = append(order, "truncated")
		case strings.Contains(s, `"seq":9`):
			order = append(order, "live")
		default:
			order = append(order, "backlog")
		}
	}
	want := []string{"backlog", "backlog", "truncated", "live"}
	if !slices.Equal(order, want) {
		t.Fatalf("frames went out as %v, want %v", order, want)
	}
}
