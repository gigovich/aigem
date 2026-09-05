package web

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// The runs API: the collection a page lists, the record it opens, and the
// timeline it reads back. Everything that changes a run goes through here
// rather than up a socket, so every mutation has a status code, an error body
// and an httptest that drives it.

const (
	// maxRunBody bounds a create request. Everything in one is a short string,
	// and without a bound a signed-in page's mistake is the daemon's memory.
	maxRunBody = 16 << 10
	// maxEventPage bounds one page of a timeline. A long conversation is read
	// in pages: a client that gets a full one asks again from the last sequence
	// it saw, which is the same loop it uses to catch up after a disconnect.
	maxEventPage = 2000
)

// handleRuns lists the conversations, oldest first.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.backend.Runs(r.Context())
	if err != nil {
		writeRunError(w, "listing the runs", err)
		return
	}
	if runs == nil {
		// A JSON client asked for a collection: an empty one is [] and never
		// null, or every caller needs a null check the wire could have spared.
		runs = []Run{}
	}
	writeJSON(w, runs)
}

// handleOpenRun starts a conversation.
func (s *Server) handleOpenRun(w http.ResponseWriter, r *http.Request) {
	var req NewRun
	if err := decodeJSON(w, r, &req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	run, err := s.backend.OpenRun(r.Context(), req)
	if err != nil {
		writeRunError(w, "opening a run", err)
		return
	}
	// Nothing is published here. Every change to a run is announced by whoever
	// made it, through Server.Publish, because most of them do not happen
	// during a request - and a route that also published would put two messages
	// on the stream for the one thing that happened.
	writeJSONStatus(w, http.StatusCreated, run)
}

// handleRun reports one conversation.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.backend.Run(r.Context(), r.PathValue("id"))
	if err != nil {
		writeRunError(w, "reading a run", err)
		return
	}
	writeJSON(w, run)
}

// handleCloseRun saves a conversation and ends its session. The record and the
// timeline stay: a run a person is done with is one they can still read, and
// the activity feed goes on referring to it.
func (s *Server) handleCloseRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.backend.CloseRun(r.Context(), id); err != nil {
		writeRunError(w, "closing a run", err)
		return
	}
	// Read back rather than assumed: what a client applies to its table is the
	// record as it now stands, and this daemon is not the only writer of it.
	run, err := s.backend.Run(r.Context(), id)
	if err != nil {
		writeRunError(w, "reading a closed run", err)
		return
	}
	writeJSON(w, run)
}

// handleRunEvents serves a page of a run's timeline.
//
// It is what a client reads after a desync, and what renders a run whose
// session is gone: a closed run has no socket to open, and its journal is the
// whole of what is left.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	since, ok := cursor(w, r, "since")
	if !ok {
		return
	}
	limit, ok := count(w, r, "limit", maxEventPage)
	if !ok {
		return
	}
	events, err := s.backend.RunEvents(r.Context(), r.PathValue("id"), since, limit)
	if err != nil {
		writeRunError(w, "reading a run's events", err)
		return
	}
	if events == nil {
		events = []RunEvent{}
	}
	writeJSON(w, events)
}

// handleRunBlob serves the whole body of a tool result the timeline shows only
// the head of.
//
// It is the one route here whose success is not JSON. What it returns is a
// tool's own output - a diff, a build log, a page of grep - which is an opaque
// document rather than a structure: there is no field to add to it later, and
// nothing for a client to read out of it that the event did not already carry
// in bytes and blob. Served as text it is also what a person can open in a tab.
//
// Handing a browser a tool's own output is safe because the wrapper puts the
// security headers on every response: nosniff is what stops this being pulled
// in cross-origin as a script or a stylesheet, and the content policy stops
// anything in it running if it is opened directly.
func (s *Server) handleRunBlob(w http.ResponseWriter, r *http.Request) {
	seq, ok := pathCursor(w, r, "seq")
	if !ok {
		return
	}
	body, err := s.backend.RunBlob(r.Context(), r.PathValue("id"), seq)
	if err != nil {
		writeRunError(w, "reading a stored tool result", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = io.WriteString(w, body)
}

// handleRunArtifacts reports the files a run changed.
func (s *Server) handleRunArtifacts(w http.ResponseWriter, r *http.Request) {
	arts, err := s.backend.RunArtifacts(r.Context(), r.PathValue("id"))
	if err != nil {
		writeRunError(w, "reading a run's artifacts", err)
		return
	}
	if arts == nil {
		arts = []Artifact{}
	}
	writeJSON(w, arts)
}

// decodeJSON reads a bounded request body into v and refuses anything but one
// JSON document. A second document in the same body would be a field the
// server silently ignored.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRunBody))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("the body holds more than one JSON document")
	}
	return nil
}

