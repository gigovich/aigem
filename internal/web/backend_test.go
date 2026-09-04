package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBackend stands in for the agent. Every HTTP and websocket test in this
// package runs against one, which is the whole point of the seam: no model, no
// MCP server and no session behind the router.
type fakeBackend struct {
	meta Meta
	// err is what Meta answers with. A backend that cannot describe the daemon
	// is a state the router has to have an answer for, on the socket as well as
	// on the route.
	err error

	mu sync.Mutex
	// runs is the table, and order keeps the order they were opened in, which
	// is the order a list is served in.
	runs  map[string]*fakeRun
	order []string
	next  int
	// applied records every operation that reached the backend, so a test can
	// tell "the router refused it" from "the router passed it on".
	applied []RunOp
	// The errors a test arms. Each stands for a state the router has to have an
	// answer for and cannot otherwise be driven into.
	openErr, watchErr, eventsErr, opErr, listErr error
}

// fakeRun is one conversation in the fake: its record, everything that has
// happened in it, and whoever is currently watching.
type fakeRun struct {
	run    Run
	events []RunEvent
	arts   []Artifact
	subs   map[*fakeStream]struct{}
}

func (b *fakeBackend) Meta(context.Context) (Meta, error) {
	if b.err != nil {
		return Meta{}, b.err
	}
	return b.meta, nil
}

func (b *fakeBackend) Runs(context.Context) ([]Run, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.listErr != nil {
		return nil, b.listErr
	}
	if len(b.order) == 0 {
		// A nil slice, deliberately: it is what a backend built on a Go map or
		// a filtered loop hands back, it encodes as null, and the router's job
		// is to turn it into an array. A fake that helpfully returned an empty
		// slice would make the test for that pass on its own.
		return nil, nil
	}
	out := make([]Run, 0, len(b.order))
	for _, id := range b.order {
		out = append(out, b.runs[id].run)
	}
	return out, nil
}

func (b *fakeBackend) OpenRun(_ context.Context, req NewRun) (Run, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openErr != nil {
		return Run{}, b.openErr
	}
	if req.Mode != "" && req.Mode != "interactive" {
		return Run{}, Refuse(errors.New("unsupported run mode " + req.Mode))
	}
	b.next++
	id := "RUN-" + strconv.Itoa(b.next)
	run := Run{
		ID: id, Mode: "interactive", Title: req.Title, Model: req.Model,
		Status: "open", Live: true,
		Created: time.Unix(int64(b.next), 0).UTC(),
		Updated: time.Unix(int64(b.next), 0).UTC(),
	}
	b.addLocked(&fakeRun{run: run})
	return run, nil
}

// addLocked puts a run in the table. It is also how a test seeds one.
func (b *fakeBackend) addLocked(fr *fakeRun) {
	if fr.subs == nil {
		fr.subs = map[*fakeStream]struct{}{}
	}
	if b.runs == nil {
		b.runs = map[string]*fakeRun{}
	}
	b.runs[fr.run.ID] = fr
	b.order = append(b.order, fr.run.ID)
}

// seed adds a run a test did not open through the API.
func (b *fakeBackend) seed(run Run, events ...RunEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.addLocked(&fakeRun{run: run, events: events})
}

func (b *fakeBackend) Run(_ context.Context, id string) (Run, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fr := b.runs[id]
	if fr == nil {
		return Run{}, ErrNoRun
	}
	return fr.run, nil
}

func (b *fakeBackend) CloseRun(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	fr := b.runs[id]
	if fr == nil {
		return ErrNoRun
	}
	fr.run.Status, fr.run.Live, fr.run.Running = "closed", false, false
	for s := range fr.subs {
		s.finish()
	}
	fr.subs = map[*fakeStream]struct{}{}
	return nil
}

