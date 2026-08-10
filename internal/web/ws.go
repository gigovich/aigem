package web

import (
	"encoding/json"
	"net"
	"net/http"
	"sync"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/uisession"
)

// clientOp is what a front-end sends up. Everything a client can do to a
// session is one of these; the stream going down is the session's events,
// verbatim.
type clientOp struct {
	Op   string `json:"op"`
	Text string `json:"text,omitempty"`
	// Images are data the client attached to a message, base64-encoded with
	// their media type, as the model API expects them.
	Images []llm.Image `json:"images,omitempty"`

	ID       string             `json:"id,omitempty"`
	Decision uisession.Decision `json:"decision,omitempty"`
	Name     string             `json:"name,omitempty"`
	Args     string             `json:"args,omitempty"`
	Label    string             `json:"label,omitempty"`
}

// wsError is sent when an op cannot be carried out. It is deliberately not an
// Event: events are what happened in the session, and one client's bad request
// did not happen in the session.
//
// The kind is client_error rather than error, because error is a real Event
// kind - naming it that made every client mistake a rejected request for
// something that happened in the conversation, and put "approval already
// decided" into the timeline as a failure at exactly the moment the design says
// it must not be one.
type wsError struct {
	Kind  string `json:"kind"`
	Op    string `json:"op,omitempty"`
	Error string `json:"error"`
}

const kindClientError = "client_error"

// handleSocket attaches one client to a session for as long as it stays
// connected. Everything it is sent comes from the session's own stream, which
// is what keeps a terminal and a browser looking at the same conversation
// rather than at two renderings that drifted.
func (s *Server) handleSocket(w http.ResponseWriter, r *http.Request) {
	if !s.originOK(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	if !tokenOK(s.token, requestToken(r)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	e, ok := s.lookup(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	client := uisession.Client{
		Kind:  firstNonEmpty(r.URL.Query().Get("kind"), "web"),
		Label: r.URL.Query().Get("label"),
	}
	events, detach, err := e.sess.Subscribe(client, sinceParam(r))
	if err != nil {
		// The client asked to resume from a point no longer available: it has to
		// reload rather than render a hole.
		http.Error(w, err.Error(), http.StatusGone)
		return
	}

	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		detach()
		return
	}
	c := &wsConn{conn: conn}
	// Either end can finish first, and each has to unblock the other. A client
	// that disconnects ends the reader, and detaching closes the event channel
	// under the writer. A session that closes - the daemon shutting down - ends
	// the writer first, and the reader would otherwise sit on a read that never
	// returns, because a hijacked connection is no longer the http server's to
	// close. So whichever ends closes the connection.
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.writeLoop(events)
		c.close()
	}()
	c.readLoop(e.sess, client.Kind)
	detach()
	<-done
	c.close()
}

// wsConn serialises writes. The event pump and a reply to a bad op can both
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

func (c *wsConn) writeLoop(events <-chan uisession.Event) {
	for ev := range events {
		if err := c.send(ev); err != nil {
			return
		}
	}
}

// readLoop applies the client's ops until it disconnects. A malformed frame is
// answered rather than fatal: one bad message from a reconnecting phone should
// not take the conversation down with it.
func (c *wsConn) readLoop(sess *uisession.Local, kind string) {
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
			_ = c.send(wsError{Kind: "error", Error: "bad message: " + err.Error()})
			continue
		}
		if err := apply(sess, in, kind); err != nil {
			_ = c.send(wsError{Kind: kindClientError, Op: in.Op, Error: err.Error()})
		}
	}
}

func apply(sess *uisession.Local, in clientOp, kind string) error {
	switch in.Op {
	case "submit":
		return sess.Submit(in.Text, in.Images)
	case "interrupt":
		sess.Interrupt()
		return nil
	case "resolve":
		by := in.Label
		if by == "" {
			by = kind
		}
		return sess.Resolve(in.ID, in.Decision, by)
	case "command":
		return sess.Command(in.Name, in.Args)
	case "ping":
		return nil
	default:
		return errUnknownOp{in.Op}
	}
}

type errUnknownOp struct{ op string }

func (e errUnknownOp) Error() string { return "unknown op " + e.op }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
