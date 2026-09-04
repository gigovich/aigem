package web

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
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
}

func (b *fakeBackend) Meta(context.Context) (Meta, error) {
	if b.err != nil {
		return Meta{}, b.err
	}
	return b.meta, nil
}

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
