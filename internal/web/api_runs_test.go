package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// errNope is a failure the backend did not classify: the daemon's own fault,
// not the client's.
var errNope = errors.New("the model provider fell over")

// api performs an authenticated request, the way a signed-in page does.
func api(t *testing.T, srv *Server, method, path string, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.Base()+strings.TrimPrefix(path, "/"), rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decode[T any](t *testing.T, res *http.Response) T {
	t.Helper()
	var v T
	body := readBody(t, res)
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return v
}

// A collection is an array or it is nothing, and "nothing" costs every caller a
// null check the wire could have spared. A Go nil slice encodes as null, so
// this is one line away from being wrong at all times.
func TestAnEmptyRunListIsAnArrayAndNotNull(t *testing.T) {
	srv := newTestServer(t, Config{})
	res := api(t, srv, http.MethodGet, "/api/runs", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if got := strings.TrimSpace(readBody(t, res)); got != "[]" {
		t.Errorf("body = %s, want []", got)
	}
}

func TestTheRunListIsServedOldestFirst(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	for range 3 {
		if res := api(t, srv, http.MethodPost, "/api/runs", `{}`); res.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", res.StatusCode)
		}
	}
	runs := decode[[]Run](t, api(t, srv, http.MethodGet, "/api/runs", ""))
	var ids []string
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	if want := []string{"RUN-1", "RUN-2", "RUN-3"}; strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v", ids, want)
	}
}

// Opening a run is a mutation, and the other tabs learn about it from the
// control stream rather than by polling. The revision has to move with it, or a
// page that is looking at the list has no way to know it is now stale.
func TestOpeningARunAnswersTheRecordAndPublishesIt(t *testing.T) {
	srv := newTestServer(t, Config{})
	c := dialControl(t, srv)
	hello := c.next()

	res := api(t, srv, http.MethodPost, "/api/runs", `{"title":"a first look"}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", res.StatusCode)
	}
	run := decode[Run](t, res)
	if run.ID == "" || run.Title != "a first look" || !run.Live {
		t.Fatalf("run = %+v, want a live run carrying the title it was given", run)
	}

	f := c.next()
	if f.Type != "run.updated" {
		t.Fatalf("control message = %q, want run.updated", f.Type)
	}
	if f.Rev != hello.Rev+1 {
		t.Errorf("rev = %d, want %d", f.Rev, hello.Rev+1)
	}
	var got Run
	if err := json.Unmarshal(f.Data, &got); err != nil {
		t.Fatalf("decode %s: %v", f.Data, err)
	}
	if got.ID != run.ID {
		t.Errorf("published run = %q, want the one that was opened, %q", got.ID, run.ID)
	}
}

// A mode this phase does not serve is the client's mistake, and the reason for
// it is worth reading: the alternative is a button that does nothing.
func TestAModeThisPhaseDoesNotServeIsRefusedWithItsReason(t *testing.T) {
	srv := newTestServer(t, Config{})
	res := api(t, srv, http.MethodPost, "/api/runs", `{"mode":"autonomous"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if body := readBody(t, res); !strings.Contains(body, "autonomous") {
		t.Errorf("body = %q, want the mode that was refused", body)
	}
}

// Everything the backend cannot classify is the daemon's own fault, and the
// detail stays in the log: an error assembled inside the agent is not a
// sentence anyone at a browser can act on, and it is where a path or a
// provider's message would otherwise leak into a page.
func TestAFailureTheBackendDidNotClassifyIsAnsweredWithoutItsDetail(t *testing.T) {
	b := &fakeBackend{openErr: errNope}
	srv := newTestServer(t, Config{Backend: b})
	res := api(t, srv, http.MethodPost, "/api/runs", `{}`)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
	if body := readBody(t, res); strings.Contains(body, errNope.Error()) {
		t.Errorf("body = %q, it repeats the internal error", body)
	}
}

func TestAnUnknownRunIs404OnEveryRouteThatNamesOne(t *testing.T) {
	srv := newTestServer(t, Config{})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/runs/RUN-9"},
		{http.MethodDelete, "/api/runs/RUN-9"},
		{http.MethodGet, "/api/runs/RUN-9/events"},
		{http.MethodGet, "/api/runs/RUN-9/artifacts"},
	} {
		res := api(t, srv, tc.method, tc.path, "")
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", tc.method, tc.path, res.StatusCode)
		}
	}
}

