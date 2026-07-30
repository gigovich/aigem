package mattermost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/bot"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

func newTestTransport() *Transport {
	t := &Transport{
		botUserID: "bot123",
		events:    make(chan bot.Inbound, 4),
		done:      make(chan struct{}),
		threads:   map[string]bool{},
		buffer:    newChannelBuffer(10, time.Hour),
		usernames: map[string]string{},
	}
	t.debounce = newThreadDebouncer(threadQuietPeriod, t.fireThreadUpdate)
	return t
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// frame builds a raw "posted" WebSocket frame. The post and mentions fields are
// double-encoded (a JSON string holding JSON), matching what Mattermost sends.
func frame(channelType, rootID, userID, msg, mentions string) []byte {
	data := map[string]json.RawMessage{
		"post":         mustJSON(postJSON("c1", rootID, userID, msg)),
		"channel_type": mustJSON(channelType),
	}
	if mentions != "" {
		data["mentions"] = mustJSON(mentions)
	}
	b, _ := json.Marshal(map[string]any{"event": "posted", "data": data})
	return b
}

func TestDispatchBuffersUnaddressed(t *testing.T) {
	tr := newTestTransport()
	tr.dispatch(frame("O", "", "u-x", "ambient noise", ""))
	got := tr.buffer.recent("c1")
	if len(got) != 1 || got[0].text != "ambient noise" || got[0].author != "u-x" {
		t.Fatalf("buffer = %+v", got)
	}
	select {
	case in := <-tr.events:
		t.Fatalf("unaddressed post should not be forwarded, got %+v", in)
	default:
	}
}

func TestDispatchForwardsMentionWithoutBuffering(t *testing.T) {
	tr := newTestTransport()
	tr.dispatch(frame("O", "", "u-x", "@amiran hi", `["bot123"]`))
	select {
	case in := <-tr.events:
		if in.Kind != "mention" {
			t.Fatalf("kind = %q", in.Kind)
		}
	default:
		t.Fatal("mention should be forwarded")
	}
	if len(tr.buffer.recent("c1")) != 0 {
		t.Fatal("addressed post must not be buffered")
	}
}

func TestDispatchIgnoresOwnPosts(t *testing.T) {
	tr := newTestTransport()
	tr.dispatch(frame("O", "", "bot123", "mine", ""))
	if len(tr.buffer.recent("c1")) != 0 {
		t.Fatal("own post must not be buffered")
	}
}

// A reply in a thread the bot is in (here, one it owns) is observed via the debouncer and not
// buffered as ambient chatter. The fired thread_update carries the thread, no text.
func TestDispatchObservesThreadReply(t *testing.T) {
	tr := newTestTransport()
	tr.threads["root1"] = true // bot owns/participates in this thread

	// Capture the debounce timer's callback instead of waiting the quiet period; fire it after
	// dispatch returns, mirroring how time.AfterFunc runs it off-goroutine (not under note's lock).
	var scheduled []func()
	tr.debounce.after = func(_ time.Duration, f func()) stoppable {
		scheduled = append(scheduled, f)
		return &fakeTimer{}
	}

	tr.dispatch(frame("O", "root1", "u-other", "@jane ready", ""))
	for _, f := range scheduled {
		f()
	}

	select {
	case in := <-tr.events:
		if in.Kind != "thread_update" || in.Thread.RootID != "root1" || in.Channel != "c1" {
			t.Fatalf("thread_update = %+v", in)
		}
		if in.Text != "" {
			t.Fatalf("thread_update should carry no text, got %q", in.Text)
		}
	default:
		t.Fatal("reply in an owned thread should fire a thread_update")
	}
	if len(tr.buffer.recent("c1")) != 0 {
		t.Fatal("observed thread reply must not be buffered as ambient chatter")
	}
}

// A reply in a thread the bot is NOT in is just ambient chatter: buffered, no thread_update.
func TestDispatchThreadReplyNotInThreadIsBuffered(t *testing.T) {
	tr := newTestTransport()
	tr.dispatch(frame("O", "root-unknown", "u-other", "chatter", ""))
	if len(tr.buffer.recent("c1")) != 1 {
		t.Fatal("reply in an untracked thread should be buffered")
	}
	select {
	case in := <-tr.events:
		t.Fatalf("untracked thread reply should not fire an event, got %+v", in)
	default:
	}
}

// Posting at root level records the new post as a thread the bot owns, so later replies to it
// are observed even without an @mention.
func TestPostTracksOwnedThread(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"new-root"}`))
	}))
	defer srv.Close()

	tr := newTestTransport()
	tr.client = NewClient(srv.URL, "tok")
	if err := tr.Post("c1", "status update"); err != nil {
		t.Fatal(err)
	}
	if !tr.inThread("new-root") {
		t.Fatal("Post should track the new root post as an owned thread")
	}
}

func TestTrackThreadEvictsOldestOverCap(t *testing.T) {
	tr := newTestTransport()
	tr.mu.Lock()
	for i := 0; i < maxTrackedThreads+5; i++ {
		tr.trackThread(fmt.Sprintf("root-%d", i))
	}
	tr.mu.Unlock()
	if len(tr.threads) != maxTrackedThreads {
		t.Fatalf("tracked %d threads, want cap %d", len(tr.threads), maxTrackedThreads)
	}
	if tr.inThread("root-0") {
		t.Fatal("oldest thread should have been evicted")
	}
	if !tr.inThread(fmt.Sprintf("root-%d", maxTrackedThreads+4)) {
		t.Fatal("newest thread should still be tracked")
	}
}

func TestHistoryFormatsAndCachesUsernames(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`[{"id":"u1","username":"alice"},{"id":"u2","username":"bob"}]`))
	}))
	defer srv.Close()

	tr := newTestTransport()
	tr.client = NewClient(srv.URL, "tok")
	tr.buffer.add("c1", "u1", "hello")
	tr.buffer.add("c1", "u2", "world")

	want := "alice: hello\nbob: world"
	if got := tr.History(context.Background(), "c1"); got != want {
		t.Fatalf("History = %q, want %q", got, want)
	}
	// second call must hit the username cache, not the server again
	_ = tr.History(context.Background(), "c1")
	if calls != 1 {
		t.Fatalf("username lookups = %d, want 1 (cached)", calls)
	}
	if got := tr.History(context.Background(), "empty"); got != "" {
		t.Fatalf("empty channel History = %q", got)
	}
}

func TestTypingPayload(t *testing.T) {
	got, err := typingPayload(2, bot.ThreadRef{ChannelID: "c1", RootID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Action string `json:"action"`
		Seq    int64  `json:"seq"`
		Data   struct {
			ChannelID string `json:"channel_id"`
			ParentID  string `json:"parent_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatal(err)
	}
	if m.Action != "user_typing" || m.Seq != 2 || m.Data.ChannelID != "c1" || m.Data.ParentID != "r1" {
		t.Fatalf("payload = %s", got)
	}
}

