package chat

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// clientOp is what a client sends up. Everything it can do to the fleet's
// conversations is one of these; the stream going down is frames.
type clientOp struct {
	Op     string `json:"op"`
	Thread string `json:"thread,omitempty"`
	Text   string `json:"text,omitempty"`
	Actor  string `json:"actor,omitempty"`
	Seq    uint64 `json:"seq,omitempty"`
	Title  string `json:"title,omitempty"`
	// There is deliberately no await flag. It marks a thread as owing the
	// operator an answer, and only a bot can put a thread into that state - the
	// operator asking a question would be demanding their own attention.
	Mentions    []string `json:"mentions,omitempty"`
	Attachments []string `json:"attachments,omitempty"`
	Archived    bool     `json:"archived,omitempty"`
}

// wsError is sent when an op cannot be carried out. It is deliberately not a
// Frame: frames are what happened in the conversation, and one client's bad
// request did not happen in the conversation.
type wsError struct {
	Kind  string `json:"kind"`
	Op    string `json:"op,omitempty"`
	Error string `json:"error"`
}

const kindClientError = "client_error"

// writeTimeout bounds one frame's write. It is generous: the point is to bound
// a stalled connection, not to police a slow one.
const writeTimeout = 30 * time.Second

// socket is the one connection the browser keeps.
//
// One socket, not one per thread: a phone gets a handful of sockets and a
// reconnect storm per socket, and the inbox has to stay live while a thread is
// open. Message and thread frames arrive for every thread the operator is in;
// timeline events arrive only for the thread the client says it is watching.
func (a *API) socket(w http.ResponseWriter, r *http.Request) {
	since := uintParam(r.URL.Query().Get("since"))

	// The hub registers first and splices the history in front of whatever
	// arrived while it was being read. Fetching first and registering after
	// would lose any write that committed in between, and for a SQLite read
	// that is a real interval rather than a theoretical one.
	var truncated uint64
	client, err := a.hub.Attach(Operator, since, func(from uint64) ([]Frame, error) {
		frames, cursor, more, err := a.store.Tail(r.Context(), Operator, from, 0)
		if more {
			truncated = cursor
		}
		return frames, err
	})
	if err != nil {
		writeErr(w, err)
		return
	}

	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		client.Detach()
		return
	}
	// The request context is cancelled the moment the connection is hijacked,
	// but the ops that arrive over it are real writes that must not be born
	// already cancelled. WithoutCancel keeps the request's values and drops its
	// deadline; the socket's own lifetime is the bound.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	c := &wsConn{conn: conn}
	// Either end can finish first, and each has to unblock the other: a client
	// that disconnects ends the reader, and detaching closes the frame channel
	// under the writer. Whichever ends closes the connection.
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The close runs on both exits. Leaving it off the error path left a
		// connection whose write half had failed but whose read half was still
		// open, parking readLoop and holding the subscriber until the client
		// went away on its own.
		defer c.close()
		c.pump(client.Frames(), truncated)
	}()
	c.readLoop(ctx, a, client)
	client.Detach()
	<-done
	c.close()
}

// pump ships frames until the stream ends, and reports whether it got that far.
//
// truncated, when non-zero, is where a backlog too large to ship in one page
// stopped. The signal goes out after the backlog, not before: the established
// client contract for a desync is "set your cursor to From and reconnect", and
// a client told that first would close the socket and throw away the very
// frames it was about to be given.
func (c *wsConn) pump(frames <-chan Frame, truncated uint64) {
	more := Frame{Stream: StreamTruncated, From: truncated}
	sent := false
	for f := range frames {
		// The backlog is everything at or below the point Tail stopped; the
		// live tail starts above it. The signal belongs between the two.
		if truncated > 0 && !sent && f.Seq > truncated {
			if err := c.send(more); err != nil {
				return
			}
			sent = true
		}
		if err := c.send(f); err != nil {
			return
		}
	}
	if truncated > 0 && !sent {
		_ = c.send(more)
	}
}

