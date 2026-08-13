package chat

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// testAPI stands up the store, the hub and the routes on a real listener, with
// the same no-op guard the daemon's own tests use in place of the token check.
// socketWait bounds a read in these tests. It is generous rather than tight:
// the thing being asserted is that a frame arrives at all, and a machine also
// running the frontend suite has taken longer than three seconds to schedule
// the goroutine that writes it - which failed as "no frame" and pointed at the
// hub rather than at the load.
const socketWait = 15 * time.Second

func testAPI(t *testing.T) (*Store, *httptest.Server) {
	t.Helper()
	s := newStore(t)
	hub := NewHub()
	_ = s.AddPublisher("hub", hub.Publish)

	mux := http.NewServeMux()
	NewAPI(s, hub).Mount(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

func do(t *testing.T, srv *httptest.Server, method, path string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func decode[T any](t *testing.T, res *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatalf("decoding %s: %v", res.Request.URL.Path, err)
	}
	return v
}

// The client draws its filters from States and validates a message against the
// limits before sending it. A state the store can produce but this list omits
// is a thread the operator has no filter for; a limit that drifts is a write
// the daemon refuses after the composer accepted it.
func TestMetaCarriesEveryStateAndLimit(t *testing.T) {
	_, srv := testAPI(t)

	meta := decode[Meta](t, do(t, srv, http.MethodGet, "/api/chat/meta", nil))
	for _, state := range []string{StateNeedsYou, StateWorking, StateWaiting, StateIdle} {
		if !slices.Contains(meta.States, state) {
			t.Errorf("meta omits the %q state the store can put on a thread", state)
		}
	}
	if meta.Operator != Operator {
		t.Errorf("meta names the operator %q, want %q", meta.Operator, Operator)
	}
	for _, l := range []struct {
		name      string
		got, want int
	}{
		{"max_body_bytes", meta.MaxBodyBytes, MaxBodyBytes},
		{"max_title_chars", meta.MaxTitleChars, MaxTitleChars},
		{"max_unread", meta.MaxUnread, MaxUnread},
	} {
		if l.got != l.want {
			t.Errorf("meta reports %s = %d, but the store enforces %d", l.name, l.got, l.want)
		}
	}
}

func TestAPIThreadLifecycle(t *testing.T) {
	_, srv := testAPI(t)

	res := do(t, srv, http.MethodPost, "/api/chat/threads", map[string]any{
		"title": "refresh-token rotation", "participants": []string{amiran}, "text": "the logout is back",
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("creating a thread answered %d", res.StatusCode)
	}
	th := decode[ThreadView](t, res)
	if th.Title != "refresh-token rotation" {
		t.Fatalf("title = %q", th.Title)
	}
	if len(th.Participants) != 2 {
		t.Fatalf("participants = %v, want the operator and amiran", th.Participants)
	}

	res = do(t, srv, http.MethodGet, "/api/chat/threads", nil)
	if list := decode[[]ThreadView](t, res); len(list) != 1 || list[0].ID != th.ID {
		t.Fatalf("the inbox does not list the new thread: %+v", list)
	}

	res = do(t, srv, http.MethodGet, "/api/chat/threads/"+th.ID+"/messages", nil)
	page := decode[Page[Message]](t, res)
	if len(page.Items) != 1 || page.Items[0].Body != "the logout is back" {
		t.Fatalf("the opening text was not stored: %+v", page.Items)
	}
	if page.More || page.Cursor != 0 {
		t.Fatalf("a one-message thread paged: more=%v cursor=%d", page.More, page.Cursor)
	}

	res = do(t, srv, http.MethodPatch, "/api/chat/threads/"+th.ID,
		map[string]any{"title": "renamed", "archived": true})
	patched := decode[ThreadView](t, res)
	if patched.Title != "renamed" || !patched.Archived {
		t.Fatalf("patch did not apply: %+v", patched)
	}

	res = do(t, srv, http.MethodDelete, "/api/chat/threads/"+th.ID, nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete answered %d, want 204", res.StatusCode)
	}
	res = do(t, srv, http.MethodGet, "/api/chat/threads/"+th.ID, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("a deleted thread answered %d, want 404", res.StatusCode)
	}
}

// The two sentinels are the whole error mapping. Everything else is a 500, and
// a bad request must not become one.
func TestAPIMapsTheStoreSentinelsOntoStatusCodes(t *testing.T) {
	s, srv := testAPI(t)
	th := mustThread(t, s, "retries", amiran)

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
		want   int
	}{
		{"missing thread", http.MethodGet, "/api/chat/threads/t_0000000000000000", nil, 404},
		{"empty body", http.MethodPost, "/api/chat/threads/" + th.ID + "/messages",
			map[string]any{"text": "  "}, 400},
		{"unknown field", http.MethodPost, "/api/chat/threads/" + th.ID + "/messages",
			map[string]any{"nope": 1}, 400},
		{"unknown actor", http.MethodPost, "/api/chat/threads/" + th.ID + "/participants",
			map[string]any{"actor": "bot:ghost"}, 400},
		{"bad blob seq", http.MethodGet, "/api/chat/threads/" + th.ID + "/blobs/notanumber", nil, 400},
		{"missing blob", http.MethodGet, "/api/chat/threads/" + th.ID + "/blobs/999", nil, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := do(t, srv, tc.method, tc.path, tc.body)
			if res.StatusCode != tc.want {
				body, _ := io.ReadAll(res.Body)
				t.Fatalf("answered %d, want %d: %s", res.StatusCode, tc.want, body)
			}
		})
	}
}

// The one route that returns bytes an outsider chose gets its own tighter
// policy, and anything that is not an image the browser can safely render is a
// download rather than a page.
func TestAPIServesAttachmentsUnderATighterPolicy(t *testing.T) {
	s, srv := testAPI(t)
	th := mustThread(t, s, "with a file", amiran)

	upload := func(name string, body []byte) Attachment {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			srv.URL+"/api/chat/threads/"+th.ID+"/attachments", &buf)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("upload answered %d: %s", res.StatusCode, b)
		}
		return decode[Attachment](t, res)
	}

	image := upload("shot.png", pngBytes)
	res := do(t, srv, http.MethodGet, "/api/chat/attachments/"+image.ID, nil)
	if got := res.Header.Get("Content-Type"); got != "image/png" {
		t.Fatalf("image Content-Type = %q, want image/png", got)
	}
	if got := res.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "inline") {
		t.Fatalf("image Content-Disposition = %q, want inline", got)
	}
	if got := res.Header.Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
		t.Fatalf("attachment CSP = %q, want it sandboxed", got)
	}
	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("attachment nosniff = %q", got)
	}

	// An SVG is a document that can carry script, so it downloads rather than
	// rendering, whatever it was called.
	svg := upload("harmless.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`))
	res = do(t, srv, http.MethodGet, "/api/chat/attachments/"+svg.ID, nil)
	if got := res.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("svg Content-Type = %q, want application/octet-stream", got)
	}
	if got := res.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Fatalf("svg Content-Disposition = %q, want attachment", got)
	}
}

