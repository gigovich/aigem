package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// controlClient is a page's end of the control stream: the connection, plus a
// reader that fails the test rather than blocking forever.
type controlClient struct {
	t    *testing.T
	conn net.Conn
	// read is where frames come from, which is not always the connection. The
	// daemon sends hello the moment it has upgraded, so the handshake response
	// and the first frame often arrive in one TCP segment and the dialer's
	// buffer keeps whatever it read past the response. Reading the connection
	// directly then blocks forever on a message already in hand - as a flake,
	// since it depends on the timing of two writes.
	read io.ReadWriter
}

// readWriter reads from one place and writes to another: wsutil answers a
// control frame on the connection while the frames come out of the dialer's
// buffer.
type readWriter struct {
	io.Reader
	io.Writer
}

// dialControl opens /api/socket with the daemon's token, the way a page does
// with its cookie.
func dialControl(t *testing.T, srv *Server) *controlClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	d := ws.Dialer{Header: ws.HandshakeHeaderHTTP(http.Header{
		"Authorization": []string{"Bearer " + srv.Token()},
	})}
	conn, br, _, err := d.Dial(ctx, "ws://"+srv.Addr().String()+"/api/socket")
	if err != nil {
		t.Fatalf("dial /api/socket: %v", err)
	}
	c := &controlClient{t: t, conn: conn, read: conn}
	if br != nil {
		// It wraps the connection, so it is the whole stream and not just the
		// part that arrived early.
		c.read = readWriter{Reader: br, Writer: conn}
	}
	t.Cleanup(c.close)
	return c
}

func (c *controlClient) close() { _ = c.conn.Close() }

// next returns the next message, decoded far enough to see the envelope and
// keep the payload.
func (c *controlClient) next() controlFrame {
	c.t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(testWait)); err != nil {
		c.t.Fatal(err)
	}
	data, err := wsutil.ReadServerText(c.read)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var f controlFrame
	if err := json.Unmarshal(data, &f); err != nil {
		c.t.Fatalf("decode %s: %v", data, err)
	}
	f.raw = data
	return f
}

// expectHangUp waits for the daemon to end the connection. A read that merely
// times out is not that: it is the daemon still holding whatever the client
// sent, which is the defect these callers are about.
func (c *controlClient) expectHangUp() {
	c.t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(testWait)); err != nil {
		c.t.Fatal(err)
	}
	_, err := wsutil.ReadServerText(c.read)
	if err == nil {
		c.t.Fatal("the daemon answered rather than hanging up")
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		c.t.Fatalf("the daemon neither answered nor hung up: %v", err)
	}
}

