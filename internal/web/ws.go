package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// The websocket plumbing every stream in this package is built on: one
// connection, serialised writes, and a registry so that Close reaches
// connections net/http has handed over and no longer knows about.

const (
	// wsWriteTimeout bounds a single frame. A client that has stopped reading -
	// a phone that slept, a proxy that went away without a FIN - otherwise pins
	// the writing goroutine for as long as its receive window stays shut, and
	// the daemon learns about it only when the socket cap fills up.
	wsWriteTimeout = 5 * time.Second
	// wsDrainTimeout is how long Close waits for the socket slots to come back
	// after every connection has been closed under their handlers. It is a
	// backstop, not a duration anything is expected to take.
	wsDrainTimeout = 10 * time.Second
	// wsCloseWait bounds the two things ending a connection politely can wait
	// for: a frame already in flight, and the close frame that says goodbye. It
	// is short because both are only worth waiting for while there is a client
	// still reading, and a client that is reading needs neither.
	wsCloseWait = 250 * time.Millisecond
	// wsIdleTimeout is how long a connection may go without a frame from the
	// client before the daemon gives up on it.
	//
	// It is what makes a half-open connection recoverable. Nothing else notices
	// one: the hijack takes the socket out of the http.Server's timeouts, an
	// idle stream writes nothing that could fail, and TCP alone would sit on it
	// for hours - all while it holds a slot out of maxSockets, a subscription,
	// and two goroutines. Sixty-four of those and the operator's own tab is
	// answered with 503 until the daemon restarts.
	wsIdleTimeout = 90 * time.Second
	// wsPingInterval keeps a live but quiet connection inside that timeout.
	// A browser answers a protocol ping itself, so this needs nothing of the
	// page, which is the point: a client cannot keep its slot by staying silent.
	wsPingInterval = 30 * time.Second
	// wsMaxFrame and wsMaxMessage bound what one client can make the daemon
	// hold. Everything a client sends up any of these streams is a small op
	// envelope, and without a bound a single unterminated message is an
	// out-of-memory the sender pays nothing for.
	//
	// The frame bound is the operative one, and it bounds a browser: a page
	// sends a message as one frame rather than fragmenting it, so this is what
	// a submit may carry. A megabyte holds typed text and a screenshot the page
	// has scaled down, and 64 sockets of it is a quarter of a gigabyte on a
	// daemon that serves one signed-in person.
	//
	// The message bound only matters for a client that fragments a send across
	// continuation frames - a browser never does - so it is really a second,
	// looser ceiling on a non-browser client; the frame bound above is the real
	// per-socket limit.
	wsMaxFrame   = 1 << 20
	wsMaxMessage = 2 << 20
)

// wsConn is one hijacked connection. It serialises writes, because the event
// pump, a reply to a bad op and the answer to a protocol ping can all want the
// connection, and interleaving two frames corrupts the stream.
type wsConn struct {
	conn net.Conn
	// src is where frames are read from, which is not always the connection:
	// the hijack hands over whatever the client sent in the same segment as its
	// handshake, and those bytes are in this buffer rather than on the socket.
	src io.Reader
	// done is closed by the first close. A writer checks it rather than
	// discovering the closure as an error on a frame it has already started.
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	// peerClosed is set once the client has sent its own Close frame. The
	// control handler answers that frame; close() must not answer it again.
	peerClosed atomic.Bool
}

func newWSConn(conn net.Conn, src io.Reader) *wsConn {
	return &wsConn{conn: conn, src: src, done: make(chan struct{})}
}

func (c *wsConn) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.sendBytes(b)
}

// sendBytes writes an already-marshalled payload as one text frame.
func (c *wsConn) sendBytes(b []byte) error {
	return c.write(func() error { return wsutil.WriteServerText(c.conn, b) })
}

// writeFrame writes bytes that are already a complete frame - the reply the
// control-frame handler produced for a ping or a close.
func (c *wsConn) writeFrame(frame []byte) error {
	return c.write(func() error {
		_, err := c.conn.Write(frame)
		return err
	})
}

func (c *wsConn) ping() error {
	return c.write(func() error { return ws.WriteFrame(c.conn, ws.NewPingFrame(nil)) })
}