// ---- the socket ----

type wsClient struct {
	conn io.ReadWriteCloser
	t    *testing.T
}

func dialSocket(t *testing.T, srv *httptest.Server, path string) *wsClient {
	t.Helper()
	conn, _, _, err := ws.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http")+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &wsClient{conn: conn, t: t}
}

func (c *wsClient) send(v any) {
	c.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := wsutil.WriteClientText(c.conn, b); err != nil {
		c.t.Fatal(err)
	}
}

// next reads until a frame satisfies want, or the test times out. Frames the
// caller is not looking for are skipped rather than failing: a write produces
// several, and a test asserting on one of them should not have to enumerate the
// rest.
func (c *wsClient) next(want func(Frame) bool) Frame {
	c.t.Helper()
	deadline := time.Now().Add(socketWait)
	for time.Now().Before(deadline) {
		if d, ok := c.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
			_ = d.SetReadDeadline(deadline)
		}
		data, err := wsutil.ReadServerText(c.conn)
		if err != nil {
			c.t.Fatalf("reading a frame: %v", err)
		}
		var f Frame
		if err := json.Unmarshal(data, &f); err != nil {
			c.t.Fatalf("decoding a frame: %v (%s)", err, data)
		}
		if want(f) {
			return f
		}
	}
	c.t.Fatal("timed out waiting for the frame the test wanted")
	return Frame{}
}

// nextError reads until a rejection arrives. It has to skip frames: attaching
// replays the backlog, so a client that sends a bad op the moment it connects
// hears about the conversation before it hears about its own mistake.
func (c *wsClient) nextError() wsError {
	c.t.Helper()
	deadline := time.Now().Add(socketWait)
	for time.Now().Before(deadline) {
		if d, ok := c.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
			_ = d.SetReadDeadline(deadline)
		}
		data, err := wsutil.ReadServerText(c.conn)
		if err != nil {
			c.t.Fatalf("reading a frame: %v", err)
		}
		var e wsError
		if err := json.Unmarshal(data, &e); err == nil && e.Kind != "" {
			return e
		}
	}
	c.t.Fatal("timed out waiting for a client_error")
	return wsError{}
}