func TestParseAuthFrame(t *testing.T) {
	cases := []struct {
		name    string
		frame   string
		wantOK  bool
		wantErr bool
	}{
		{"fail", `{"status":"FAIL","seq_reply":1,"error":{"message":"token invalid","status_code":401}}`, false, true},
		{"ok", `{"status":"OK","seq_reply":1}`, true, false},
		{"hello", `{"event":"hello","data":{}}`, true, false},
		{"other", `{"event":"status_change","data":{}}`, false, false},
		{"garbage", `not json`, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, err := parseAuthFrame([]byte(c.frame))
			if ok != c.wantOK || (err != nil) != c.wantErr {
				t.Fatalf("parseAuthFrame(%s) = ok %v err %v, want ok %v err %v",
					c.frame, ok, err, c.wantOK, c.wantErr)
			}
		})
	}
}

func TestAwaitAuthRejectsBadToken(t *testing.T) {
	client, server := net.Pipe()
	tr := newTestTransport()
	tr.conn, tr.rw = client, client
	go func() {
		fail := `{"status":"FAIL","seq_reply":1,"error":{"message":"token invalid","status_code":401}}`
		_ = wsutil.WriteServerText(server, []byte(fail))
	}()
	err := tr.awaitAuth(time.Second)
	_ = server.Close()
	if err == nil {
		t.Fatal("expected auth error for rejected token")
	}
}

