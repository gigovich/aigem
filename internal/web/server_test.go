package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
)

func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	go func() { _ = srv.Serve() }()
	return srv
}

func TestHealthzReportsWhetherTheBuildCarriesAUI(t *testing.T) {
	srv := newTestServer(t, Config{})
	res, err := http.Get(srv.URL() + "healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var got struct {
		OK bool `json:"ok"`
		UI bool `json:"ui"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK {
		t.Error("ok = false")
	}
	if got.UI != HasAssets() {
		t.Errorf("ui = %v, want %v", got.UI, HasAssets())
	}
	if got := res.Header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") {
		t.Errorf("CSP = %q, want it to start from default-src 'self'", got)
	}
}

// Binding an address the network can reach means answering requests whose
// origin nothing in this process can verify. Until the origin check exists,
// refusing is the only honest answer - and the message has to say what to do
// instead, or the refusal reads as a bug.
func TestNonLoopbackBindIsRefused(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "192.0.2.10:8080", "[2001:db8::1]:8080"} {
		_, err := New(Config{Addr: addr})
		if err == nil {
			t.Errorf("New(%q) succeeded, want a refusal", addr)
			continue
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("New(%q) error = %v, want it to explain the loopback rule", addr, err)
		}
	}
}

func TestLoopbackBindsAreAccepted(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
		srv, err := New(Config{Addr: addr})
		if err != nil {
			t.Errorf("New(%q) = %v, want it to bind", addr, err)
			continue
		}
		_ = srv.Close()
	}
}

// The asset handler is the mux's catch-all, so it has to lose to a real route.
func TestARealRouteWinsOverTheAssetCatchAll(t *testing.T) {
	srv := newTestServer(t, Config{Assets: spaHandler(fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html>the page")},
	})})

	res, err := http.Get(srv.URL() + "healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	if strings.Contains(string(body), "the page") {
		t.Fatalf("GET /healthz served the SPA:\n%s", body)
	}

	page, err := http.Get(srv.URL() + "models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = page.Body.Close() }()
	pageBody, _ := io.ReadAll(page.Body)
	if !strings.Contains(string(pageBody), "the page") {
		t.Fatalf("GET /models did not serve the SPA:\n%s", pageBody)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	srv, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
