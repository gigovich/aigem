package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// openRun starts a run through the API and returns its id, which is how a page
// gets one too.
func openRun(t *testing.T, srv *Server) string {
	t.Helper()
	return decode[Run](t, api(t, srv, http.MethodPost, "/api/runs", `{}`)).ID
}

// dialRunSocket attaches to a run the way a page does.
func dialRunSocket(t *testing.T, srv *Server, id, query string) *controlClient {
	t.Helper()
	return dialWS(t, srv, "/api/runs/"+id+"/socket"+query)
}

// handshake sends a request that looks like an upgrade without completing one,
// so a refusal the handler makes before the hijack can be read as an ordinary
// HTTP response.
func handshake(t *testing.T, srv *Server, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.Base()+strings.TrimPrefix(path, "/"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token())
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// runError decodes the one frame on this stream that is not an event.
func runError(t *testing.T, raw []byte) wsError {
	t.Helper()
	var e wsError
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	// The literal, deliberately, and not the constant: the whole point of this
	// name is that it does not collide with "error", which is a real event
	// kind. Asserting against the constant would let a rename to "error" pass
	// while putting every client mistake into the timeline as something that
	// happened in the conversation.
	if e.Kind != "client_error" {
		t.Fatalf("frame = %s, want a client_error", raw)
	}
	return e
}

// The stream is the conversation as it happens, in order and byte for byte as
// the session wrote it. Re-encoding it here would be this package taking a view
// on a vocabulary it deliberately does not have, and the drift would show up as
// a browser rendering a conversation that did not happen.
func TestAWholeTurnReachesTheClientInOrderAndVerbatim(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	id := openRun(t, srv)
	c := dialRunSocket(t, srv, id, "")

	var want [][]byte
	for _, payload := range []string{
		`"kind":"user_message","text":"go"`,
		`"kind":"turn_start"`,
		`"kind":"tool_batch","round":1`,
		`"kind":"approval_request","id":"a1"`,
		`"kind":"approval_resolved","id":"a1","decision":"once"`,
		`"kind":"turn_end"`,
	} {
		want = append(want, b.emit(id, payload))
	}
	for i, w := range want {
		if got := c.nextRaw(); !bytes.Equal(got, w) {
			t.Fatalf("event %d = %s, want %s", i, got, w)
		}
	}
}

// A second tab is not a second conversation. It resumes from where it got to
// and sees the same timeline, which is the whole reason the stream carries a
// sequence at all.
func TestASecondConnectionResumesTheIdenticalTimeline(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	id := openRun(t, srv)

	first := dialRunSocket(t, srv, id, "")
	var all [][]byte
	for _, payload := range []string{`"kind":"user_message"`, `"kind":"turn_start"`, `"kind":"turn_end"`} {
		all = append(all, b.emit(id, payload))
	}
	for range all {
		first.nextRaw()
	}

	second := dialRunSocket(t, srv, id, "?since=1")
	for i, w := range all[1:] {
		if got := second.nextRaw(); !bytes.Equal(got, w) {
			t.Fatalf("resumed event %d = %s, want %s", i, got, w)
		}
	}
}

// An approval is one question with one answer, seen by everyone attached. The
// second answer is refused, and the refusal is not an event: it happened to a
// client, not in the conversation.
func TestAnApprovalIsSeenByBothClientsAndAnsweredOnce(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	id := openRun(t, srv)
	one := dialRunSocket(t, srv, id, "")
	two := dialRunSocket(t, srv, id, "")

	ask := b.emit(id, `"kind":"approval_request","id":"a1"`)
	for _, c := range []*controlClient{one, two} {
		if got := c.nextRaw(); !bytes.Equal(got, ask) {
			t.Fatalf("client saw %s, want the approval %s", got, ask)
		}
	}

	one.send(map[string]any{"op": "resolve", "id": "a1", "decision": "once"})
	// An applied operation has nothing to say, so the acknowledgement is the
	// backend having been given it.
	waitFor(t, func() bool { return len(b.ops()) == 1 })

	// What both clients see next is the session's own event, not a reply to the
	// one that answered: an approval somebody else decided is not a failure in
	// the conversation.
	resolved := b.emit(id, `"kind":"approval_resolved","id":"a1"`)
	for _, c := range []*controlClient{one, two} {
		if got := c.nextRaw(); !bytes.Equal(got, resolved) {
			t.Fatalf("client got %s, want the event %s", got, resolved)
		}
	}

	b.mu.Lock()
	b.opErr = Refuse(errors.New("approval already decided"))
	b.mu.Unlock()
	two.send(map[string]any{"op": "resolve", "id": "a1", "decision": "deny"})
	if e := runError(t, two.nextRaw()); e.Op != "resolve" || e.Error != "approval already decided" {
		t.Fatalf("refusal = %+v, want the reason the backend gave", e)
	}

	if ops := b.ops(); len(ops) != 1 || ops[0].Decision != "once" {
		t.Fatalf("the backend was given %+v, want the first answer alone", ops)
	}
}

// A typo must not reach a session, and must not take the conversation down
// with it either.
func TestAnUnknownOpIsAnsweredRatherThanFatal(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	id := openRun(t, srv)
	c := dialRunSocket(t, srv, id, "")

	c.send(map[string]any{"op": "sumbit", "text": "go"})
	if e := runError(t, c.nextRaw()); e.Op != "sumbit" || !strings.Contains(e.Error, "unknown op") {
		t.Fatalf("refusal = %+v, want an unknown op named back", e)
	}
	// Still usable afterwards.
	ev := b.emit(id, `"kind":"notice"`)
	if got := c.nextRaw(); !bytes.Equal(got, ev) {
		t.Fatalf("after a bad op the stream gave %s, want %s", got, ev)
	}
	if ops := b.ops(); len(ops) != 0 {
		t.Fatalf("the backend was given %+v, want nothing", ops)
	}
}

// "stop" belongs to a phase that has worktrees to discard. Until then a client
// that sends it is told the truth rather than being quietly ignored.
func TestStopIsNotYetAnOperation(t *testing.T) {
	if runOps["stop"] {
		t.Fatal("stop is in the vocabulary; the handler behind it has to exist first")
	}
}

func TestAMalformedFrameOnARunSocketIsAnsweredRatherThanFatal(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	id := openRun(t, srv)
	c := dialRunSocket(t, srv, id, "")

	c.sendRaw([]byte("{not json"))
	if e := runError(t, c.nextRaw()); !strings.Contains(e.Error, "bad message") {
		t.Fatalf("refusal = %+v, want a bad message", e)
	}
}

// A client behind a proxy with an idle timeout needs something to send, and it
// must not be a message to the model or a frame the page has to filter out.
func TestPingIsAnsweredWithSilence(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	id := openRun(t, srv)
	c := dialRunSocket(t, srv, id, "")

	c.send(map[string]any{"op": "ping"})
	// The answer to a ping is the next thing that actually happens. Reading one
	// frame and finding the event is not enough on its own - a reply the daemon
	// should not have sent would be sitting in front of it - so the event has
	// to be the *first* frame after the ping.
	ev := b.emit(id, `"kind":"notice"`)
	if got := c.nextRaw(); !bytes.Equal(got, ev) {
		t.Fatalf("after a ping the stream gave %s, want the next event %s", got, ev)
	}
	if ops := b.ops(); len(ops) != 0 {
		t.Fatalf("ping reached the backend as %+v", ops)
	}
	// And nothing follows it either.
	c.expectSilence()
}

// expectSilence fails if the daemon has anything more to say soon.
func (c *controlClient) expectSilence() {
	c.t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		c.t.Fatal(err)
	}
	data, err := wsutil.ReadServerText(c.read)
	if err == nil {
		c.t.Fatalf("the daemon sent an unexpected frame: %s", data)
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		c.t.Fatalf("the connection ended rather than going quiet: %v", err)
	}
}

