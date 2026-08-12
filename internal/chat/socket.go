package chat

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"

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
	// Await marks a message as one the operator is owed an answer to. It exists
	// on the op because the operator can ask a question too, and the thread
	// should say so.
	Await       bool     `json:"await,omitempty"`
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

// socket is the one connection the browser keeps.
//
// One socket, not one per thread: a phone gets a handful of sockets and a
// reconnect storm per socket, and the inbox has to stay live while a thread is
// open. Message and thread frames arrive for every thread the operator is in;
// timeline events arrive only for the thread the client says it is watching.
func (a *API) socket(w http.ResponseWriter, r *http.Request) {
	since := uintParam(r.URL.Query().Get("since"))

	// Read the backlog before attaching, and attach before returning, so there
	// is no window in which a frame is neither replayed nor delivered.
	backlog, cursor, more, err := a.store.Tail(r.Context(), Operator, since, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	id, frames, detach := a.hub.Attach(Operator, backlog)

	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		detach()
		return
	}
	// The request context is cancelled the moment the connection is hijacked,
	// but the ops that arrive over it are real writes that must not be born
	// already cancelled. WithoutCancel keeps the request's values and drops its
	// deadline; the socket's own lifetime is the bound.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	c := &wsConn{conn: conn}
	// A client whose backlog did not fit is told where it got to, so it asks for
	// the rest rather than assuming it has everything.
	if more {
		_ = c.send(Frame{Seq: cursor, Stream: StreamDesync, From: cursor})
	}

	// Either end can finish first, and each has to unblock the other: a client
	// that disconnects ends the reader, and detaching closes the frame channel
	// under the writer. Whichever ends closes the connection.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for f := range frames {
			if err := c.send(f); err != nil {
				return
			}
		}
		c.close()
	}()
	c.readLoop(ctx, a, id)
	detach()
	<-done
	c.close()
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

	backlog, err := a.store.Timeline(r.Context(), Operator, threadID, since, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	id, frames, detach := a.hub.Attach(Operator, backlog)
	a.hub.Watch(id, threadID)

	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		detach()
		return
	}
	c := &wsConn{conn: conn}
	for f := range frames {
		if f.Stream != StreamEvent || f.ThreadID != threadID {
			continue
		}
		if err := c.sendRaw(f.Event); err != nil {
			break
		}
	}
	detach()
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

// readLoop applies the client's ops until it disconnects. A malformed frame is
// answered rather than fatal: one bad message from a reconnecting phone should
// not take the conversation down with it.
func (c *wsConn) readLoop(ctx context.Context, a *API, id uint64) {
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
		if err := a.apply(ctx, id, in); err != nil {
			_ = c.send(wsError{Kind: kindClientError, Op: in.Op, Error: err.Error()})
		}
	}
}

func (a *API) apply(ctx context.Context, id uint64, in clientOp) error {
	switch in.Op {
	case "send":
		_, err := a.store.Say(ctx, in.Thread, Draft{
			Author: Operator, Body: in.Text, Mentions: in.Mentions,
			AwaitReply: in.Await, Attachments: in.Attachments,
		})
		return err
	case "watch":
		a.hub.Watch(id, in.Thread)
		return nil
	case "unwatch":
		a.hub.Watch(id, "")
		return nil
	case "read":
		return a.store.MarkRead(ctx, Operator, in.Thread, in.Seq)
	case "add":
		return a.store.AddParticipant(ctx, Operator, in.Thread, in.Actor)
	case "remove":
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
