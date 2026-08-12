package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// API serves the chat over HTTP. It is mounted into the daemon that already
// owns the listener, the token and the CSP, rather than standing up a second
// server with its own answers about who may connect.
//
// Every request is the operator. There is one human, authenticated by the
// daemon before anything here runs; bots reach the store directly, in process.
type API struct {
	store *Store
	hub   *Hub
}

func NewAPI(s *Store, h *Hub) *API { return &API{store: s, hub: h} }

// Mount registers the routes. guard is the daemon's wrapper: it applies the
// origin check, the token check and the security headers, so nothing here can
// answer under weaker rules than the rest of the daemon.
func (a *API) Mount(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	for pattern, h := range map[string]http.HandlerFunc{
		"GET /api/chat/threads":                          a.listThreads,
		"POST /api/chat/threads":                         a.newThread,
		"GET /api/chat/threads/{id}":                     a.getThread,
		"PATCH /api/chat/threads/{id}":                   a.patchThread,
		"DELETE /api/chat/threads/{id}":                  a.deleteThread,
		"GET /api/chat/threads/{id}/messages":            a.listMessages,
		"POST /api/chat/threads/{id}/messages":           a.postMessage,
		"GET /api/chat/threads/{id}/timeline":            a.timeline,
		"GET /api/chat/threads/{id}/turns":               a.turns,
		"GET /api/chat/threads/{id}/blobs/{seq}":         a.blob,
		"POST /api/chat/threads/{id}/participants":       a.addParticipant,
		"DELETE /api/chat/threads/{id}/participants/{a}": a.removeParticipant,
		"POST /api/chat/threads/{id}/read":               a.markRead,
		"POST /api/chat/threads/{id}/attachments":        a.putAttachment,
		"GET /api/chat/attachments/{id}":                 a.getAttachment,
		"GET /api/chat/search":                           a.search,
		"GET /api/chat/fleet":                            a.fleet,
		"GET /api/chat/socket":                           a.socket,
		"GET /api/chat/threads/{id}/socket":              a.threadSocket,
	} {
		mux.HandleFunc(pattern, guard(h))
	}
}

// ---- threads ----

func (a *API) listThreads(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if term := q.Get("q"); term != "" {
		hits, err := a.store.Search(r.Context(), Operator, term, intParam(q.Get("limit")))
		writeResult(w, hits, err)
		return
	}
	views, err := a.store.Inbox(r.Context(), Operator,
		q.Get("state"), q.Get("archived") == "true", intParam(q.Get("limit")))
	writeResult(w, views, err)
}

func (a *API) newThread(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title        string   `json:"title"`
		Participants []string `json:"participants"`
		Text         string   `json:"text"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	th, err := a.store.NewThread(r.Context(), body.Title, Operator, body.Participants)
	if err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(body.Text) != "" {
		if _, err := a.store.Say(r.Context(), th.ID,
			Draft{Author: Operator, Body: body.Text}); err != nil {
			writeErr(w, err)
			return
		}
	}
	view, err := a.store.ThreadFor(r.Context(), Operator, th.ID)
	writeResult(w, view, err)
}

func (a *API) getThread(w http.ResponseWriter, r *http.Request) {
	view, err := a.store.ThreadFor(r.Context(), Operator, r.PathValue("id"))
	writeResult(w, view, err)
}

func (a *API) patchThread(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title    *string `json:"title"`
		Archived *bool   `json:"archived"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	if body.Title != nil {
		if err := a.store.SetTitle(r.Context(), Operator, id, *body.Title); err != nil {
			writeErr(w, err)
			return
		}
	}
	if body.Archived != nil {
		if err := a.store.SetArchived(r.Context(), Operator, id, *body.Archived); err != nil {
			writeErr(w, err)
			return
		}
	}
	view, err := a.store.ThreadFor(r.Context(), Operator, id)
	writeResult(w, view, err)
}

func (a *API) deleteThread(w http.ResponseWriter, r *http.Request) {
	writeErrOrNoContent(w, a.store.DeleteThread(r.Context(), Operator, r.PathValue("id")))
}

// ---- messages ----

func (a *API) listMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	msgs, err := a.store.Messages(r.Context(), Operator, r.PathValue("id"),
		uintParam(q.Get("before")), intParam(q.Get("limit")))
	writeResult(w, msgs, err)
}

