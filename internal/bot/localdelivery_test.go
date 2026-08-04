package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// idPoster is a fakePoster that can report post ids, which is what enables local
// delivery.
type idPoster struct {
	fakePoster
	nextID string
	// extraIDs stands in for the further posts a message too long for one post is split into.
	extraIDs []string
}

func (p *idPoster) PostWithIDs(channelID, rootID, text string) ([]string, error) {
	if rootID != "" {
		if err := p.PostToThread(channelID, rootID, text); err != nil {
			return nil, err
		}
	} else if err := p.Post(channelID, text); err != nil {
		return nil, err
	}
	return append([]string{p.nextID}, p.extraIDs...), nil
}

func fleetWith(t *testing.T, names ...string) (*Fleet, map[string]*Runtime) {
	t.Helper()
	f := NewFleet()
	rts := map[string]*Runtime{}
	for _, n := range names {
		rt, _ := fleetRuntime(t)
		rts[n] = rt
		f.Register(Member{Name: n, Username: n, Role: "tester", Runtime: rt,
			Resolver: fakeResolver{ids: map[string]string{"Tasks": "c-tasks", "@jane": "c-dm"}}})
	}
	return f, rts
}

func TestHandoffDeliversLocallyAndStillPostsToChat(t *testing.T) {
	f, rts := fleetWith(t, "jane")
	poster := &idPoster{nextID: "post-1"}
	local := &LocalDelivery{Self: "amiran", SelfUserID: "amiran-id", Fleet: f}
	tool := NewHandoffTool(poster, fakeResolver{ids: map[string]string{"Tasks": "c-tasks"}}, local)

	out, err := tool.Run(context.Background(), json.RawMessage(`{"to":"jane","summary":"review #7"}`))
	if err != nil {
		t.Fatal(err)
	}
	// The chat record is not optional: it is what a human reads.
	if poster.calls != 1 || poster.channel != "c-tasks" || !strings.Contains(poster.text, "@jane") {
		t.Fatalf("handoff was not written to chat: %+v", poster.fakePoster)
	}
	if !strings.Contains(out, "woke them directly") {
		t.Fatalf("tool result does not report local delivery: %q", out)
	}
	select {
	case got := <-rts["jane"].enqueued:
		if got.PostID != "post-1" {
			t.Fatalf("delivered post id = %q, want post-1", got.PostID)
		}
		if got.Author != "amiran-id" {
			t.Fatalf("author = %q, want the sending bot's user id", got.Author)
		}
		// A new root post is its own thread, so the reply lands in it.
		if got.Thread.RootID != "post-1" {
			t.Fatalf("thread root = %q, want the new post", got.Thread.RootID)
		}
	case <-time.After(time.Second):
		t.Fatal("the teammate was never woken in-process")
	}
}

func TestHandoffToNonLocalTeammateUsesChatOnly(t *testing.T) {
	f, _ := fleetWith(t, "jane")
	poster := &idPoster{nextID: "post-2"}
	local := &LocalDelivery{Self: "amiran", SelfUserID: "amiran-id", Fleet: f}
	tool := NewHandoffTool(poster, fakeResolver{ids: map[string]string{"Tasks": "c-tasks"}}, local)

	out, err := tool.Run(context.Background(), json.RawMessage(`{"to":"someone-else","summary":"look"}`))
	if err != nil {
		t.Fatal(err)
	}
	if poster.calls != 1 {
		t.Fatalf("chat post count = %d, want 1", poster.calls)
	}
	if strings.Contains(out, "woke them directly") {
		t.Fatalf("claimed local delivery for a bot that is not in this process: %q", out)
	}
}