func (c *controlClient) send(v any) {
	c.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := wsutil.WriteClientText(c.conn, b); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// controlFrame is the reading half of controlMessage: Data stays raw so a test
// can decode it into whatever that message carries.
type controlFrame struct {
	Type string          `json:"type"`
	Rev  uint64          `json:"rev"`
	Data json.RawMessage `json:"data"`
	raw  []byte
}

func (f controlFrame) clientError(t *testing.T) clientError {
	t.Helper()
	var e clientError
	if err := json.Unmarshal(f.Data, &e); err != nil {
		t.Fatalf("decode %s: %v", f.raw, err)
	}
	return e
}

// ---- hello ----

// The first message is the client's base. Everything after it is a delta, so a
// page that started reading deltas without one would be applying changes to
// state it never had.
func TestTheFirstControlMessageIsHelloWithTheCurrentRev(t *testing.T) {
	want := Meta{Version: "9.9.9-test", DefaultModel: "openai/gpt-5.6-sol"}
	srv := newTestServer(t, Config{Backend: &fakeBackend{meta: want}, Assets: spaHandler(testDist())})
	srv.hub.publish("run.updated", map[string]string{"id": "before-anyone-connected"})
	srv.hub.publish("run.updated", map[string]string{"id": "still-before"})

	got := dialControl(t, srv).next()
	if got.Type != controlHello {
		t.Fatalf("first message type = %q, want %q", got.Type, controlHello)
	}
	if rev := srv.hub.current(); got.Rev != rev {
		t.Errorf("hello rev = %d, want the daemon's current %d", got.Rev, rev)
	}
	var body metaResponse
	if err := json.Unmarshal(got.Data, &body); err != nil {
		t.Fatalf("decode hello: %v", err)
	}
	if body.Version != want.Version || body.DefaultModel != want.DefaultModel {
		t.Errorf("hello meta = %+v, want %+v", body.Meta, want)
	}
	if !body.UI {
		t.Error("hello says this daemon has no UI, but it was built with assets")
	}
	if !body.Features["controlSocket"] {
		t.Errorf("hello carries no feature map: %v", body.Features)
	}
	// The base is the same document /api/meta serves, stamped with the same
	// revision the envelope carries: a page that reconnects must not have to
	// fetch what it has just been handed.
	if body.Rev != got.Rev {
		t.Errorf("hello data is at rev %d while the envelope says %d", body.Rev, got.Rev)
	}
}

// A reconnect re-bases on the revision the daemon is at now, and the first
// thing it is sent afterwards is the next mutation rather than an old one.
//
// The daemon keeps no history, so the replay half cannot fail today; it is
// here as the guard for the day someone adds a buffer, and the re-base half is
// what the assertions are really on.
func TestAReconnectRebasesOnTheCurrentRevision(t *testing.T) {
	srv := newTestServer(t, Config{})
	first := dialControl(t, srv)
	first.next()
	srv.hub.publish("run.updated", map[string]string{"id": "one"})
	got := first.next()
	if got.Type != "run.updated" {
		t.Fatalf("type = %q, want run.updated", got.Type)
	}
	first.close()

	next := dialControl(t, srv)
	hello := next.next()
	if hello.Type != controlHello {
		t.Fatalf("the reconnect's first message = %q, want hello", hello.Type)
	}
	if hello.Rev != got.Rev {
		t.Errorf("hello rev = %d, want the revision already published, %d", hello.Rev, got.Rev)
	}
	// Nothing has been published since, so anything arriving now is a replay.
	rev := srv.hub.publish("counts", map[string]int{"runs": 0})
	after := next.next()
	if after.Type != "counts" || after.Rev != rev {
		t.Errorf("after hello: %s at rev %d, want the counts published at %d, "+
			"which means the socket replayed", after.Type, after.Rev, rev)
	}
}

// ---- rev ----

// One counter for the daemon, one step per mutation. A client checks for a gap
// by arithmetic alone, which only works if nothing else ever moves it.
func TestEachPublishedMutationStepsTheRevByOne(t *testing.T) {
	srv := newTestServer(t, Config{})
	c := dialControl(t, srv)
	base := c.next().Rev

	for i := range 3 {
		want := base + uint64(i) + 1
		if rev := srv.hub.publish("run.updated", map[string]int{"n": i}); rev != want {
			t.Fatalf("publish %d assigned rev %d, want %d", i, rev, want)
		}
		got := c.next()
		if got.Rev != want {
			t.Fatalf("message %d carried rev %d, want %d", i, got.Rev, want)
		}
	}
}

// A client that stops reading is dropped, not skipped past.
//
// Skipping and leaving the socket open is the tempting version and it does not
// work: a skip is only ever noticed by a message that arrives after it, and the
// burst that fills a queue is exactly the burst that then ends. The client
// would sit on a contiguous prefix, see no gap, and be stale forever. Ending
// the socket makes its recovery a reconnect, and a reconnect re-bases it.
func TestAClientThatStopsReadingIsDroppedRatherThanLeftStale(t *testing.T) {
	srv := newTestServer(t, Config{})
	sub, base := srv.hub.subscribe()
	defer srv.hub.unsubscribe(sub)

	// Nothing is reading this subscription. The daemon must not block on it,
	// which is what the deadline here is really testing.
	const overflow = controlBuffer * 2
	done := make(chan uint64, 1)
	go func() {
		var last uint64
		for i := range overflow {
			last = srv.hub.publish("run.updated", map[string]int{"n": i})
		}
		done <- last
	}()
	select {
	case last := <-done:
		if want := base + overflow; last != want {
			t.Fatalf("last rev = %d, want %d", last, want)
		}
	case <-time.After(testWait):
		t.Fatal("the hub blocked on a client that was not reading")
	}

	select {
	case <-sub.behind:
	default:
		t.Fatal("a client that never read a message was not marked behind")
	}
}

// And the socket ends, so the client finds out and reconnects. This is the
// other half of the drop: the hub marking a subscriber behind is only useful
// because the writer stops on it.
//
// Driven directly rather than over a real connection: making a peer stop
// reading means filling its kernel receive buffer, which takes megabytes and
// proves nothing this does not.
func TestTheWriterStopsForAClientTheHubHasGivenUpOn(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	c := newWSConn(server, server)
	defer c.close()

	sub := &controlSub{msgs: make(chan controlDelta, controlBuffer), behind: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		pump(c, sub.msgs, func(d controlDelta) error { return c.sendBytes(d.frame) },
			sub.behind, wsPingInterval)
	}()

	close(sub.behind)
	select {
	case <-done:
	case <-time.After(testWait):
		t.Fatal("the writer kept going for a client the hub had given up on")
	}
}

// A payload that will not encode still spends its revision, and the client is
// still told the revision moved - an envelope with no data. Spending the number
// and sending nothing would leave a page stale with no gap to notice, which is
// the one outcome this design has no recovery for.
func TestAnUnencodablePayloadStillReachesTheClientAsARevision(t *testing.T) {
	srv := newTestServer(t, Config{})
	c := dialControl(t, srv)
	base := c.next().Rev

	rev := srv.hub.publish("run.updated", make(chan int))
	if rev != base+1 {
		t.Fatalf("rev = %d, want %d", rev, base+1)
	}
	got := c.next()
	if got.Rev != rev || got.Type != "run.updated" {
		t.Fatalf("got %s, want a run.updated at rev %d", got.raw, rev)
	}
	if len(got.Data) != 0 {
		t.Errorf("the frame carries data that could not be encoded: %s", got.raw)
	}

	// A mutation with nothing to say takes the same shape, and must not put a
	// bare null on the wire for a client to unwrap.
	rev = srv.hub.publish("counts", nil)
	if got = c.next(); got.Rev != rev || len(got.Data) != 0 {
		t.Errorf("a payload-free mutation came out as %s", got.raw)
	}
}

// A message published between the subscription and hello would be delivered
// under a rev the client was told it already had, and the client would drop it
// as old. The two have to happen under one lock.
func TestNoMutationFallsBetweenTheSubscriptionAndHello(t *testing.T) {
	h := newHub()
	// The publisher runs for the whole test rather than a fixed number of
	// times, so every subscription below is guaranteed a message to check. The
	// first version of this raced two finite loops and checked nothing at all
	// on most runs.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				h.publish("run.updated", map[string]bool{"x": true})
			}
		}
	}()
	defer func() { close(stop); <-done }()

	for range 200 {
		sub, rev := h.subscribe()
		select {
		case d := <-sub.msgs:
			var f controlFrame
			if err := json.Unmarshal(d.frame, &f); err != nil {
				t.Fatal(err)
			}
			// Exactly the next one. "Greater than" would pass against the
			// other half of the defect too: a subscription registered after
			// the rev was read loses every message in between, and its first
			// delivery is simply further along.
			if f.Rev != rev+1 {
				t.Fatalf("a subscription based at rev %d was first delivered rev %d, "+
					"want %d", rev, f.Rev, rev+1)
			}
		case <-time.After(testWait):
			t.Fatal("a subscription against a running publisher received nothing")
		}
		h.unsubscribe(sub)
	}
}

