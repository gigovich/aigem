package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// mountServer is a daemon with no session factory at all, which is what the bot
// fleet runs: it serves someone else's API and creates no conversations.
func mountServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	srv, err := New(Config{
		// With assets, because the shipped binary has them and the asset
		// handler is the mux's catch-all: without one here, every assertion
		// about a route this daemon does not serve was being made against a
		// daemon that could not serve a page either.
		Assets: spaHandler(testDist()),
		Mount: func(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
			mux.HandleFunc("GET /api/chat/ping", guard(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("pong"))
			}))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func TestNewRefusesADaemonWithNothingToServe(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, err := New(Config{}); err == nil {
		t.Fatal("a daemon with neither a factory nor a mount must not start")
	}
}

func TestMountedRoutesAnswerUnderTheDaemonsRules(t *testing.T) {
	srv := mountServer(t)

	res := srv.get(t, "/api/chat/ping")
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("mounted route answered %d, want 200", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "pong" {
		t.Fatalf("body = %q, want %q", body, "pong")
	}
	// The CSP is what makes one listener safe to share: a mounted API must not
	// be able to answer without the directive that closes the exfiltration hole.
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' data:") {
		t.Fatalf("mounted route Content-Security-Policy = %q, want the daemon's img-src", csp)
	}
}

func TestMountedRoutesRefuseAnUnauthenticatedRequest(t *testing.T) {
	srv := mountServer(t)

	res, err := http.Get("http://" + srv.Addr().String() + "/api/chat/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated mounted route answered %d, want 401", res.StatusCode)
	}
}

func TestMountedRoutesRefuseAForeignOrigin(t *testing.T) {
	srv := mountServer(t)

	req, err := http.NewRequest(http.MethodGet, "http://"+srv.Addr().String()+"/api/chat/ping", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+srv.token)
	req.Header.Set("Origin", "http://evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin answered %d, want 403", res.StatusCode)
	}
}

// A mount-only daemon has no conversations. The session routes are therefore
// never registered, so the request falls through to the asset handler instead
// of reaching a nil factory - which is the failure this guards against. What
// the fall-through answers depends on whether the binary carries a UI, so the
// assertion is only that it is not the session API.
func TestMountOnlyDaemonHasNoSessionAPI(t *testing.T) {
	srv := mountServer(t)

	res := srv.get(t, "/api/sessions")
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusOK {
		t.Fatal("GET /api/sessions answered 200; a daemon with no factory must not serve the session API")
	}

	res2, err := http.Post("http://"+srv.Addr().String()+"/api/sessions", "application/json",
		strings.NewReader(`{"cwd":"/tmp"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res2.Body.Close() }()
	if res2.StatusCode == http.StatusOK {
		t.Fatal("POST /api/sessions answered 200 with no factory to build one")
	}
}

// The mode switch is drawn from this answer, so a daemon that reported the
// wrong half would offer the operator a screen every request of which 404s.
func TestModesReportWhatTheDaemonActuallyServes(t *testing.T) {
	for _, tc := range []struct {
		name string
		srv  func(*testing.T) *Server
		want Modes
	}{
		{"mount only", mountServer, Modes{Sessions: false, Chat: true}},
		{"factory only", testServer, Modes{Sessions: true, Chat: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.srv(t).get(t, "/api/modes")
			defer func() { _ = res.Body.Close() }()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("GET /api/modes answered %d, want 200", res.StatusCode)
			}
			var got Modes
			if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("modes = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestModesNeedsTheToken(t *testing.T) {
	srv := mountServer(t)

	res, err := http.Get("http://" + srv.Addr().String() + "/api/modes")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/modes answered %d, want 401", res.StatusCode)
	}
}

// Spend is a property of the provider account, not of a conversation, so it
// survives having no sessions.
func TestMountOnlyDaemonStillReportsUsage(t *testing.T) {
	srv := mountServer(t)

	res := srv.get(t, "/api/usage")
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/usage answered %d, want 200", res.StatusCode)
	}
}