// Who answered is shown to the other clients, and a client that did not name
// itself is still somebody.
func TestAnOperationWithoutALabelIsAttributedToTheClientKind(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	id := openRun(t, srv)
	c := dialRunSocket(t, srv, id, "?kind=phone")

	c.send(map[string]any{"op": "resolve", "id": "a1", "decision": "once"})
	waitFor(t, func() bool { return len(b.ops()) == 1 })
	if got := b.ops()[0].Label; got != "phone" {
		t.Errorf("label = %q, want the client kind it dialled with", got)
	}

	// A client that names no kind is still somebody, and "web" is who.
	plain := dialRunSocket(t, srv, id, "")
	waitFor(t, func() bool {
		for _, w := range b.watchers(id) {
			if w.Kind == "web" {
				return true
			}
		}
		return false
	})
	plain.send(map[string]any{"op": "resolve", "id": "a2", "decision": "once"})
	waitFor(t, func() bool { return len(b.ops()) == 2 })
	if got := b.ops()[1].Label; got != "web" {
		t.Errorf("label = %q, want the default client kind", got)
	}
}

// Everything the backend did not classify is the daemon's own fault, and the
// client is not handed a sentence assembled somewhere inside the agent.
func TestAFailedOperationTheBackendDidNotClassifyKeepsItsDetail(t *testing.T) {
	b := &fakeBackend{opErr: errNope}
	srv := newTestServer(t, Config{Backend: b})
	id := openRun(t, srv)
	c := dialRunSocket(t, srv, id, "")

	c.send(map[string]any{"op": "submit", "text": "go"})
	e := runError(t, c.nextRaw())
	if strings.Contains(e.Error, errNope.Error()) {
		t.Fatalf("refusal = %+v, it repeats the internal error", e)
	}
}