func (b *fakeBackend) RunEvents(_ context.Context, id string, since uint64, limit int) (
	[]RunEvent, error,
) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.eventsErr != nil {
		return nil, b.eventsErr
	}
	fr := b.runs[id]
	if fr == nil {
		return nil, ErrNoRun
	}
	var out []RunEvent
	for _, ev := range fr.events {
		if ev.Seq > since {
			out = append(out, ev)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (b *fakeBackend) WatchRun(_ context.Context, id string, c RunClient, since uint64) (
	RunStream, error,
) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.watchErr != nil {
		return nil, b.watchErr
	}
	fr := b.runs[id]
	if fr == nil {
		return nil, ErrNoRun
	}
	if !fr.run.Live {
		return nil, ErrRunClosed
	}
	// Buffered past what any test sends, so the backlog and the live events go
	// in without a reader having to be there yet - which is what the session
	// itself does with a subscriber's queue.
	s := &fakeStream{b: b, run: fr, client: c, ch: make(chan RunEvent, 64)}
	for _, ev := range fr.events {
		if ev.Seq > since {
			s.ch <- ev
		}
	}
	fr.subs[s] = struct{}{}
	return s, nil
}

func (b *fakeBackend) RunArtifacts(_ context.Context, id string) ([]Artifact, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fr := b.runs[id]
	if fr == nil {
		return nil, ErrNoRun
	}
	return fr.arts, nil
}

func (b *fakeBackend) ApplyRunOp(_ context.Context, id string, op RunOp) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	fr := b.runs[id]
	if fr == nil {
		return ErrNoRun
	}
	if b.opErr != nil {
		return b.opErr
	}
	b.applied = append(b.applied, op)
	return nil
}

// watchers reports who is currently attached to a run, which is also how a
// test sees whether a disconnecting client detached.
func (b *fakeBackend) watchers(id string) []RunClient {
	b.mu.Lock()
	defer b.mu.Unlock()
	fr := b.runs[id]
	if fr == nil {
		return nil
	}
	out := make([]RunClient, 0, len(fr.subs))
	for s := range fr.subs {
		out = append(out, s.client)
	}
	return out
}

// ops reports what reached the backend.
func (b *fakeBackend) ops() []RunOp {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]RunOp(nil), b.applied...)
}

// emit is the session happening: it records an event and hands it to whoever is
// watching. The sequence is assigned here, as the session assigns it.
func (b *fakeBackend) emit(id string, payload string) RunEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	fr := b.runs[id]
	ev := RunEvent{Seq: uint64(len(fr.events)) + 1}
	ev.Data = json.RawMessage(`{"seq":` + strconv.FormatUint(ev.Seq, 10) + `,` + payload + `}`)
	fr.events = append(fr.events, ev)
	for s := range fr.subs {
		s.ch <- ev
	}
	return ev
}

// fakeStream is one client's attachment. Close is idempotent, as the interface
// promises, so a handler may defer it and still close early.
type fakeStream struct {
	b   *fakeBackend
	run *fakeRun
	// client is who attached. Presence in the UI is built out of it, so a
	// router that dropped it on the way to the backend has to fail a test
	// rather than quietly show every tab as an unnamed one.
	client RunClient
	ch     chan RunEvent
	once   sync.Once
}

func (s *fakeStream) Events() <-chan RunEvent { return s.ch }

func (s *fakeStream) Close() {
	s.b.mu.Lock()
	delete(s.run.subs, s)
	s.b.mu.Unlock()
	s.finish()
}

// finish closes the channel once, whether the client detached or the run ended
// under it. It is called with the backend's lock held by one caller and without
// it by the other, so it takes none.
func (s *fakeStream) finish() { s.once.Do(func() { close(s.ch) }) }

// withBackend fills in a fake for the tests that are about something else. A
// test that cares which backend it gets says so.
func withBackend(cfg Config) Config {
	if cfg.Backend == nil {
		cfg.Backend = &fakeBackend{}
	}
	return cfg
}