// write is the only path to the connection. It holds the mutex for the whole
// frame and refuses a connection that is closing.
//
// The deadline is checked twice around being set. close poisons the deadline
// without waiting for this mutex, precisely so that a write to a client that
// has stopped reading cannot make shutdown wait for it - and setting a fresh
// deadline here would undo that poisoning. The second check is what catches a
// close that landed in between.
func (c *wsConn) write(frame func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing() {
		return net.ErrClosed
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	if c.closing() {
		_ = c.conn.SetDeadline(time.Now().Add(-time.Second))
		return net.ErrClosed
	}
	return frame()
}

func (c *wsConn) closing() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// stopWrites refuses further frames without touching what is on the wire. It is
// what makes close below able to wait for a frame in flight rather than cut it:
// nothing new starts, and the one thing that had started finishes.
func (c *wsConn) stopWrites() { c.once.Do(func() { close(c.done) }) }

// shutdown unblocks whatever this connection is doing, without waiting for the
// write mutex. A deadline in the past ends a read and a write that are already
// in flight - which cuts that frame in half, so it is for the shutdown path
// alone: see closeAll, where the alternative is one write timeout per
// connection instead of one for all of them.
func (c *wsConn) shutdown() {
	c.stopWrites()
	_ = c.conn.SetDeadline(time.Now().Add(-time.Second))
}

// close ends the connection without cutting a frame in half.
//
// The mutex is taken before anything touches the socket, so a frame the pump
// has already begun goes out whole. Closing under it - which is what this used
// to do, by poisoning the deadline first - leaves the client reading the tail
// of that frame as a header, which a browser reports as a reserved opcode and
// a failed connection rather than as the run it was watching ending. It cost
// about one socket in twelve when a run was closed with tabs attached.
//
// The wait is bounded by the deadline every write sets, and the shutdown path
// has poisoned those already, so Close still costs one timeout rather than one
// per connection.
//
// A close frame goes first when there is still a connection to put it on, so a
// page sees a clean close rather than a dropped socket. It is best-effort: on
// the shutdown path the deadline is already in the past and it simply fails.
func (c *wsConn) close() {
	c.stopWrites()
	if !c.lockBy(time.Now().Add(wsCloseWait)) {
		// A frame that has not finished in this long is going to somebody who
		// has stopped reading, and waiting out its whole deadline would hold a
		// goroutine on a connection that is already gone. Cutting it is the
		// worse answer only while there is a client left to notice.
		c.shutdown()
		c.mu.Lock()
	}
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(wsCloseWait))
	if !c.peerClosed.Load() {
		_ = ws.WriteFrame(c.conn, ws.NewCloseFrame(ws.NewCloseFrameBody(ws.StatusNormalClosure, "")))
	}
	_ = c.conn.SetDeadline(time.Now().Add(-time.Second))
	_ = c.conn.Close()
}