// Every socket sees the same order, because a client that applied two deltas
// out of order would end up holding the older one's state under the newer
// one's rev.
//
// The publishes race each other on purpose: with a single publisher the channel
// alone would keep the order, and the test would pass with the fan-out moved
// out from under the lock that actually guarantees it.
func TestEveryClientSeesTheSameOrder(t *testing.T) {
	srv := newTestServer(t, Config{})
	clients := []*controlClient{dialControl(t, srv), dialControl(t, srv), dialControl(t, srv)}
	for _, c := range clients {
		c.next()
	}
	const publishers, each = 4, 5
	var wg sync.WaitGroup
	for range publishers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				srv.hub.publish("run.updated", map[string]bool{"x": true})
			}
		}()
	}
	wg.Wait()

	for i, c := range clients {
		for j := range publishers * each {
			if got, want := c.next().Rev, uint64(j)+1; got != want {
				t.Fatalf("client %d message %d carried rev %d, want %d", i, j, got, want)
			}
		}
	}
}

// ---- client ops ----

// The rule the socket exists to keep: it reads. A mutation sent up it gets a
// refusal, and the refusal is not a state delta - it carries the revision the
// daemon is already at, so a client applying "greater than the last plus one
// means a gap" does not refetch over its own mistake.
func TestTheControlSocketRefusesEverythingButPing(t *testing.T) {
	srv := newTestServer(t, Config{})
	c := dialControl(t, srv)
	base := c.next().Rev

	// A ping is answered with nothing at all, which is what the published
	// message after it proves: anything the ping had provoked would arrive
	// first and fail the check below.
	c.send(map[string]string{"op": "ping"})
	c.send(map[string]any{"op": "run.delete", "id": "1"})
	got := c.next()
	if got.Type != controlClientError {
		t.Fatalf("a mutation over the control socket answered %s, want %s",
			got.raw, controlClientError)
	}
	if e := got.clientError(t); e.Op != "run.delete" {
		t.Errorf("the refusal names op %q, want the one that was sent", e.Op)
	}
	if got.Rev != base {
		t.Errorf("the refusal carries rev %d, want the unchanged %d: a client mistake "+
			"is not a mutation and must not read as a gap", got.Rev, base)
	}

	// And the socket survives it: one bad message is not a reason to drop a page.
	rev := srv.hub.publish("counts", map[string]int{"runs": 1})
	after := c.next()
	if after.Type != "counts" || after.Rev != rev {
		t.Errorf("after a refused op: %s, want the counts at rev %d", after.raw, rev)
	}
}