// cursor reads a resume point. Every cursor on this API is a non-negative JSON
// number, opaque to the client and compared only for equality and order, so
// the one parser is the one rule.
func cursor(w http.ResponseWriter, r *http.Request, name string) (uint64, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, true
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		http.Error(w, name+" must be a non-negative whole number", http.StatusBadRequest)
		return 0, false
	}
	return n, true
}

// pathCursor reads a cursor that arrives as a path segment rather than as a
// query parameter. It is the same rule and the same sentence as cursor: a
// client that mistypes a sequence should not have to learn which half of the
// URL it was in to recognise the answer.
func pathCursor(w http.ResponseWriter, r *http.Request, name string) (uint64, bool) {
	n, err := strconv.ParseUint(r.PathValue(name), 10, 64)
	if err != nil {
		http.Error(w, name+" must be a non-negative whole number", http.StatusBadRequest)
		return 0, false
	}
	return n, true
}

// count reads a page size, clamped to most. Zero and an absent value both mean
// "as much as you will give me", which is most.
func count(w http.ResponseWriter, r *http.Request, name string, most int) (int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return most, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	// A number too large for the type is still a number, and it means the same
	// thing every other number past the cap means. Refusing it would be the one
	// value of "as much as you will give me" that is an error.
	if errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(raw, "-") {
		return most, true
	}
	if err != nil || n < 0 {
		http.Error(w, name+" must be a non-negative whole number", http.StatusBadRequest)
		return 0, false
	}
	if n == 0 || n > int64(most) {
		return most, true
	}
	return int(n), true
}

// writeRunError turns a backend error into a status code.
//
// The sentinels are the contract; a *Refusal is a sentence written for the
// person at the browser, and everything else is the daemon's own fault and is
// answered without a detail, because an error assembled somewhere inside the
// agent is not something anyone at a browser can act on.
//
// A Refusal does carry whatever the backend put in it, and a backend that
// classifies broadly will put a filesystem path or a provider's message there.
// That is the deliberate trade: this daemon serves one signed-in person on
// their own machine, and hiding "run `aigem auth login openai`" behind "the
// daemon could not carry that out" leaves them with a button that stopped
// working and no way to find out why.
func writeRunError(w http.ResponseWriter, doing string, err error) {
	var refusal *Refusal
	switch {
	case errors.Is(err, ErrNoRun):
		http.Error(w, "no such run", http.StatusNotFound)
	case errors.Is(err, ErrNoBlob):
		http.Error(w, "that event has no stored body", http.StatusNotFound)
	case errors.Is(err, ErrRunClosed):
		http.Error(w, "this run has no live session", http.StatusConflict)
	case errors.Is(err, ErrHistoryGone):
		http.Error(w, "the run's history no longer reaches that point; reload it",
			http.StatusGone)
	case errors.Is(err, ErrBusy):
		// The message rather than a fixed sentence: it says how much is open and
		// what the limit is, which is what turns "try later" into "close one".
		w.Header().Set("Retry-After", "5")
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case errors.As(err, &refusal):
		http.Error(w, refusal.Reason, http.StatusBadRequest)
	default:
		slog.Error("the daemon failed a request about a run", "doing", doing, "err", err)
		http.Error(w, "the daemon could not carry that out", http.StatusInternalServerError)
	}
}