func isMessage(body string) func(Frame) bool {
	return func(f Frame) bool {
		return f.Stream == StreamMessage && f.Message != nil && f.Message.Body == body
	}
}

func TestSocketDeliversWhatHappensAfterAttaching(t *testing.T) {
	s, srv := testAPI(t)
	th := mustThread(t, s, "retries", amiran)

	c := dialSocket(t, srv, "/api/chat/socket")
	mustSay(t, s, th.ID, amiran, "reproduced")

	f := c.next(isMessage("reproduced"))
	if f.ThreadID != th.ID {
		t.Fatalf("frame is for %q, want %q", f.ThreadID, th.ID)
	}
	// The thread row is redrawn without the client having to refetch the list.
	c.next(func(f Frame) bool {
		return f.Stream == StreamThread && f.Thread != nil && f.Thread.LastText == "reproduced"
	})
}

// Resuming with ?since= replays what was missed, which is what makes an inbox
// correct again after a phone slept.
func TestSocketReplaysFromSince(t *testing.T) {
	s, srv := testAPI(t)
	th := mustThread(t, s, "retries", amiran)

	mark, err := s.Seq(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	mustSay(t, s, th.ID, amiran, "said while you were away")

	c := dialSocket(t, srv, "/api/chat/socket?since="+itoa(mark))
	c.next(isMessage("said while you were away"))
}

// A fleet mid-turn produces hundreds of events a minute across every thread.
// Shipping all of them to a client showing a list is how the fan-out budget
// goes, so a client only gets the timeline of the thread it says it is watching.
func TestSocketSendsTimelineEventsOnlyForTheWatchedThread(t *testing.T) {
	s, srv := testAPI(t)
	ctx := t.Context()
	watched := mustThread(t, s, "watched", amiran)
	other := mustThread(t, s, "other", demetre)

	c := dialSocket(t, srv, "/api/chat/socket")
	c.send(clientOp{Op: "watch", Thread: watched.ID})
	// The watch has to land before the events; a message round trip proves it.
	mustSay(t, s, watched.ID, amiran, "watching now")
	c.next(isMessage("watching now"))

	quiet, err := s.BeginTurn(ctx, other.ID, demetre)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, EventRecord{
		Thread: other.ID, Actor: demetre, TurnSeq: quiet, Kind: "tool_start",
		Payload: []byte(`{"kind":"tool_start","name":"grep"}`),
	}); err != nil {
		t.Fatal(err)
	}
	loud, err := s.BeginTurn(ctx, watched.ID, amiran)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, EventRecord{
		Thread: watched.ID, Actor: amiran, TurnSeq: loud, Kind: "tool_start",
		Payload: []byte(`{"kind":"tool_start","name":"bash"}`),
	}); err != nil {
		t.Fatal(err)
	}

	f := c.next(func(f Frame) bool { return f.Stream == StreamEvent })
	if f.ThreadID != watched.ID {
		t.Fatalf("got a timeline event for %q while watching %q", f.ThreadID, watched.ID)
	}
	if !strings.Contains(string(f.Event), "bash") {
		t.Fatalf("event payload = %s, want the watched thread's", f.Event)
	}
}

