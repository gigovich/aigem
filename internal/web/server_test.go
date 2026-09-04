package web

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	srv, err := New(withBackend(cfg))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	go func() { _ = srv.Serve() }()
	return srv
}

// A liveness probe and nothing else. Everything it used to carry - the version,
// whether this build has a UI - moved to /api/meta behind Guard, and this test
// is what keeps it from creeping back: an unauthenticated caller learns that
// the process is up, and no more than that.
func TestHealthzSaysOnlyThatTheDaemonIsUp(t *testing.T) {
	for _, cfg := range []Config{{}, {Assets: spaHandler(testDist())}} {
		srv := newTestServer(t, cfg)
		res, err := http.Get(srv.Base() + "healthz")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if ok, _ := got["ok"].(bool); !ok {
			t.Errorf("ok = %v", got["ok"])
		}
		if len(got) != 1 {
			t.Errorf("/healthz answered %s; it says whether the process is up and nothing else", body)
		}
	}
}

// Binding an address the network can reach means answering requests whose
// origin nothing in this process can verify. Until the origin check exists,
// refusing is the only honest answer - and the message has to say what to do
// instead, or the refusal reads as a bug.
func TestNonLoopbackBindIsRefused(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "192.0.2.10:8080", "[2001:db8::1]:8080"} {
		_, err := New(withBackend(Config{Addr: addr}))
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
	// Mixed case included: hostnames are case-insensitive to the resolver, so
	// refusing "Localhost" would be a usability wart, not a control.
	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "Localhost:0", "[::1]:0"} {
		srv, err := New(withBackend(Config{Addr: addr}))
		if err != nil {
			t.Errorf("New(%q) = %v, want it to bind", addr, err)
			continue
		}
		_ = srv.Close()
	}
}

// The agent reads pages an attacker may have written and the UI renders model
// output, so the policy has to reach the document it bounds. It is applied as
// one wrapper around the mux precisely because the page and the bundle are
// served by http.FileServerFS, which calls nothing of ours - and because the
// build that carries no UI is not the one that needs protecting.
func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	check := func(t *testing.T, cfg Config, paths ...string) {
		t.Helper()
		srv := newTestServer(t, cfg)
		for _, path := range paths {
			res, err := http.Get(srv.Base() + path)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			_ = res.Body.Close()
			csp := res.Header.Get("Content-Security-Policy")
			for _, want := range []string{"default-src 'self'", "img-src 'self' data:", "form-action 'none'"} {
				if !strings.Contains(csp, want) {
					t.Errorf("/%s CSP = %q, missing %q", path, csp, want)
				}
			}
			if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("/%s X-Content-Type-Options = %q, want nosniff", path, got)
			}
			if got := res.Header.Get("Referrer-Policy"); got != "no-referrer" {
				t.Errorf("/%s Referrer-Policy = %q, want no-referrer", path, got)
			}
		}
	}

	check(t, Config{Assets: spaHandler(testDist())},
		"", "index.html", "models", "assets/main.js", "assets/", "healthz", "api/typo")
	// The 501 page too: it is the build most likely to be refactored, and the
	// policy is not optional there either.
	check(t, Config{}, "", "models")
}

// net/http answers "OPTIONS *" itself, without ever calling Handler, so that one
// response would leave the daemon without the policy.
func TestOptionsStarDoesNotBypassTheHeaders(t *testing.T) {
	srv := newTestServer(t, Config{Assets: spaHandler(testDist())})
	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "OPTIONS * HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodOptions})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("OPTIONS * X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := res.Header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") {
		t.Errorf("OPTIONS * CSP = %q, want the policy", got)
	}
}

// The asset handler matches every method, so the mux never reports a method
// mismatch by itself and a POST would otherwise be answered with the page.
func TestAWrongMethodOnARealRouteIsRejected(t *testing.T) {
	srv := newTestServer(t, Config{Assets: spaHandler(testDist())})
	res, err := http.Post(srv.Base()+"healthz", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d, want 405", res.StatusCode)
	}
	if got := res.Header.Get("Allow"); got == "" {
		t.Error("405 carries no Allow header")
	}
	if strings.Contains(string(body), "the page") {
		t.Errorf("POST /healthz served the SPA:\n%s", body)
	}
}

// The page is fetched by the browser before it can hold any credential, so the
// asset handler is deliberately outside whatever guard the API grows. This is
// the test that fails if someone puts it behind one.
func TestThePageIsServedWithoutACredential(t *testing.T) {
	srv := newTestServer(t, Config{Assets: spaHandler(testDist())})
	for _, path := range []string{"", "models", "assets/main.js"} {
		req, err := http.NewRequest(http.MethodGet, srv.Base()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("/%s answered %s with no credential", path, res.Status)
		}
	}
}