func TestHandoffReportsABusyTeammate(t *testing.T) {
	f, rts := fleetWith(t, "jane")
	done := rts["jane"].EnterTurn()
	defer done()
	poster := &idPoster{nextID: "post-3"}
	local := &LocalDelivery{Self: "amiran", SelfUserID: "amiran-id", Fleet: f}
	tool := NewHandoffTool(poster, fakeResolver{ids: map[string]string{"Tasks": "c-tasks"}}, local)

	out, err := tool.Run(context.Background(), json.RawMessage(`{"to":"jane","summary":"review #7"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "mid-turn") {
		t.Fatalf("a busy teammate must be reported so the caller does not ping again: %q", out)
	}
}

func TestPostMessageDeliversDirectMessagesLocally(t *testing.T) {
	f, rts := fleetWith(t, "jane")
	poster := &idPoster{nextID: "post-4"}
	local := &LocalDelivery{Self: "amiran", SelfUserID: "amiran-id", Fleet: f}
	tool := NewPostMessageTool(poster, fakeResolver{ids: map[string]string{"@jane": "c-dm"}}, local)

	if _, err := tool.Run(context.Background(), json.RawMessage(`{"channel":"@jane","text":"ready?"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-rts["jane"].enqueued:
		if got.Text != "ready?" {
			t.Fatalf("delivered text = %q", got.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("a direct message to a local teammate was not delivered in-process")
	}
}

func TestPostMessageToAChannelIsChatOnly(t *testing.T) {
	f, rts := fleetWith(t, "jane")
	poster := &idPoster{nextID: "post-5"}
	local := &LocalDelivery{Self: "amiran", SelfUserID: "amiran-id", Fleet: f}
	tool := NewPostMessageTool(poster, fakeResolver{ids: map[string]string{"Tasks": "c-tasks"}}, local)

	if _, err := tool.Run(context.Background(),
		json.RawMessage(`{"channel":"Tasks","text":"@jane can you look"}`)); err != nil {
		t.Fatal(err)
	}
	// Only a direct message names one recipient; a channel post reaches whoever it
	// mentions through chat, exactly as before.
	select {
	case <-rts["jane"].enqueued:
		t.Fatal("a channel post must not be delivered in-process")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestLocalDeliveryNeverTargetsItself(t *testing.T) {
	f, _ := fleetWith(t, "amiran")
	local := &LocalDelivery{Self: "amiran", SelfUserID: "amiran-id", Fleet: f}
	if got := local.target("@amiran"); got != "" {
		t.Fatalf("target(self) = %q, want empty", got)
	}
	if got := local.target("Amiran"); got != "" {
		t.Fatal("self-delivery must be rejected regardless of case")
	}
	var nilDelivery *LocalDelivery
	if nilDelivery.target("jane") != "" || nilDelivery.busyNote("jane") != "" {
		t.Fatal("a nil LocalDelivery must disable local delivery entirely")
	}
}

func TestPostAndDeliverFallsBackWhenPosterCannotReportIDs(t *testing.T) {
	f, rts := fleetWith(t, "jane")
	// fakePoster has no PostWithID, so there is no id to deduplicate on and local
	// delivery must be skipped rather than risk the teammate acting twice.
	poster := &fakePoster{}
	local := &LocalDelivery{Self: "amiran", SelfUserID: "amiran-id", Fleet: f}
	delivered, err := postAndDeliver(context.Background(), poster, local, "jane", "Tasks", "c-tasks",
		"", false, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if delivered {
		t.Fatal("delivered in-process with no post id to deduplicate on")
	}
	if poster.calls != 1 {
		t.Fatalf("chat post count = %d, want 1", poster.calls)
	}
	select {
	case <-rts["jane"].enqueued:
		t.Fatal("delivered in-process without a post id to deduplicate on")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestDirectMessageDeliveryMatchesTheWebsocketCopy pins the two paths together. The transport
// classifies a direct message as "dm" and leaves it at conversation root (see classifyPost); if
// the in-process copy opened a thread instead, the same conversation would be split across two
// per-thread agents and the reply would land in a different place depending on which copy of the
// message won the race.
func TestDirectMessageDeliveryMatchesTheWebsocketCopy(t *testing.T) {
	f, rts := fleetWith(t, "jane")
	poster := &idPoster{nextID: "post-dm"}
	local := &LocalDelivery{Self: "amiran", SelfUserID: "amiran-id", Fleet: f}
	tool := NewPostMessageTool(poster, fakeResolver{ids: map[string]string{"@jane": "c-dm"}}, local)

	if _, err := tool.Run(context.Background(), json.RawMessage(`{"channel":"@jane","text":"ready?"}`)); err != nil {
		t.Fatal(err)
	}
	got := <-rts["jane"].enqueued
	if got.Kind != "dm" {
		t.Errorf("kind = %q, want dm", got.Kind)
	}
	if got.Thread.RootID != "" {
		t.Errorf("thread root = %q, want empty: a direct message sits at conversation root", got.Thread.RootID)
	}
}

// TestChunkedHandoffWakesTheTeammateOnce covers a message too long for one chat post. Every chunk
// comes back over the websocket with its own id; without marking them, chunks 2..n look like new
// messages and wake the teammate again on a partial copy of what they already have.
func TestChunkedHandoffWakesTheTeammateOnce(t *testing.T) {
	f, rts := fleetWith(t, "jane")
	poster := &idPoster{nextID: "chunk-1", extraIDs: []string{"chunk-2", "chunk-3"}}
	local := &LocalDelivery{Self: "amiran", SelfUserID: "amiran-id", Fleet: f}
	tool := NewHandoffTool(poster, fakeResolver{ids: map[string]string{"Tasks": "c-tasks"}}, local)

	if _, err := tool.Run(context.Background(),
		json.RawMessage(`{"to":"jane","summary":"a very long handoff"}`)); err != nil {
		t.Fatal(err)
	}
	if got := <-rts["jane"].enqueued; got.PostID != "chunk-1" {
		t.Fatalf("delivered post id = %q, want the first chunk", got.PostID)
	}
	for _, id := range []string{"chunk-2", "chunk-3"} {
		if !rts["jane"].alreadyRouted(id) {
			t.Errorf("chunk %s was not marked as handled; its chat copy would run a second turn", id)
		}
	}
}

func TestFleetResolvesTeammateNamesCaseInsensitively(t *testing.T) {
	f, _ := fleetWith(t, "kate")
	// Chat usernames are case-insensitive, so "@Kate" must reach the bot registered as "kate".
	if got := f.Resolve("Kate"); got != "kate" {
		t.Fatalf("Resolve(%q) = %q, want kate", "Kate", got)
	}
	if f.Resolve("nobody") != "" {
		t.Fatal("a name no bot in this process answers to must not resolve")
	}
}

// TestLocalDeliveryStaysInsideChatMembership is the security property the in-process path must
// not break. Over the websocket a bot only ever sees posts in channels it belongs to. The sender
// picks the recipient and the text itself, and the woken message is handed into the recipient's
// running turn, so without this check one bot could make another act on arbitrary text in any
// channel the SENDER can post to - a strictly larger set than chat allows.
func TestLocalDeliveryStaysInsideChatMembership(t *testing.T) {
	f := NewFleet()
	rt, _ := fleetRuntime(t)
	// jane belongs to Tasks only.
	f.Register(Member{Name: "jane", Username: "jane", Role: "tester", Runtime: rt,
		Resolver: fakeResolver{ids: map[string]string{"Tasks": "c-tasks"}}})
	local := &LocalDelivery{Self: "amiran", SelfUserID: "amiran-id", Fleet: f}
	poster := &idPoster{nextID: "post-x"}

	// amiran posts in Secret, a channel amiran belongs to and jane does not.
	delivered, err := postAndDeliver(context.Background(), poster, local, "jane", "Secret",
		"c-secret", "", false, "@jane do this")
	if err != nil {
		t.Fatal(err)
	}
	if delivered {
		t.Fatal("woke a teammate in a channel they do not belong to")
	}
	select {
	case got := <-rt.enqueued:
		t.Fatalf("message reached a non-member: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}

	// The same handoff in a channel jane is in goes through.
	if delivered, err = postAndDeliver(context.Background(), poster, local, "jane", "Tasks",
		"c-tasks", "", false, "@jane do this"); err != nil {
		t.Fatal(err)
	}
	if !delivered {
		t.Fatal("a handoff in a shared channel was not delivered")
	}
}

// TestLocalDeliveryMatchesOnChatUsername covers the misdelivery a name collision would cause:
// the caller addressed a chat account, so a bot whose aigem name happens to match some person's
// username must not receive that person's direct messages.
func TestLocalDeliveryMatchesOnChatUsername(t *testing.T) {
	f := NewFleet()
	rt, _ := fleetRuntime(t)
	f.Register(Member{Name: "kate", Username: "kate-bot", Role: "architect", Runtime: rt})
	local := &LocalDelivery{Self: "amiran", SelfUserID: "amiran-id", Fleet: f}

	if got := local.target("@kate"); got != "" {
		t.Fatalf("target(@kate) = %q; that is a person's username, not this bot's", got)
	}
	if got := local.target("@kate-bot"); got != "kate" {
		t.Fatalf("target(@kate-bot) = %q, want the bot registered under that account", got)
	}
}

// TestChunksAreNotSuppressedWhenDeliveryIsRefused: marking the trailing chunks as handled after a
// refused delivery would suppress the chat copies that are then the recipient's only path.
func TestChunksAreNotSuppressedWhenDeliveryIsRefused(t *testing.T) {
	f := NewFleet()
	rt, _ := fleetRuntime(t)
	f.Register(Member{Name: "jane", Username: "jane", Role: "tester", Runtime: rt,
		Resolver: fakeResolver{ids: map[string]string{"Tasks": "c-tasks"}}})
	local := &LocalDelivery{Self: "amiran", SelfUserID: "amiran-id", Fleet: f}
	poster := &idPoster{nextID: "chunk-1", extraIDs: []string{"chunk-2"}}

	if _, err := postAndDeliver(context.Background(), poster, local, "jane", "Secret", "c-secret",
		"", false, "long text"); err != nil {
		t.Fatal(err)
	}
	if rt.alreadyRouted("chunk-2") {
		t.Fatal("a chunk was marked handled although the teammate was never given the message")
	}
}