// Closing a run leaves the record and the timeline behind: a run a person is
// done with is one they can still read. What comes back is the record as it now
// stands, so a client applies what is true rather than what it assumed.
func TestClosingARunReportsTheRecordItLeftBehind(t *testing.T) {
	srv := newTestServer(t, Config{})
	c := dialControl(t, srv)
	hello := c.next()
	opened := decode[Run](t, api(t, srv, http.MethodPost, "/api/runs", `{}`))
	_ = c.next() // run.updated for the open

	res := api(t, srv, http.MethodDelete, "/api/runs/"+opened.ID, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	closed := decode[Run](t, res)
	if closed.Live || closed.Status != "closed" {
		t.Errorf("closed run = %+v, want a record that is no longer live", closed)
	}
	if f := c.next(); f.Type != "run.updated" || f.Rev != hello.Rev+2 {
		t.Errorf("control message = %q at rev %d, want run.updated at %d",
			f.Type, f.Rev, hello.Rev+2)
	}
	// Still listed: the record outlives the session on purpose.
	if runs := decode[[]Run](t, api(t, srv, http.MethodGet, "/api/runs", "")); len(runs) != 1 {
		t.Errorf("the list holds %d runs after a close, want 1", len(runs))
	}
}

// Two tabs pressing the same button is the ordinary case, not a failure.
func TestClosingARunTwiceIsNotAnError(t *testing.T) {
	srv := newTestServer(t, Config{})
	opened := decode[Run](t, api(t, srv, http.MethodPost, "/api/runs", `{}`))
	for i := range 2 {
		res := api(t, srv, http.MethodDelete, "/api/runs/"+opened.ID, "")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("close %d = %d, want 200", i+1, res.StatusCode)
		}
	}
}

// The timeline is read in pages, and the page is what the client asked for
// after the cursor it gave. The events go out as the session encoded them.
func TestTheTimelineIsPagedFromACursor(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	run := decode[Run](t, api(t, srv, http.MethodPost, "/api/runs", `{}`))
	for _, k := range []string{"user_message", "turn_start", "content", "turn_end"} {
		b.emit(run.ID, `"kind":"`+k+`"`)
	}

	all := decode[[]json.RawMessage](t, api(t, srv, http.MethodGet,
		"/api/runs/"+run.ID+"/events", ""))
	if len(all) != 4 {
		t.Fatalf("got %d events, want 4", len(all))
	}
	if !bytes.Contains(all[0], []byte(`"kind":"user_message"`)) {
		t.Errorf("first event = %s, want the session's own bytes", all[0])
	}

	rest := decode[[]json.RawMessage](t, api(t, srv, http.MethodGet,
		"/api/runs/"+run.ID+"/events?since=2&limit=1", ""))
	if len(rest) != 1 || !bytes.Contains(rest[0], []byte(`"seq":3`)) {
		t.Fatalf("page after seq 2 = %s, want one event at seq 3", rest)
	}
}

func TestAnEmptyTimelineIsAnArrayAndNotNull(t *testing.T) {
	srv := newTestServer(t, Config{})
	run := decode[Run](t, api(t, srv, http.MethodPost, "/api/runs", `{}`))
	res := api(t, srv, http.MethodGet, "/api/runs/"+run.ID+"/events", "")
	if got := strings.TrimSpace(readBody(t, res)); got != "[]" {
		t.Errorf("body = %s, want []", got)
	}
}

// Every cursor on this API is a non-negative whole number. One parser is one
// rule, and a client that sends anything else finds out at the request rather
// than by getting a page it did not ask for.
func TestACursorThatIsNotANonNegativeNumberIsRefused(t *testing.T) {
	srv := newTestServer(t, Config{})
	run := decode[Run](t, api(t, srv, http.MethodPost, "/api/runs", `{}`))
	for _, q := range []string{"?since=-1", "?since=later", "?since=1.5", "?limit=-1", "?limit=all"} {
		res := api(t, srv, http.MethodGet, "/api/runs/"+run.ID+"/events"+q, "")
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", q, res.StatusCode)
		}
	}
}

// A gap the journal can no longer fill is not something a client retries: it
// reloads, and 410 is how it is told to.
func TestAHistoryThatNoLongerReachesThePointIsGone(t *testing.T) {
	b := &fakeBackend{eventsErr: ErrHistoryGone}
	srv := newTestServer(t, Config{Backend: b})
	b.seed(Run{ID: "RUN-1", Status: "open", Live: true})
	res := api(t, srv, http.MethodGet, "/api/runs/RUN-1/events?since=90", "")
	if res.StatusCode != http.StatusGone {
		t.Fatalf("status = %d, want 410", res.StatusCode)
	}
}

func TestArtifactsAreServedForARunAndAreAnArrayWhenThereAreNone(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	b.seed(Run{ID: "RUN-1", Status: "open", Live: true})
	res := api(t, srv, http.MethodGet, "/api/runs/RUN-1/artifacts", "")
	if got := strings.TrimSpace(readBody(t, res)); got != "[]" {
		t.Errorf("body = %s, want []", got)
	}

	b.mu.Lock()
	b.runs["RUN-1"].arts = []Artifact{{Path: "/w/main.go", Old: "a", New: "b"}}
	b.mu.Unlock()
	arts := decode[[]Artifact](t, api(t, srv, http.MethodGet, "/api/runs/RUN-1/artifacts", ""))
	if len(arts) != 1 || arts[0].Path != "/w/main.go" || arts[0].New != "b" {
		t.Errorf("artifacts = %+v, want the one change with both sides", arts)
	}
}