// http.Server only closes listeners Serve registered, so a Server that is built
// and then abandoned would hold its port for the life of the process. Closed
// twice, because the command path closes on a signal and again from a defer.
func TestCloseReleasesThePortAndCanBeCalledTwice(t *testing.T) {
	srv, err := New(withBackend(Config{}))
	if err != nil {
		t.Fatal(err)
	}
	addr := srv.Addr().String()
	if err := srv.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the port is still held after Close: %v", err)
	}
	_ = ln.Close()
}

// checkBound is the backstop for the rule checkBind can only approximate, and
// neither of its refusals is reachable through New on a machine whose resolver
// behaves - so they are checked directly.
func TestCheckBoundRefusesAnythingNotLoopbackTCP(t *testing.T) {
	if err := checkBound(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, nil); err != nil {
		t.Errorf("loopback TCP was refused: %v", err)
	}
	if err := checkBound(&net.TCPAddr{IP: net.IPv6loopback, Port: 1}, nil); err != nil {
		t.Errorf("loopback IPv6 was refused: %v", err)
	}
	routable := checkBound(&net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1}, nil)
	if routable == nil {
		t.Error("a routable address was accepted")
	} else if !strings.Contains(routable.Error(), "network can reach") {
		t.Errorf("error = %v, want it to say why", routable)
	}
	notTCP := checkBound(&net.UnixAddr{Name: "/tmp/x.sock", Net: "unix"}, nil)
	if notTCP == nil {
		t.Error("a non-TCP address was accepted")
	} else if !strings.Contains(notTCP.Error(), "not a TCP address") {
		t.Errorf("error = %v, want it to say the rule could not be checked", notTCP)
	}
}

// The asset handler is the mux's catch-all, so it has to lose to a real route.
func TestARealRouteWinsOverTheAssetCatchAll(t *testing.T) {
	srv := newTestServer(t, Config{Assets: spaHandler(fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html>the page")},
	})})

	res, err := http.Get(srv.Base() + "healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	if strings.Contains(string(body), "the page") {
		t.Fatalf("GET /healthz served the SPA:\n%s", body)
	}

	page, err := http.Get(srv.Base() + "models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = page.Body.Close() }()
	pageBody, _ := io.ReadAll(page.Body)
	if !strings.Contains(string(pageBody), "the page") {
		t.Fatalf("GET /models did not serve the SPA:\n%s", pageBody)
	}
}

// Every other test here serves an fstest.MapFS, so this is the only one that
// touches the real //go:embed filesystem - the arrangement the whole dist
// dance exists for. It skips unless the binary was built with a UI, which is
// why CI has a job that runs `npm run build` before calling it.
func TestTheEmbeddedBundleIsServed(t *testing.T) {
	if !HasAssets() {
		// A skip exits 0, so the CI job that builds the bundle before calling
		// this would pass while proving nothing. It sets the variable.
		if os.Getenv("AIGEM_REQUIRE_UI") != "" {
			t.Fatal("AIGEM_REQUIRE_UI is set but this binary carries no UI; did `npm run build` write to internal/web/dist?")
		}
		t.Skip("built without a UI; run `make web` first")
	}
	assets := Assets()
	if assets == nil {
		t.Fatal("Assets() is nil for a build that reports HasAssets()")
	}
	srv := newTestServer(t, Config{Assets: assets})

	res, err := http.Get(srv.Base() + "models")
	if err != nil {
		t.Fatal(err)
	}
	page, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /models = %d, want 200", res.StatusCode)
	}
	if !strings.Contains(string(page), `id="root"`) {
		t.Fatalf("the served page is not the built index.html:\n%s", page)
	}

	// Follow the bundle the page actually names, so this cannot pass against a
	// stale or partial dist.
	_, rest, ok := strings.Cut(string(page), `src="/assets/`)
	if !ok {
		t.Fatal("the page names no module under /assets/")
	}
	name, _, _ := strings.Cut(rest, `"`)
	bundle, err := http.Get(srv.Base() + "assets/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bundle.Body.Close() }()
	if bundle.StatusCode != http.StatusOK {
		t.Errorf("GET /assets/%s = %d, want 200", name, bundle.StatusCode)
	}
	if got := bundle.Header.Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Errorf("bundle Content-Type = %q, want javascript", got)
	}
	if got := bundle.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("bundle Cache-Control = %q, want it cached forever", got)
	}
}