func TestSocketOpsWriteThroughTheStore(t *testing.T) {
	s, srv := testAPI(t)
	th := mustThread(t, s, "retries", amiran)

	c := dialSocket(t, srv, "/api/chat/socket")
	c.send(clientOp{Op: "send", Thread: th.ID, Text: "please look"})
	c.next(isMessage("please look"))

	msgs, _, _, err := s.Messages(t.Context(), Operator, th.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Author != Operator {
		t.Fatalf("the op did not reach the store: %+v", msgs)
	}

	c.send(clientOp{Op: "title", Thread: th.ID, Title: "renamed over the socket"})
	c.next(func(f Frame) bool {
		return f.Stream == StreamThread && f.Thread != nil && f.Thread.Title == "renamed over the socket"
	})
}

// A client's bad request did not happen in the conversation, so it must not
// arrive as one - and it must not take the socket down either.
func TestSocketAnswersABadOpWithoutClosing(t *testing.T) {
	s, srv := testAPI(t)
	th := mustThread(t, s, "retries", amiran)

	c := dialSocket(t, srv, "/api/chat/socket")
	c.send(clientOp{Op: "nonsense", Thread: th.ID})

	e := c.nextError()
	if e.Kind != kindClientError || !strings.Contains(e.Error, "nonsense") {
		t.Fatalf("bad op answered %+v, want a client_error naming the op", e)
	}

	// Still usable.
	c.send(clientOp{Op: "send", Thread: th.ID, Text: "still here"})
	c.next(isMessage("still here"))
}

// The terminal socket speaks bare events, the same bytes the session daemon
// emits, so an existing renderer can be pointed at a bot thread.
func TestThreadSocketEmitsBareEvents(t *testing.T) {
	s, srv := testAPI(t)
	ctx := t.Context()
	th := mustThread(t, s, "retries", amiran)
	turn, err := s.BeginTurn(ctx, th.ID, amiran)
	if err != nil {
		t.Fatal(err)
	}

	conn, _, _, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+
		"/api/chat/threads/"+th.ID+"/socket")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := s.AppendEvent(ctx, EventRecord{
		Thread: th.ID, Actor: amiran, TurnSeq: turn, Kind: "assistant_message",
		Payload: []byte(`{"kind":"assistant_message","text":"reproduced"}`),
	}); err != nil {
		t.Fatal(err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(socketWait))
	data, err := wsutil.ReadServerText(conn)
	if err != nil {
		t.Fatal(err)
	}
	var ev struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("the terminal socket sent something that is not an event: %s", data)
	}
	if ev.Kind != "assistant_message" || ev.Text != "reproduced" {
		t.Fatalf("event = %+v, want the one appended", ev)
	}
}

// ---- the hub ----

// Entitlement travels on the frame, so a subscriber never sees a thread it is
// not in - which is the same boundary the store enforces, applied to the live
// stream.
func TestHubDeliversOnlyToTheAudienceOnTheFrame(t *testing.T) {
	s := newStore(t)
	hub := NewHub()
	_ = s.AddPublisher("hub", hub.Publish)
	ctx := t.Context()

	th := mustThread(t, s, "private", amiran)
	participant := attachClient(t, hub, amiran)
	defer participant.Detach()
	mine := participant.Frames()
	outsider := attachClient(t, hub, jane)
	defer outsider.Detach()
	theirs := outsider.Frames()

	mustSay(t, s, th.ID, amiran, "between us")

	select {
	case f := <-mine:
		if f.Stream != StreamMessage && f.Stream != StreamThread {
			t.Fatalf("participant got %q", f.Stream)
		}
	case <-time.After(socketWait):
		t.Fatal("a participant was not delivered their own thread's message")
	}
	select {
	case f := <-theirs:
		t.Fatalf("an outsider was delivered %+v", f)
	case <-time.After(100 * time.Millisecond):
	}

	// And once they are added, they start hearing about it.
	if err := s.AddParticipant(ctx, amiran, th.ID, jane); err != nil {
		t.Fatal(err)
	}
	select {
	case <-theirs:
	case <-time.After(socketWait):
		t.Fatal("a new participant hears nothing")
	}
}

// A subscriber that stops reading must not stall the writer every bot is queued
// behind. It is dropped with a resume point instead.
func TestHubDropsASubscriberThatStopsReading(t *testing.T) {
	s := newStore(t)
	hub := NewHub()
	_ = s.AddPublisher("hub", hub.Publish)
	th := mustThread(t, s, "busy", amiran)

	client := attachClient(t, hub, Operator)
	defer client.Detach()
	slow := client.Frames()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range queueCap + 20 {
			mustSay(t, s, th.ID, amiran, "chatter")
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("writing blocked on a subscriber that stopped reading")
	}

	var last uint64
	for f := range slow {
		if f.Stream == StreamDesync {
			if f.From != last {
				t.Fatalf("desync resumes at %d, want %d - the last frame actually delivered",
					f.From, last)
			}
			return
		}
		last = f.Seq
	}
	t.Fatal("the slow subscriber was never dropped")
}

func TestHubDetachIsIdempotent(t *testing.T) {
	hub := NewHub()
	client := attachClient(t, hub, Operator)
	out := client.Frames()
	client.Detach()
	client.Detach()
	if _, ok := <-out; ok {
		t.Fatal("the channel must close when the subscriber detaches")
	}
	if n := hub.Attached(); n != 0 {
		t.Fatalf("%d subscribers left after detaching", n)
	}
}

// attachClient adds a hub client that follows from now, with no backlog.
func attachClient(t *testing.T, hub *Hub, actor string) *Client {
	t.Helper()
	c, err := hub.Attach(actor, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func itoa(n uint64) string { return strconv.FormatUint(n, 10) }