// A run whose session is gone still answers reads. It is the difference between
// a closed conversation a person can look back at and one that has
// disappeared. What it refuses is the socket, which runsocket_test.go pins.
func TestAClosedRunStillServesItsTimeline(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	b.seed(Run{ID: "RUN-1", Status: "closed"},
		RunEvent{Seq: 1, Data: json.RawMessage(`{"seq":1,"kind":"turn_end"}`)})

	events := decode[[]json.RawMessage](t, api(t, srv, http.MethodGet,
		"/api/runs/RUN-1/events", ""))
	if len(events) != 1 {
		t.Fatalf("got %d events from a closed run, want 1", len(events))
	}
}

func TestTheRunRoutesRefuseOtherMethods(t *testing.T) {
	srv := newTestServer(t, Config{})
	for _, tc := range []struct{ method, path, allow string }{
		{http.MethodPut, "/api/runs", "GET, HEAD, POST"},
		{http.MethodPost, "/api/runs/RUN-1", "GET, HEAD, DELETE"},
		{http.MethodPost, "/api/runs/RUN-1/events", "GET, HEAD"},
		{http.MethodPost, "/api/runs/RUN-1/artifacts", "GET, HEAD"},
		{http.MethodPost, "/api/runs/RUN-1/socket", "GET, HEAD"},
	} {
		res := api(t, srv, tc.method, tc.path, "")
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.path, res.StatusCode)
		}
		if got := res.Header.Get("Allow"); got != tc.allow {
			t.Errorf("%s Allow = %q, want %q", tc.path, got, tc.allow)
		}
	}
}

// A body with no bound is a signed-in page's mistake turned into the daemon's
// memory.
func TestACreateBodyPastTheBoundIsRefused(t *testing.T) {
	srv := newTestServer(t, Config{})
	body := `{"title":"` + strings.Repeat("x", maxRunBody) + `"}`
	res := api(t, srv, http.MethodPost, "/api/runs", body)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

// A second document in the same body is a field the server would otherwise
// ignore in silence.
func TestACreateBodyWithMoreThanOneDocumentIsRefused(t *testing.T) {
	srv := newTestServer(t, Config{})
	res := api(t, srv, http.MethodPost, "/api/runs", `{} {"title":"and another"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

// The whole API is closed by default. This is the run half of that promise.
//
// Every method as well as every path: the mux matches method-qualified
// patterns, so a route registered on the server's bare mux rather than through
// s.api is only reachable - and only detectable - by the method it was
// registered under. Probing GET alone would have left "open a run" and "close a
// run" answerable by anyone who could reach the port.
func TestTheRunRoutesNeedACredential(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/runs"},
		{http.MethodPost, "/api/runs"},
		{http.MethodGet, "/api/runs/RUN-1"},
		{http.MethodDelete, "/api/runs/RUN-1"},
		{http.MethodGet, "/api/runs/RUN-1/events"},
		{http.MethodGet, "/api/runs/RUN-1/artifacts"},
		{http.MethodGet, "/api/runs/RUN-1/socket"},
	} {
		req, err := http.NewRequest(tc.method, srv.Base()+strings.TrimPrefix(tc.path, "/"),
			strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s = %d with no credential, want 401",
				tc.method, tc.path, res.StatusCode)
		}
	}
	// And nothing happened: a mutation that answered 401 after doing the work
	// would pass every status assertion above.
	if runs, _ := b.Runs(context.Background()); len(runs) != 0 {
		t.Errorf("an unauthenticated request opened %d runs", len(runs))
	}
}

// The 201 is the one answer on this API written with an explicit status, which
// is the one place a Content-Type can be set after net/http has already
// snapshotted the header block and be silently dropped.
func TestOpeningARunAnswersJSON(t *testing.T) {
	srv := newTestServer(t, Config{})
	res := api(t, srv, http.MethodPost, "/api/runs", `{}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// The page cap is what stops one request from serialising an entire journal.
// Absent, zero and past it all mean the same thing: as much as the daemon will
// give, which is one page.
func TestAPageIsCappedWhateverTheClientAsksFor(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	run := decode[Run](t, api(t, srv, http.MethodPost, "/api/runs", `{}`))
	for range maxEventPage + 5 {
		b.emit(run.ID, `"kind":"content"`)
	}
	for _, q := range []string{"", "?limit=0", "?limit=999999"} {
		page := decode[[]json.RawMessage](t, api(t, srv, http.MethodGet,
			"/api/runs/"+run.ID+"/events"+q, ""))
		if len(page) != maxEventPage {
			t.Errorf("limit %q gave %d events, want the cap of %d", q, len(page), maxEventPage)
		}
	}
	// And a smaller one is honoured, so the cap is a ceiling and not the answer.
	if page := decode[[]json.RawMessage](t, api(t, srv, http.MethodGet,
		"/api/runs/"+run.ID+"/events?limit=3", "")); len(page) != 3 {
		t.Errorf("limit=3 gave %d events, want 3", len(page))
	}
}

// A page reads the feature map rather than the version string, so a route that
// exists and is not named there is a screen the UI hides for no reason.
func TestTheFeatureMapNamesTheRunsApi(t *testing.T) {
	srv := newTestServer(t, Config{})
	meta := decode[metaResponse](t, api(t, srv, http.MethodGet, "/api/meta", ""))
	if !meta.Features["runs"] {
		t.Errorf("features = %v, want runs among them", meta.Features)
	}
}