// A socket that cannot be opened is a status code the page's fetch can read,
// not a socket that opens and immediately says nothing.
func TestASocketThatCannotBeOpenedIsRefusedBeforeTheUpgrade(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	b.seed(Run{ID: "RUN-9", Status: "closed"})
	live := openRun(t, srv)

	for _, tc := range []struct {
		name, path string
		want       int
	}{
		{"unknown run", "/api/runs/RUN-nope/socket", http.StatusNotFound},
		{"closed run", "/api/runs/RUN-9/socket", http.StatusConflict},
		{"bad cursor", "/api/runs/" + live + "/socket?since=-1", http.StatusBadRequest},
	} {
		res := handshake(t, srv, tc.path)
		if res.StatusCode != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, res.StatusCode, tc.want)
		}
		if res.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("%s answered without the security headers", tc.name)
		}
	}
}

// A signed-in browser that follows this URL sends a plain GET. gobwas hijacks
// before it validates and writes its own refusal onto the connection, past the
// wrapper that puts the policy on every other response, so the check has to
// happen first.
func TestANonHandshakeOnTheRunSocketRouteIsAnOrdinaryRefusal(t *testing.T) {
	srv := newTestServer(t, Config{})
	id := openRun(t, srv)
	res := api(t, srv, http.MethodGet, "/api/runs/"+id+"/socket", "")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Error("the refusal carries no security headers")
	}
}

func TestARunSocketRefusesAForeignOrigin(t *testing.T) {
	srv := newTestServer(t, Config{})
	id := openRun(t, srv)
	req, err := http.NewRequest(http.MethodGet, srv.Base()+"api/runs/"+id+"/socket", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token())
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
}

// A run that ends under a client ends its socket. Otherwise the tab sits on a
// stream that will never say anything again and never says so.
func TestClosingARunEndsItsSockets(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	id := openRun(t, srv)
	c := dialRunSocket(t, srv, id, "")

	if res := api(t, srv, http.MethodDelete, "/api/runs/"+id, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("close = %d, want 200", res.StatusCode)
	}
	c.expectHangUp()
}

// A hijacked connection is no longer the http.Server's, so nothing else would
// ever end the goroutines behind it or give back the socket-cap slot it holds.
// The control stream has the same test; a run socket takes a different route
// into the same registry, and a route that forgot to register would leak.
func TestCloseEndsTheRunSocketsAndGivesTheSlotsBack(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	id := openRun(t, srv)
	c := dialRunSocket(t, srv, id, "")
	waitFor(t, func() bool { return openSockets(srv) == 1 })

	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	c.expectHangUp()
	// Close waits for the slots, so this is a fact by the time it returns
	// rather than something to poll for.
	if n := openSockets(srv); n != 0 {
		t.Errorf("%d socket slots are still held after Close", n)
	}
}

func openSockets(srv *Server) int {
	srv.sockets.mu.Lock()
	defer srv.sockets.mu.Unlock()
	return srv.sockets.open
}

// waitFor polls until cond holds, and fails rather than hanging.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the condition never held")
}

// A submitted message is bounded like everything else a client sends. The bound
// that bites is the frame one, because a browser sends a message as a single
// frame: this is what a page may put in one submit, and it is why a scaled
// image attachment has to fit inside it.
func TestASubmitPastTheFrameBoundEndsTheSocket(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	id := openRun(t, srv)

	inside := dialRunSocket(t, srv, id, "")
	inside.sendRaw([]byte(`{"op":"submit","text":"` + strings.Repeat("x", wsMaxFrame/2) + `"}`))
	waitFor(t, func() bool { return len(b.ops()) == 1 })

	past := dialRunSocket(t, srv, id, "")
	// The daemon hangs up as soon as it sees the frame is too large, without
	// draining the rest: the write may fail partway, which is the outcome
	// under test rather than a problem with it.
	_ = wsutil.WriteClientText(past.conn, []byte(`{"op":"submit","text":"`+strings.Repeat("x", wsMaxFrame)+`"}`))
	past.expectHangUp()
	if n := len(b.ops()); n != 1 {
		t.Errorf("%d operations reached the backend, want the one inside the bound", n)
	}
}

// A tab that goes away has to take its subscription with it. Left attached, the
// session goes on publishing to a reader that will never come back: a
// subscriber and a goroutine per disconnect, and a presence line that shows
// people who are not there.
func TestADisconnectingClientDetachesFromTheRun(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	id := openRun(t, srv)

	c := dialRunSocket(t, srv, id, "?kind=phone&label=the%20kitchen%20one")
	// Attached, and attached as who it said it was: presence is built out of
	// this, so a router that dropped the identity would show an unnamed tab.
	waitFor(t, func() bool { return len(b.watchers(id)) == 1 })
	if got := b.watchers(id)[0]; got.Kind != "phone" || got.Label != "the kitchen one" {
		t.Errorf("watcher = %+v, want the kind and label it dialled with", got)
	}

	c.close()
	waitFor(t, func() bool { return len(b.watchers(id)) == 0 })
}