func TestAwaitAuthAcceptsHello(t *testing.T) {
	client, server := net.Pipe()
	tr := newTestTransport()
	tr.conn, tr.rw = client, client
	go func() { _ = wsutil.WriteServerText(server, []byte(`{"event":"hello","data":{}}`)) }()
	if err := tr.awaitAuth(time.Second); err != nil {
		t.Fatalf("hello should authenticate: %v", err)
	}
	_ = server.Close()
}

func TestAwaitAuthFailsOnClosedConn(t *testing.T) {
	client, server := net.Pipe()
	tr := newTestTransport()
	tr.conn, tr.rw = client, client
	_ = server.Close()
	if err := tr.awaitAuth(time.Second); err == nil {
		t.Fatal("expected error when server closes before auth confirmation")
	}
}

func TestTypingWritesFrame(t *testing.T) {
	client, server := net.Pipe()
	tr := newTestTransport()
	tr.conn = client
	tr.seq.Store(1)

	go func() { io.Copy(io.Discard, server) }()

	if err := tr.Typing(bot.ThreadRef{ChannelID: "c1", RootID: "r1"}); err != nil {
		t.Fatalf("Typing: %v", err)
	}
	_ = client.Close()
	_ = server.Close()
}

// errReader fails every Read, so readServerText returns an error immediately - the same signal
// consume() sees when a live WebSocket drops.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }

func errRW() io.ReadWriter { return readWriter{errReader{}, io.Discard} }