// A daemon with no backend would serve the page and then fail one API request
// at a time, which reports the wiring mistake nowhere near where it was made.
func TestNewRefusesADaemonWithNoBackend(t *testing.T) {
	srv, err := New(Config{})
	if err == nil {
		_ = srv.Close()
		t.Fatal("New with no backend succeeded, want a refusal")
	}
	if !errors.Is(err, ErrNoBackend) {
		t.Fatalf("New error = %v, want ErrNoBackend", err)
	}
	if srv != nil {
		t.Error("New returned a server along with the error")
	}
}

// The refusal happens before the listener, and this is what says so. A leaked
// listener would make the operator's second attempt fail with a message about
// the address rather than about the wiring they just fixed.
func TestTheRefusalWithNoBackendBindsNothing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if _, err := New(Config{Addr: addr}); !errors.Is(err, ErrNoBackend) {
		t.Fatalf("New error = %v, want ErrNoBackend", err)
	}
	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the port is held after a refusal that should never have bound it: %v", err)
	}
	_ = again.Close()
}

func TestTheServerKeepsTheBackendItWasGiven(t *testing.T) {
	want := Meta{Version: "1.2.3", DefaultModel: "openai/gpt-5.6-sol"}
	srv := newTestServer(t, Config{Backend: &fakeBackend{meta: want}})
	got, err := srv.backend.Meta(context.Background())
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if got != want {
		t.Errorf("Meta = %+v, want %+v", got, want)
	}
}

// The mux is a field so that the route files added as the API grows register on
// the server they belong to. That only works if the handler the http.Server was
// built with is the same mux - and a route registered on it has to come out
// carrying the security headers, since they wrap the mux once rather than being
// applied per handler.
func TestRoutesRegisteredOnTheServersMuxAreServedAndCarryTheHeaders(t *testing.T) {
	srv, err := New(withBackend(Config{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.mux.HandleFunc("GET /api/late", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	req := httptest.NewRequest(http.MethodGet, "http://"+srv.Addr().String()+"/api/late", nil)
	rec := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d: the http.Server is not serving the server's own mux",
			rec.Code, http.StatusTeapot)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("a route registered on the mux answered without the security headers")
	}
}

// The API is closed by construction, not by each route remembering to wrap
// itself: s.api is the only way a handler under /api/ is registered, so this is
// the test that fails if it stops applying the guard.
func TestARouteRegisteredThroughApiNeedsACredential(t *testing.T) {
	srv := newTestServer(t, Config{})
	var reached atomic.Bool
	srv.api("GET /api/late", func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})

	res, err := http.Get(srv.Base() + "api/late")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.StatusCode)
	}
	if reached.Load() {
		t.Error("the handler ran for a request carrying no credential")
	}

	req, err := http.NewRequest(http.MethodGet, srv.Base()+"api/late", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token())
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Errorf("status with the token = %d, want 204", res.StatusCode)
	}
}

// routes() promises that an unknown path under /api/ is a 404 and not the page.
// assets_test.go proves the asset handler refuses such a path; this is the
// assembled daemon answering, which is what a JSON client actually meets - and
// the failure it prevents is "unexpected token <" from a decoder handed HTML.
func TestAnUnknownApiPathIs404AndNotThePage(t *testing.T) {
	srv := newTestServer(t, Config{Assets: spaHandler(testDist())})
	req, err := http.NewRequest(http.MethodGet, srv.Base()+"api/nothing-here", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
	if strings.Contains(string(body), "the page") {
		t.Errorf("an unknown /api/ path served the SPA:\n%s", body)
	}
}

// The API is closed by default, so the two routes that are not have to be
// stated rather than left to be noticed. /healthz is a liveness probe, and the
// caller that needs it most is the one that has not signed in yet.
func TestHealthzAnswersWithNoCredential(t *testing.T) {
	srv := newTestServer(t, Config{})
	res, err := http.Get(srv.Base() + "healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 with no credential", res.StatusCode)
	}
}