// lockBy takes the write mutex, giving up at the deadline. A frame going to a
// client that is reading finishes in microseconds; one going to a client that
// is not would otherwise hold this for the whole write timeout.
func (c *wsConn) lockBy(deadline time.Time) bool {
	for {
		if c.mu.TryLock() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// pump writes what a stream queues until the connection ends, and pings every
// so often while the stream is quiet, so that a peer which has gone away is
// found within wsIdleTimeout rather than held until the process exits.
//
// stop ends it from the stream's side; a nil channel never does. The control
// stream uses it to end a socket the hub has given up on.
func pump[T any](c *wsConn, msgs <-chan T, send func(T) error, stop <-chan struct{},
	every time.Duration,
) {
	ping := time.NewTicker(every)
	defer ping.Stop()
	for {
		select {
		case v, ok := <-msgs:
			if !ok {
				return
			}
			if err := send(v); err != nil {
				return
			}
		case <-ping.C:
			if err := c.ping(); err != nil {
				return
			}
		case <-stop:
			return
		case <-c.done:
			return
		}
	}
}

// readClientOps hands each text frame to apply and writes back whatever apply
// returns, until the client disconnects. A message apply rejects is answered
// rather than fatal: one bad frame from a reconnecting phone should not take
// the stream down with it.
//
// apply is given the whole message and decodes its own shape, so a stream with
// a richer vocabulary does not make this function grow a field per stream.
//
// The reader is built here rather than taken from wsutil.ReadClientData,
// which answers a protocol ping by writing straight to the connection from this
// goroutine - past the mutex the whole type exists to hold, and with whatever
// deadline the writer last left behind.
func (c *wsConn) readClientOps(apply func(data []byte) any) {
	var reply bytes.Buffer
	control := func(h ws.Header, r io.Reader) error {
		reply.Reset()
		err := wsutil.ControlHandler{
			Src:                 r,
			Dst:                 &reply,
			State:               ws.StateServerSide,
			DisableSrcCiphering: true,
		}.Handle(h)
		// Buffered and then written whole, under the write lock: a pong put
		// straight on the connection could land inside a frame the pump is
		// partway through.
		if reply.Len() > 0 {
			if werr := c.writeFrame(reply.Bytes()); werr != nil {
				return werr
			}
		}
		var closed wsutil.ClosedError
		if errors.As(err, &closed) {
			c.peerClosed.Store(true)
		}
		return err
	}
	rd := &wsutil.Reader{
		Source:         c.src,
		State:          ws.StateServerSide,
		CheckUTF8:      true,
		MaxFrameSize:   wsMaxFrame,
		OnIntermediate: control,
	}
	for {
		// Refreshed per frame, so the timeout measures silence rather than the
		// life of a connection that is working perfectly well.
		if err := c.conn.SetReadDeadline(time.Now().Add(wsIdleTimeout)); err != nil {
			return
		}
		hdr, err := rd.NextFrame()
		if err != nil {
			return
		}
		if hdr.OpCode.IsControl() {
			if err := control(hdr, rd); err != nil {
				return
			}
			continue
		}
		if hdr.OpCode != ws.OpText {
			if err := rd.Discard(); err != nil {
				return
			}
			continue
		}
		// MaxFrameSize bounds one frame; a message split across continuation
		// frames is bounded only here.
		data, err := io.ReadAll(io.LimitReader(rd, wsMaxMessage+1))
		if err != nil || len(data) > wsMaxMessage {
			return
		}
		if out := apply(data); out != nil {
			if err := c.send(out); err != nil {
				return
			}
		}
	}
}

// hijacked is the set of connections net/http has handed over. http.Server.Close
// closes listeners and the connections it still owns, and a hijacked one is
// neither, so without this a shutdown leaves every open socket - and the
// goroutines and socket-cap slots behind it - running until the process exits.
type hijacked struct {
	mu     sync.Mutex
	closed bool
	conns  map[*wsConn]struct{}
}

// add registers a connection and reports whether the daemon is still serving.
// A false answer means Close has already run, and the caller must close what it
// just accepted rather than hold a connection nothing will ever shut down.
func (h *hijacked) add(c *wsConn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.conns[c] = struct{}{}
	return true
}

func (h *hijacked) remove(c *wsConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, c)
}

// closeAll ends every registered connection and refuses later ones. Calling it
// twice is safe, which is part of what makes Server.Close idempotent.
//
// Unblocking every connection before closing any of them is not tidiness: close
// waits on a connection's write mutex, and a frame in flight to a client that
// has stopped reading holds it until that frame's deadline. Poisoning them all
// first makes a shutdown cost one such deadline rather than one per connection.
func (h *hijacked) closeAll() {
	h.mu.Lock()
	h.closed = true
	conns := make([]*wsConn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.conns = nil
	h.mu.Unlock()

	for _, c := range conns {
		c.shutdown()
	}
	// Outside the lock: a handler unblocked by the close calls remove on its
	// way out.
	for _, c := range conns {
		c.close()
	}
}

// upgrade completes the handshake and registers the connection. It answers the
// request itself on failure, so a caller that gets no connection back is done.
func (s *Server) upgrade(w http.ResponseWriter, r *http.Request) (*wsConn, bool) {
	conn, rw, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		// It hijacks before it validates anything, so it hands back a live
		// connection along with the refusal it has already written on it.
		// Nothing else can reach that connection: net/http has given it up, and
		// it never reached the registry Close walks, so not closing it here
		// leaks the descriptor for the life of the process.
		if conn != nil {
			_ = conn.Close()
		}
		return nil, false
	}
	// The buffer wraps the connection, so it is the whole stream and not just
	// the part that arrived early.
	var src io.Reader = conn
	if rw != nil && rw.Reader != nil {
		src = rw.Reader
	}
	c := newWSConn(conn, src)
	if !s.conns.add(c) {
		c.close()
		return nil, false
	}
	return c, true
}