// The refusal carries the revision this connection has been sent, not the one
// the daemon is at. publish only queues, so the two diverge whenever a delta is
// waiting - and a refusal stamped with the hub's number would read as a gap,
// send the page to refetch a snapshot, and then hand it the queued deltas to
// apply back over the top of it.
//
// The delta is held mid-write, which is what keeps the two numbers apart for
// long enough to tell them apart.
func TestARefusalCarriesWhatThisConnectionHasBeenSent(t *testing.T) {
	srv := newTestServer(t, Config{})
	c := dialControl(t, srv)
	base := c.next().Rev
	conn := serverConn(t, srv)

	held, release := make(chan struct{}), make(chan struct{})
	writing := make(chan struct{})
	free := sync.OnceFunc(func() { close(release) })
	t.Cleanup(free)
	go func() {
		defer close(writing)
		_ = conn.write(func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	// Queued and unwritable: the hub is now a revision ahead of this socket.
	rev := srv.hub.publish("run.updated", map[string]int{"n": 1})
	c.send(map[string]string{"op": "run.delete"})

	// Nothing can reach the client while that write holds the connection, which
	// is also what gives the reader time to have taken the revision it will
	// answer with.
	if err := c.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := wsutil.ReadServerText(c.read); err == nil {
		t.Fatal("a frame arrived while the connection was held")
	}
	free()
	<-writing

	// Both frames arrive in some order; the refusal is the one under test.
	var refusal controlFrame
	for range 2 {
		got := c.next()
		if got.Type == controlClientError {
			refusal = got
		}
	}
	if refusal.Type != controlClientError {
		t.Fatal("no refusal arrived")
	}
	if refusal.Rev != base {
		t.Errorf("the refusal carries rev %d, want %d - the revision this socket had been "+
			"sent, not the %d the daemon had reached", refusal.Rev, base, rev)
	}
}

// serverConn is the daemon's end of the one open socket.
func serverConn(t *testing.T, srv *Server) *wsConn {
	t.Helper()
	srv.conns.mu.Lock()
	defer srv.conns.mu.Unlock()
	if n := len(srv.conns.conns); n != 1 {
		t.Fatalf("%d open connections, want exactly one", n)
	}
	for c := range srv.conns.conns {
		return c
	}
	return nil
}

func TestAMalformedFrameIsAnsweredRatherThanFatal(t *testing.T) {
	srv := newTestServer(t, Config{})
	c := dialControl(t, srv)
	c.next()

	if err := wsutil.WriteClientText(c.conn, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	got := c.next()
	if got.Type != controlClientError {
		t.Fatalf("a malformed frame answered %s, want %s", got.raw, controlClientError)
	}
	rev := srv.hub.publish("counts", map[string]int{"runs": 2})
	if after := c.next(); after.Rev != rev {
		t.Errorf("the socket did not survive a malformed frame: %s", after.raw)
	}
}

// A protocol ping is answered by the daemon itself, and the answer has to go
// through the same lock as everything else. gobwas writes one straight to the
// connection from the reading goroutine unless the reader is built to stop it,
// which puts a pong inside whatever frame the writer is partway through - and
// the client then reads the pong's bytes as the payload and the payload's as a
// frame header.
//
// Driven against a held lock rather than a race: the window is between the two
// writes one frame takes, and a test that hoped to land in it would report the
// defect as a flake.
func TestAControlReplyWaitsForTheFrameInFlight(t *testing.T) {
	client, server := net.Pipe()
	c := newWSConn(server, server)

	// A write that has the connection and has not finished with it. Releasing
	// it is a cleanup rather than a defer, registered last so it runs first:
	// close waits on the same mutex, and a failure before the release below
	// would otherwise hang the test rather than fail it.
	held, release := make(chan struct{}), make(chan struct{})
	writing := make(chan struct{})
	free := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(c.close)
	t.Cleanup(free)
	go func() {
		defer close(writing)
		_ = c.write(func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	reading := make(chan struct{})
	go func() {
		defer close(reading)
		c.readClientOps(func([]byte) any { return nil })
	}()
	go func() { _ = wsutil.WriteClientMessage(client, ws.OpPing, nil) }()

	// Nothing may come back while that write holds the connection.
	if err := client.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.ReadHeader(client); err == nil {
		t.Fatal("a control reply was written into a frame that was still in flight")
	}

	// And it arrives once the connection is free.
	free()
	<-writing
	if err := client.SetReadDeadline(time.Now().Add(testWait)); err != nil {
		t.Fatal(err)
	}
	h, err := ws.ReadHeader(client)
	if err != nil {
		t.Fatalf("the pong never arrived: %v", err)
	}
	if h.OpCode != ws.OpPong {
		t.Errorf("opcode = %v, want a pong", h.OpCode)
	}
	c.close()
	<-reading
}

// Everything a client sends up any of these streams is a small op envelope, and
// an unbounded read is an out-of-memory the sender pays nothing for.
func TestAnOversizedMessageEndsTheSocketRatherThanBufferingIt(t *testing.T) {
	srv := newTestServer(t, Config{})
	c := dialControl(t, srv)
	c.next()

	big := make([]byte, wsMaxFrame+1)
	for i := range big {
		big[i] = 'x'
	}
	// The write may fail partway once the daemon has hung up, which is the
	// outcome under test rather than a problem with it.
	_ = wsutil.WriteClientText(c.conn, big)
	c.expectHangUp()
}

// The keepalive is the whole reason the idle timeout is survivable: a browser
// answers a protocol ping itself, so a live but quiet connection keeps its read
// deadline moving without the page having to do anything. Nothing else would
// tell a healthy idle socket apart from a peer that vanished off the network.
func TestTheDaemonPingsAQuietConnection(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	c := newWSConn(server, server)
	defer c.close()

	msgs := make(chan controlDelta)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pump(c, msgs, func(controlDelta) error { return nil }, nil, time.Millisecond)
	}()

	if err := client.SetReadDeadline(time.Now().Add(testWait)); err != nil {
		t.Fatal(err)
	}
	h, err := ws.ReadHeader(client)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if h.OpCode != ws.OpPing {
		t.Errorf("the daemon sent opcode %v to a quiet connection, want a ping", h.OpCode)
	}
	c.close()
	<-done
}

// MaxFrameSize bounds one frame; a message split across continuation frames is
// bounded only by the limit on the accumulation. Without it a client sends any
// number of small frames and the daemon buffers all of them.
func TestAFragmentedMessagePastTheBudgetEndsTheSocket(t *testing.T) {
	srv := newTestServer(t, Config{})
	c := dialControl(t, srv)
	c.next()

	chunk := make([]byte, 8<<10)
	for i := range chunk {
		chunk[i] = 'x'
	}
	// Opened and never finished: every frame is well under wsMaxFrame, and only
	// their sum is over wsMaxMessage.
	first := ws.NewFrame(ws.OpText, false, chunk)
	if err := ws.WriteFrame(c.conn, ws.MaskFrame(first)); err != nil {
		t.Fatal(err)
	}
	for range wsMaxMessage/len(chunk) + 2 {
		next := ws.NewFrame(ws.OpContinuation, false, chunk)
		if err := ws.WriteFrame(c.conn, ws.MaskFrame(next)); err != nil {
			// The daemon hung up partway, which is the outcome under test.
			break
		}
	}

	c.expectHangUp()
}

// ---- auth ----

func TestTheControlSocketNeedsACredential(t *testing.T) {
	srv := newTestServer(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	conn, _, _, err := ws.Dialer{}.Dial(ctx, "ws://"+srv.Addr().String()+"/api/socket")
	if err == nil {
		_ = conn.Close()
		t.Fatal("an unauthenticated handshake was upgraded")
	}
	var status ws.StatusError
	if !errors.As(err, &status) || int(status) != http.StatusUnauthorized {
		t.Errorf("handshake error = %v, want 401", err)
	}
}

func TestTheControlSocketRefusesAForeignOrigin(t *testing.T) {
	srv := newTestServer(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	d := ws.Dialer{Header: ws.HandshakeHeaderHTTP(http.Header{
		"Authorization": []string{"Bearer " + srv.Token()},
		"Origin":        []string{"https://evil.example"},
	})}
	conn, _, _, err := d.Dial(ctx, "ws://"+srv.Addr().String()+"/api/socket")
	if err == nil {
		_ = conn.Close()
		t.Fatal("a handshake from a foreign origin was upgraded")
	}
	var status ws.StatusError
	if !errors.As(err, &status) || int(status) != http.StatusForbidden {
		t.Errorf("handshake error = %v, want 403", err)
	}
}

// A backend that cannot describe the daemon has to be a status code. After the
// hijack there is nowhere to put one, and a page would meet the failure as a
// socket that opened and said nothing.
func TestAFailingBackendRefusesTheHandshakeRatherThanOpeningAMuteSocket(t *testing.T) {
	srv := newTestServer(t, Config{Backend: &fakeBackend{err: errors.New("no model store")}})
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	d := ws.Dialer{Header: ws.HandshakeHeaderHTTP(http.Header{
		"Authorization": []string{"Bearer " + srv.Token()},
	})}
	conn, _, _, err := d.Dial(ctx, "ws://"+srv.Addr().String()+"/api/socket")
	if err == nil {
		_ = conn.Close()
		t.Fatal("the handshake succeeded against a backend that cannot answer")
	}
	var status ws.StatusError
	if !errors.As(err, &status) || int(status) != http.StatusInternalServerError {
		t.Errorf("handshake error = %v, want 500", err)
	}
	// The subscription is taken before the backend is asked, so the refusal has
	// to give it back. One left behind is a queue the hub writes to for the
	// life of the process.
	srv.hub.mu.Lock()
	defer srv.hub.mu.Unlock()
	if n := len(srv.hub.subs); n != 0 {
		t.Errorf("%d subscribers left behind by a refused handshake", n)
	}
}

func TestOtherMethodsOnTheControlSocketAre405(t *testing.T) {
	srv := newTestServer(t, Config{})
	res, err := http.Post(srv.Base()+"api/socket", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/socket = %d, want 405", res.StatusCode)
	}
	if res.Header.Get("Allow") == "" {
		t.Error("the refusal does not say which methods are allowed")
	}
}

// A page is told its state is current as of the rev in hello. The base has to
// be read after that number, never before: a snapshot older than its rev is a
// page holding pre-change state under a post-change revision, with no gap it
// could ever notice. The other way round costs at most a delta it already has.
//
// The backend here mutates while it is being asked, which is not exotic - a
// model being signed in is a thing this very UI does.
func TestHelloIsNeverNewerThanTheRevItClaims(t *testing.T) {
	b := &mutatingBackend{}
	srv := newTestServer(t, Config{Backend: b})
	b.hub = srv.hub

	got := dialControl(t, srv).next()
	var body metaResponse
	if err := json.Unmarshal(got.Data, &body); err != nil {
		t.Fatal(err)
	}
	at := b.at.Load()
	if at == 0 {
		t.Fatal("the backend never published; this test checked nothing")
	}
	if got.Rev >= at {
		t.Fatalf("hello claims rev %d, but its base was read before the mutation at rev %d: "+
			"the page is stale with no gap to notice", got.Rev, at)
	}
	if body.Rev != got.Rev {
		t.Errorf("hello data is at rev %d while the envelope says %d", body.Rev, got.Rev)
	}
}

// mutatingBackend publishes on its way to answering, which is what pins the
// order of subscribe and Meta.
type mutatingBackend struct {
	hub *hub
	at  atomic.Uint64
}

func (b *mutatingBackend) Meta(context.Context) (Meta, error) {
	b.at.Store(b.hub.publish("model.changed", map[string]string{"ref": "openai/gpt-5.6-sol"}))
	return Meta{Version: "1.2.3", DefaultModel: "openai/gpt-5.6-sol"}, nil
}

// gobwas hijacks the connection before it validates anything, and hands it back
// alive along with the refusal it has already written on it. Nothing else can
// reach that connection - net/http has given it up, and it never reached the
// registry Close walks - so dropping it leaks the descriptor for the life of
// the process.
//
// A handshake with the upgrade headers but no key takes that path.
func TestAHandshakeTheUpgradeRefusesDoesNotLeakItsConnection(t *testing.T) {
	srv := newTestServer(t, Config{})
	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	req := "GET /api/socket HTTP/1.1\r\nHost: " + srv.Addr().String() +
		"\r\nAuthorization: Bearer " + srv.Token() +
		"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(testWait)); err != nil {
		t.Fatal(err)
	}
	// Whatever it answered, the connection has to end. Reading to EOF is what
	// says the daemon let go of it.
	body, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("the refused handshake left its connection open: %v", err)
	}
	if !strings.HasPrefix(string(body), "HTTP/1.1 4") {
		t.Errorf("the refusal was %q, want a 4xx", string(body))
	}
}

// A request that is not a handshake at all - a signed-in browser following the
// URL - is refused before the hijack, so the answer is an ordinary response
// carrying the security headers rather than one the library writes itself.
func TestANonHandshakeOnTheSocketRouteIsAnOrdinaryRefusal(t *testing.T) {
	srv := newTestServer(t, Config{})
	req, err := http.NewRequest(http.MethodGet, srv.Base()+"api/socket", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Error("the refusal answered without the security headers")
	}
}

// ---- shutdown ----

// http.Server.Close does not know about a hijacked connection, so without the
// registry a shutdown leaves every socket - and the goroutines and cap slots
// behind them - running until the process exits.
func TestCloseEndsTheControlSocketsAndGivesTheSlotsBack(t *testing.T) {
	srv, err := New(withBackend(Config{}))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve() }()

	const clients = 4
	conns := make([]*controlClient, 0, clients)
	for range clients {
		c := dialControl(t, srv)
		c.next()
		conns = append(conns, c)
	}
	srv.sockets.mu.Lock()
	open := srv.sockets.open
	srv.sockets.mu.Unlock()
	if open != clients {
		t.Fatalf("open sockets = %d, want %d", open, clients)
	}

	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close waits for the slots, so this is a fact by the time it returns rather
	// than something to poll for.
	srv.sockets.mu.Lock()
	open = srv.sockets.open
	srv.sockets.mu.Unlock()
	if open != 0 {
		t.Errorf("open sockets after Close = %d, want 0", open)
	}

	// And each client sees the connection end rather than hanging on a socket
	// nothing will ever write to again.
	for i, c := range conns {
		if err := c.conn.SetReadDeadline(time.Now().Add(testWait)); err != nil {
			t.Fatal(err)
		}
		if _, err := wsutil.ReadServerText(c.read); err == nil {
			t.Errorf("client %d read a message from a closed daemon", i)
		}
	}

	// Twice is safe: the operator's Ctrl-C and the deferred Close in main are
	// both real.
	if err := srv.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// Shutdown must not be able to hang on a slot that never comes back. Every
// connection has been closed by the time drain runs, so this is a backstop
// rather than a duration anything is expected to take - but a backstop that
// waited forever would not be one.
func TestDrainGivesUpRatherThanWaitingForever(t *testing.T) {
	var s sockets
	if !s.acquire() {
		t.Fatal("a fresh table refused the first socket")
	}
	if s.drain(20 * time.Millisecond) {
		t.Fatal("drain reported the table empty while a slot was held")
	}
	s.release()
	if !s.drain(testWait) {
		t.Error("drain did not notice the slot coming back")
	}
	// And again, because Server.Close is safe to call twice.
	if !s.drain(testWait) {
		t.Error("a second drain of an empty table did not return")
	}
}

// A socket opened while Close is running has to be refused rather than left
// holding a connection nothing will shut down.
func TestASocketOpenedAfterCloseIsNotKept(t *testing.T) {
	h := hijacked{conns: make(map[*wsConn]struct{})}
	h.closeAll()
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	c := newWSConn(server, server)
	if h.add(c) {
		t.Fatal("a closed registry accepted a connection")
	}
	c.close()
	if _, err := server.Write([]byte("x")); err == nil {
		t.Error("the refused connection is still open")
	}
}