func (a *API) postMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text        string   `json:"text"`
		Mentions    []string `json:"mentions"`
		Attachments []string `json:"attachments"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	m, err := a.store.Say(r.Context(), r.PathValue("id"), Draft{
		Author: Operator, Body: body.Text,
		Mentions: body.Mentions, Attachments: body.Attachments,
	})
	writeResult(w, m, err)
}

func (a *API) timeline(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	frames, err := a.store.Timeline(r.Context(), Operator, r.PathValue("id"),
		uintParam(q.Get("since")), intParam(q.Get("limit")))
	writeResult(w, frames, err)
}

func (a *API) turns(w http.ResponseWriter, r *http.Request) {
	turns, err := a.store.Turns(r.Context(), Operator, r.PathValue("id"))
	writeResult(w, turns, err)
}

// blob serves the untruncated body of an oversized tool result as plain text,
// exactly as the session daemon serves its own.
func (a *API) blob(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if err != nil {
		writeErr(w, invalid("blob sequence must be a number"))
		return
	}
	body, err := a.store.Blob(r.Context(), Operator, r.PathValue("id"), seq)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(body)
}

func (a *API) markRead(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seq uint64 `json:"seq"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	writeErrOrNoContent(w, a.store.MarkRead(r.Context(), Operator, r.PathValue("id"), body.Seq))
}

// ---- participants ----

func (a *API) addParticipant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Actor string `json:"actor"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	writeErrOrNoContent(w,
		a.store.AddParticipant(r.Context(), Operator, r.PathValue("id"), body.Actor))
}

func (a *API) removeParticipant(w http.ResponseWriter, r *http.Request) {
	writeErrOrNoContent(w,
		a.store.RemoveParticipant(r.Context(), Operator, r.PathValue("id"), r.PathValue("a")))
}

// ---- attachments ----

// maxUploadBytes bounds the whole multipart body, which is the attachment plus
// its headers. MaxBytesReader answers a request that runs past it rather than
// reading it, so an upload cannot be used to fill the daemon's memory.
const maxUploadBytes = MaxAttachmentBytes + (1 << 16)

func (a *API) putAttachment(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, invalid("expected a multipart body with a \"file\" part: %v", err))
		return
	}
	defer func() { _ = file.Close() }()

	att, err := a.store.PutAttachment(r.Context(), Operator, r.PathValue("id"),
		header.Filename, file)
	writeResult(w, att, err)
}

// getAttachment serves a stored file.
//
// It is the one route that returns bytes an outsider chose, so it is served
// under its own tighter policy: sandboxed, nothing loadable, and anything that
// is not an image the browser can safely render is a download rather than a
// page. The daemon's img-src 'self' already covers the images, because they
// come from this origin.
func (a *API) getAttachment(w http.ResponseWriter, r *http.Request) {
	att, body, err := a.store.Attachment(r.Context(), Operator, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if renderableImage[att.Mime] {
		w.Header().Set("Content-Type", att.Mime)
		w.Header().Set("Content-Disposition",
			mime.FormatMediaType("inline", map[string]string{"filename": att.Filename}))
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition",
			mime.FormatMediaType("attachment", map[string]string{"filename": att.Filename}))
	}
	_, _ = w.Write(body)
}

// renderableImage is what may be shown inline. It is the sniffed type, not the
// uploaded one, and it is deliberately not the full image list: SVG is a
// document that can carry script, so it downloads.
var renderableImage = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

// ---- misc ----

func (a *API) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	hits, err := a.store.Search(r.Context(), Operator, q.Get("q"), intParam(q.Get("limit")))
	writeResult(w, hits, err)
}

func (a *API) fleet(w http.ResponseWriter, r *http.Request) {
	actors, err := a.store.Actors(r.Context())
	writeResult(w, actors, err)
}

// ---- helpers ----

// maxJSONBytes bounds a request body. The store bounds what it stores, but a
// handler that read an unbounded body first would already have paid for it.
const maxJSONBytes = MaxBodyBytes + (1 << 16)

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, invalid("bad request body: %v", err))
		return false
	}
	return true
}

func writeResult(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func writeErrOrNoContent(w http.ResponseWriter, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeErr maps the store's two sentinels onto status codes. That mapping is
// the whole reason they exist: without them this would be a string match
// against a growing list of messages.
func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrNoSuchThread), errors.Is(err, ErrNoSuchTurn):
		status = http.StatusNotFound
	case errors.Is(err, ErrInvalid):
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, fmt.Sprintf("chat: encode response: %v", err),
			http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// intParam and uintParam read an optional numeric query parameter. A value that
// is not a number is zero, which every caller treats as "unset" - a 400 for a
// malformed limit would be a worse answer than the default page.
func intParam(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func uintParam(s string) uint64 {
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}