// threadSocket emits one thread's timeline as bare event JSON - the same bytes
// the session daemon's own socket emits.
//
// It exists so a terminal can render a bot's work with the renderer that
// already exists, without teaching it the frame envelope. It is deliberately
// read-only and deliberately dumb.
func (a *API) threadSocket(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("id")
	since := uintParam(r.URL.Query().Get("since"))

	// Watch before the backlog is read, so an event that lands in between is
	// queued rather than dropped by the hub's per-thread filter.
	client, err := a.hub.Attach(Operator, since, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	client.Watch(threadID)
	backlog, err := a.store.Timeline(r.Context(), Operator, threadID, since, 0)
	if err != nil {
		client.Detach()
		writeErr(w, err)
		return
	}

	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		client.Detach()
		return
	}
	c := &wsConn{conn: conn}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, f := range backlog {
			if err := c.sendRaw(f.Event); err != nil {
				return
			}
		}
		for f := range client.Frames() {
			// A drop is the one non-event worth saying out loud: the client is
			// missing history and would otherwise just see the socket close.
			if f.Stream == StreamDesync {
				_ = c.send(f)
				return
			}
			if f.Stream != StreamEvent || f.ThreadID != threadID {
				continue
			}
			if err := c.sendRaw(f.Event); err != nil {
				return
			}
		}
	}()
	// This socket is read-only, but it still has to read: without a reader, a
	// client that vanishes is only noticed on a failed write, and a quiet thread
	// never writes. Draining until the read fails is how the connection's death
	// is detected at all.
	c.drain()
	client.Detach()
	<-done
	c.close()
}

// wsConn serialises writes. The frame pump and a reply to a bad op can both
// want the connection, and interleaving two frames corrupts the stream.
type wsConn struct {
	conn net.Conn
	mu   sync.Mutex
}

func (c *wsConn) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.sendRaw(b)
}

func (c *wsConn) sendRaw(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Without a deadline a client that stops reading blocks the write in the
	// kernel while holding this mutex, and the handler then waits on a pump that
	// will not return until the TCP retransmit timeout - which is minutes.
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return wsutil.WriteServerText(c.conn, b)
}

// close ends the connection without cutting a frame in half. Closing from one
// goroutine while another is partway through a write leaves the client reading
// the tail of a frame as a header, which it reports as a reserved opcode - a
// confusing way to learn about a race.
func (c *wsConn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.Close()
}

// drain reads and discards until the connection dies. See threadSocket.
func (c *wsConn) drain() {
	for {
		if _, _, err := wsutil.ReadClientData(c.conn); err != nil {
			return
		}
	}
}

// readLoop applies the client's ops until it disconnects. A malformed frame is
// answered rather than fatal: one bad message from a reconnecting phone should
// not take the conversation down with it.
func (c *wsConn) readLoop(ctx context.Context, a *API, client *Client) {
	for {
		data, op, err := wsutil.ReadClientData(c.conn)
		if err != nil {
			return
		}
		if op != ws.OpText {
			continue
		}
		var in clientOp
		if err := json.Unmarshal(data, &in); err != nil {
			_ = c.send(wsError{Kind: kindClientError, Error: "bad message: " + err.Error()})
			continue
		}
		if err := a.apply(ctx, client, in); err != nil {
			_ = c.send(wsError{Kind: kindClientError, Op: in.Op, Error: err.Error()})
		}
	}
}

func (a *API) apply(ctx context.Context, client *Client, in clientOp) error {
	switch in.Op {
	case "send":
		_, err := a.store.Say(ctx, in.Thread, Draft{
			Author: Operator, Body: in.Text, Mentions: in.Mentions,
			Attachments: in.Attachments,
		})
		return err
	case "watch":
		client.Watch(in.Thread)
		return nil
	case "unwatch":
		client.Watch("")
		return nil
	case "read":
		return a.store.MarkRead(ctx, Operator, in.Thread, in.Seq)
	case "add":
		return a.store.AddParticipant(ctx, Operator, in.Thread, in.Actor)
	case "remove":
		// Same rule as the HTTP route: the operator has no way back into a
		// thread they left, so they cannot leave one.
		if in.Actor == Operator {
			return invalid("you cannot remove yourself from a thread")
		}
		return a.store.RemoveParticipant(ctx, Operator, in.Thread, in.Actor)
	case "title":
		return a.store.SetTitle(ctx, Operator, in.Thread, in.Title)
	case "archive":
		return a.store.SetArchived(ctx, Operator, in.Thread, in.Archived)
	case "ping":
		return nil
	default:
		return invalid("unknown op %q", in.Op)
	}
}