// A client that asks to resume from a point the run no longer holds cannot
// retry its way out of it, and the socket route has to say so with the same
// status the timeline does - it is the other half of the same recovery.
func TestASocketResumingPastTheHistoryIsGone(t *testing.T) {
	b := &fakeBackend{watchErr: ErrHistoryGone}
	srv := newTestServer(t, Config{Backend: b})
	b.seed(Run{ID: "RUN-1", Status: "open", Live: true})
	res := handshake(t, srv, "/api/runs/RUN-1/socket?since=90")
	if res.StatusCode != http.StatusGone {
		t.Fatalf("status = %d, want 410", res.StatusCode)
	}
}

// A run that ends under a tab has to end its socket cleanly. Cutting a frame in
// half leaves the client reading its tail as a header, which a browser reports
// as a protocol error and a failed connection rather than as the conversation
// it was watching ending.
//
// The shape that used to do it: a pump partway through a frame while the run
// closes. Several rounds, because it is a race and one round would miss it.
func TestAClosingRunEndsItsSocketsWithoutCuttingAFrame(t *testing.T) {
	for round := range 12 {
		b := &fakeBackend{}
		srv := newTestServer(t, Config{Backend: b})
		id := openRun(t, srv)
		c := dialRunSocket(t, srv, id, "")

		// Keep the pump busy with frames the client is reading, so the close
		// lands while one of them is on the wire.
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range 200 {
				b.emit(id, `"kind":"content","text":"`+strings.Repeat("x", 512)+`"`)
			}
		}()

		if res := api(t, srv, http.MethodDelete, "/api/runs/"+id, ""); res.StatusCode != http.StatusOK {
			t.Fatalf("round %d: close = %d, want 200", round, res.StatusCode)
		}
		<-done

		// Drain to the end. Every frame has to be a whole one: a torn frame
		// comes back as a protocol error, not as the end of the stream.
		for {
			if err := c.conn.SetReadDeadline(time.Now().Add(testWait)); err != nil {
				t.Fatal(err)
			}
			_, err := wsutil.ReadServerText(c.read)
			if err == nil {
				continue
			}
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				break
			}
			var closed wsutil.ClosedError
			if errors.As(err, &closed) {
				break
			}
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				t.Fatalf("round %d: the socket neither ended nor said anything", round)
			}
			t.Fatalf("round %d: the stream ended mid-frame: %v", round, err)
		}
	}
}

// A browser that leaves a run sends one Close frame and expects one back. A
// second one from the daemon is what Chrome reports as "Close received after
// close" on every run switch.
func TestAClientCloseIsAnsweredWithExactlyOneCloseFrame(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	id := openRun(t, srv)
	c := dialRunSocket(t, srv, id, "")

	closing := ws.NewCloseFrame(ws.NewCloseFrameBody(ws.StatusNormalClosure, ""))
	closing = ws.MaskFrameInPlace(closing)
	if err := ws.WriteFrame(c.conn, closing); err != nil {
		t.Fatal(err)
	}

	if closes := countCloseFrames(t, c); closes != 1 {
		t.Fatalf("the daemon sent %d close frames after the client's, want exactly 1", closes)
	}
}

// A close frame with a status code outside the range the RFC allows is still a
// close: the daemon must not let a malformed body stop it from marking the
// peer closed before it replies, which is what the double close frame above
// used to come from.
func TestAClientCloseWithAnInvalidStatusIsStillAnsweredOnce(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	id := openRun(t, srv)
	c := dialRunSocket(t, srv, id, "")

	closing := ws.NewCloseFrame(ws.NewCloseFrameBody(ws.StatusCode(999), ""))
	closing = ws.MaskFrameInPlace(closing)
	if err := ws.WriteFrame(c.conn, closing); err != nil {
		t.Fatal(err)
	}

	if closes := countCloseFrames(t, c); closes != 1 {
		t.Fatalf("the daemon sent %d close frames after the client's, want exactly 1", closes)
	}
}

// countCloseFrames drains the stream to EOF and counts the Close frames on it.
func countCloseFrames(t *testing.T, c *controlClient) int {
	t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(testWait)); err != nil {
		t.Fatal(err)
	}
	var closes int
	for {
		hdr, err := ws.ReadHeader(c.read)
		if err != nil {
			break
		}
		if hdr.OpCode == ws.OpClose {
			closes++
		}
		if _, err := io.CopyN(io.Discard, c.read, hdr.Length); err != nil {
			break
		}
	}
	return closes
}
