package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
)

// The run stream: one socket per open run per tab, carrying the session's own
// events down verbatim and the client's operations up.
//
// It is the opposite of the control stream in both directions. This one
// replays - a client resumes from the last sequence it saw, and the session
// splices the backlog in front of the live events so there is no window where
// an event is neither replayed nor delivered - and this one takes mutations,
// because the operations here are the conversation itself: a message, an
// answer to an approval, an interrupt. Routing those over HTTP would put a
// request per keystroke-sized action next to a socket that is already open and
// already ordered.
//
// What goes down is uisession's event, encoded by the backend and written here
// untouched. This package does not know the vocabulary and must not learn it: a
// second copy of it would be a second thing to keep in step with the session,
// and the drift would show up as a browser rendering a conversation that did
// not happen.

// kindClientError names the frame that is not an event. It is deliberately not
// "error": that is a real event kind, and naming it the same made every client
// mistake look like something that happened in the conversation - putting
// "approval already decided" into the timeline as a failure at exactly the
// moment the design says it must not be one.
const kindClientError = "client_error"

// wsError is sent when an operation cannot be carried out.
type wsError struct {
	Kind  string `json:"kind"`
	Op    string `json:"op,omitempty"`
	Error string `json:"error"`
}

// opPing is the one operation this package answers itself. A client behind a
// proxy with an idle timeout needs something to send, and it must not be a
// message to the model.
const opPing = "ping"

// runOps is the vocabulary a client may use. It is a set here rather than a
// switch in the backend so that a typo is refused by name, before it reaches a
// session, and so that what the transport advertises is what the backend
// implements.
//
// "stop" is deliberately absent: stopping a run is bound up with discarding its
// worktree, and neither exists yet. A client that sends it is told the operation
// is unknown, which is true, rather than being quietly ignored.
var runOps = map[string]bool{
	"submit":       true,
	"interrupt":    true,
	"resolve":      true,
	"command":      true,
	"step_mode":    true,
	"switch_model": true,
}

// handleRunSocket attaches one client to a run for as long as it stays
// connected. It is what keeps a terminal and a browser looking at the same
// conversation rather than at two renderings that have drifted.
func (s *Server) handleRunSocket(w http.ResponseWriter, r *http.Request) {
	// Refused here rather than by the upgrade: gobwas hijacks before it
	// validates and writes its own refusal straight onto the connection, past
	// the wrapper that puts the security headers on every other response. A
	// signed-in browser following this URL is exactly that request.
	if !isUpgrade(r) {
		http.Error(w, "this route is a websocket", http.StatusBadRequest)
		return
	}
	since, ok := cursor(w, r, "since")
	if !ok {
		return
	}
	id := r.PathValue("id")
	client := RunClient{
		Kind:  firstNonEmpty(r.URL.Query().Get("kind"), "web"),
		Label: r.URL.Query().Get("label"),
	}

	// Attached before the hijack, so a run that is gone, closed, or past the
	// point the client asked to resume from is a status code the browser's
	// fetch can read - rather than a socket that opens and immediately says
	// nothing.
	stream, err := s.backend.WatchRun(r.Context(), id, client, since)
	if err != nil {
		writeRunError(w, "attaching to a run", err)
		return
	}
	// Armed before anything that can unwind past it. The backend is an
	// interface implemented outside this package, so a panic in it is a live
	// possibility, and net/http recovering one would leave the session
	// publishing to a subscriber nobody will ever read.
	defer stream.Close()

	c, ok := s.upgrade(w, r)
	if !ok {
		return
	}
	// Closed as well as unregistered: the registry is the only other thing that
	// could ever close it, so unwinding past here without both would leave the
	// connection and the goroutine behind it for the life of the process.
	defer func() {
		c.close()
		s.conns.remove(c)
	}()

	// Either end can finish first, and each has to unblock the other. A client
	// that disconnects ends the reader; a run that closes - or a daemon shutting
	// down - ends the writer, and a hijacked connection is no longer the http
	// server's to close.
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The payload is written as it came: it was encoded by the session, and
		// decoding it here to re-encode it would be this package taking a view
		// on a vocabulary it deliberately does not have.
		pump(c, stream.Events(), func(ev RunEvent) error { return c.sendBytes(ev.Data) },
			nil, wsPingInterval)
		c.close()
	}()
	c.readClientOps(func(data []byte) any { return s.runOp(r.Context(), id, client, data) })
	c.close()
	<-done
}

// runOp applies one client operation and returns what to answer with, or nil
// when there is nothing to say. A message it rejects is answered rather than
// fatal: one bad frame from a reconnecting phone should not take the
// conversation down with it.
func (s *Server) runOp(ctx context.Context, id string, client RunClient, data []byte) any {
	var op RunOp
	if err := json.Unmarshal(data, &op); err != nil {
		return wsError{Kind: kindClientError, Error: "bad message: " + err.Error()}
	}
	if op.Op == opPing {
		return nil
	}
	if !runOps[op.Op] {
		return wsError{Kind: kindClientError, Op: op.Op,
			Error: "unknown op " + strconv.Quote(op.Op)}
	}
	if op.Label == "" {
		// Who answered an approval is shown to the other clients, and a client
		// that did not name itself is still somebody.
		op.Label = client.Kind
	}
	if err := s.backend.ApplyRunOp(ctx, id, op); err != nil {
		return wsError{Kind: kindClientError, Op: op.Op, Error: opReason(op.Op, err)}
	}
	return nil
}

// opReason is what a client is told about a refused operation. It mirrors
// writeRunError: the sentinels have wording of their own, a *Refusal is a
// sentence written to be read, and anything else is the daemon's own fault and
// is not described.
func opReason(op string, err error) string {
	var refusal *Refusal
	switch {
	case errors.Is(err, ErrNoRun):
		return "no such run"
	case errors.Is(err, ErrRunClosed):
		return "this run has no live session"
	case errors.As(err, &refusal):
		return refusal.Reason
	default:
		slog.Error("the daemon failed a run operation", "op", op, "err", err)
		return "the daemon could not carry that out"
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