func TestStopping(t *testing.T) {
	tr := newTestTransport()
	if tr.stopping(context.Background()) {
		t.Fatal("fresh transport reported stopping")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !tr.stopping(ctx) {
		t.Fatal("canceled ctx should report stopping")
	}
	tr2 := newTestTransport()
	close(tr2.done)
	if !tr2.stopping(context.Background()) {
		t.Fatal("closed done should report stopping")
	}
}

func TestReconnectStopsWhenClosing(t *testing.T) {
	tr := newTestTransport()
	called := 0
	tr.connectFn = func(context.Context) error { called++; return nil }
	close(tr.done) // simulate Close()
	if tr.reconnect(context.Background()) {
		t.Fatal("reconnect should return false while shutting down")
	}
	if called != 0 {
		t.Fatalf("connectFn must not be called when stopping, got %d", called)
	}
}

func TestReconnectRetriesUntilSuccess(t *testing.T) {
	tr := newTestTransport()
	called := 0
	tr.connectFn = func(context.Context) error {
		called++
		if called < 2 {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	if !tr.reconnect(context.Background()) {
		t.Fatal("reconnect should return true after a successful dial")
	}
	if called != 2 {
		t.Fatalf("connectFn calls = %d, want 2", called)
	}
}

// TestRunReconnectsThenClosesEvents is the regression for the silent-death bug: a dropped
// connection must trigger a reconnect (not exit), and events must close only once the transport
// is shutting down.
func TestRunReconnectsThenClosesEvents(t *testing.T) {
	tr := newTestTransport()
	tr.rw = errRW() // first consume() returns immediately, as if the socket dropped
	reconnected := make(chan struct{}, 1)
	tr.connectFn = func(context.Context) error {
		tr.rw = errRW()
		reconnected <- struct{}{}
		close(tr.done) // next stopping() is true, so run() exits after this reconnect
		return nil
	}
	go tr.run(context.Background())

	select {
	case <-reconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not attempt to reconnect after the drop")
	}
	select {
	case _, ok := <-tr.events:
		if ok {
			t.Fatal("events channel should be closed once stopping")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not close events after stopping")
	}
}

func TestReadServerTextReadsTextFrame(t *testing.T) {
	client, server := net.Pipe()
	tr := newTestTransport()
	tr.conn, tr.rw = client, client
	go func() { _ = wsutil.WriteServerText(server, []byte(`{"event":"hello"}`)) }()
	data, err := tr.readServerText()
	_ = server.Close()
	if err != nil {
		t.Fatalf("readServerText: %v", err)
	}
	if string(data) != `{"event":"hello"}` {
		t.Fatalf("payload = %q", data)
	}
}

// TestReadServerTextAnswersPing verifies a server Ping is answered with a Pong (written under
// writeMu via the guarded control handler) and the following text frame is still returned.
func TestReadServerTextAnswersPing(t *testing.T) {
	client, server := net.Pipe()
	tr := newTestTransport()
	tr.conn, tr.rw = client, client
	go func() {
		_ = wsutil.WriteServerMessage(server, ws.OpPing, []byte("hi"))
		_ = wsutil.WriteServerText(server, []byte(`{"event":"hello"}`))
	}()
	// Read raw client->server frames so the Pong control frame is observable (ReadClientData
	// would silently absorb it). The Pong is the only frame the client writes here.
	pong := make(chan ws.OpCode, 1)
	go func() {
		for {
			hdr, err := ws.ReadHeader(server)
			if err != nil {
				return
			}
			if hdr.Length > 0 {
				_, _ = io.CopyN(io.Discard, server, hdr.Length)
			}
			if hdr.OpCode == ws.OpPong {
				pong <- hdr.OpCode
				return
			}
		}
	}()
	data, err := tr.readServerText()
	if err != nil {
		t.Fatalf("readServerText: %v", err)
	}
	if string(data) != `{"event":"hello"}` {
		t.Fatalf("payload = %q", data)
	}
	select {
	case op := <-pong:
		if op != ws.OpPong {
			t.Fatalf("expected client Pong, got op %v", op)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no Pong written in response to server Ping")
	}
	_ = server.Close()
}

func TestSeededThreadsAreObservedAfterRestart(t *testing.T) {
	// A restart used to wipe thread membership, so every existing thread silently went
	// mention-only - which looked exactly like the bot ignoring its own conversations.
	tr := newTestTransport()
	tr.SeedThreads([]string{"root-from-last-run"})
	var scheduled []func()
	tr.debounce.after = func(_ time.Duration, f func()) stoppable {
		scheduled = append(scheduled, f)
		return &fakeTimer{}
	}
	tr.dispatch(frame("O", "root-from-last-run", "u-x", "plain reply, no mention", ""))
	for _, f := range scheduled {
		f()
	}
	select {
	case in := <-tr.events:
		if in.Kind != "thread_update" || in.Thread.RootID != "root-from-last-run" {
			t.Fatalf("inbound = %+v", in)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a reply in a seeded thread should arrive as a thread_update")
	}
}

func TestThreadSinkPersistsNewThreadsOnly(t *testing.T) {
	tr := newTestTransport()
	var saves [][]string
	var seqs []uint64
	tr.SetThreadSink(func(seq uint64, ids []string) {
		saves = append(saves, append([]string{}, ids...))
		seqs = append(seqs, seq)
	})

	tr.noteThread("r1")
	tr.noteThread("r1") // already known: nothing new to persist
	tr.noteThread("r2")
	tr.noteThread("") // no thread at all
	if len(saves) != 2 {
		t.Fatalf("expected 2 saves (one per newly tracked thread), got %d: %v", len(saves), saves)
	}
	last := saves[len(saves)-1]
	if len(last) != 2 || last[0] != "r1" || last[1] != "r2" {
		t.Fatalf("sink got %v, want the full set in insertion order", last)
	}
	// Versions must increase, or a slow writer could overwrite a newer set on disk.
	if len(seqs) != 2 || seqs[0] >= seqs[1] {
		t.Fatalf("sink versions %v are not increasing", seqs)
	}
}

func TestSeedThreadsDoesNotNotifySink(t *testing.T) {
	tr := newTestTransport()
	calls := 0
	tr.SetThreadSink(func(uint64, []string) { calls++ })
	tr.SeedThreads([]string{"r1", "r2"})
	if calls != 0 {
		t.Fatalf("seeding learns nothing new, so it must not write back; got %d saves", calls)
	}
	if !tr.inThread("r1") || !tr.inThread("r2") {
		t.Fatal("seeded threads should be tracked")
	}
}

func TestSeedThreadsRespectsTheCap(t *testing.T) {
	tr := newTestTransport()
	ids := make([]string, maxTrackedThreads+10)
	for i := range ids {
		ids[i] = fmt.Sprintf("r%d", i)
	}
	tr.SeedThreads(ids)
	tr.mu.Lock()
	n := len(tr.threadOrder)
	tr.mu.Unlock()
	if n != maxTrackedThreads {
		t.Fatalf("tracked %d threads, want the cap %d", n, maxTrackedThreads)
	}
	if tr.inThread("r0") {
		t.Fatal("the oldest seeded thread should have been evicted")
	}
	if !tr.inThread(ids[len(ids)-1]) {
		t.Fatal("the newest seeded thread should be kept")
	}
}
