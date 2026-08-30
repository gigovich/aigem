package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testDist() fstest.MapFS {
	return fstest.MapFS{
		"index.html":      {Data: []byte("<!doctype html>the page")},
		"assets/main.js":  {Data: []byte("// the bundle")},
		"assets/main.css": {Data: []byte("/* the styles */")},
		"favicon.svg":     {Data: []byte("<svg/>")},
	}
}

// The bundle routes /models in the browser, so the only requests that reach the
// file server for it are the ones the browser could not answer: a reload, a
// bookmark, a link. Those have to become the page. A missing asset must not.
func TestAppRoutesFallThroughToThePageAndAssetsDoNot(t *testing.T) {
	dist := testDist()
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/models", true},
		{"/p/p-aigem/tickets", true},
		{"/chat/t_945dde0c47180ba8", true},
		// This UI is organised around projects, repositories and branches, and
		// any of those may carry a dot. "has an extension" would 404 every one of
		// them on a reload or a shared link.
		{"/p/my.project", true},
		{"/p/aigem/runs/release-1.2", true},
		{"/p/aigem/repos/site.example.com", true},
		// Repository names ending in a build-output extension are ordinary in
		// this ecosystem, and "has an extension" would 404 every one of them.
		{"/p/three.js", true},
		{"/p/aigem/repos/chart.js", true},
		{"/p/aigem/repos/tsconfig.json", true},
		{"/p/aigem/repos/site.css", true},
		{"/", false},
		{"/index.html", false},
		{"/assets/main.js", false},
		{"/favicon.svg", false},
		// A bundle renamed by a rebuild is gone, and saying so is the whole
		// point: served as HTML it becomes a parse error thrown from a script
		// tag, which points nowhere near the stale page that asked for it.
		{"/assets/stale.js", false},
		{"/assets/stale.woff2", false},
		{"/assets/STALE.CSS", false},
		// Anything under assets/ was emitted by the build, whatever its
		// extension, so a missing one is a 404 rather than a page.
		{"/assets/stale.mp4", false},
		{"/assets/nested/stale.js", false},
		{"/ASSETS/stale.js", false},
		// Clean anchors at the root, so a traversal is only ever an unknown
		// route. It becomes the page, and it can never name a file above dist.
		{"/../secret", true},
		// Nothing under /api/ is a screen. This handler is the mux's catch-all,
		// so a route the daemon does not serve arrives here, and answering it
		// with a page turns a clean 404 into "unexpected token <" inside
		// whichever client asked.
		{"/api/runs", false},
		{"/api/runs/abc/socket", false},
		{"/api/typo", false},
		{"/api", false},
		// Mis-cased too: Go's mux is case-sensitive, so this shadows no real
		// route, and "unexpected token <" is a worse answer than 404.
		{"/API/typo", false},
	} {
		if got := isAppRoute(dist, tc.path); got != tc.want {
			t.Errorf("isAppRoute(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestSPAHandlerServesThePageForARouteAnd404sAMissingAsset(t *testing.T) {
	srv := httptest.NewServer(spaHandler(testDist()))
	t.Cleanup(srv.Close)

	get := func(path string) (int, string) {
		t.Helper()
		res, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, string(body)
	}

	if code, body := get("/models"); code != http.StatusOK || body != "<!doctype html>the page" {
		t.Fatalf("GET /models = %d %q, want 200 and the page", code, body)
	}
	if code, body := get("/assets/main.js"); code != http.StatusOK || body != "// the bundle" {
		t.Fatalf("GET /assets/main.js = %d %q, want 200 and the bundle", code, body)
	}
	if code, _ := get("/assets/stale.js"); code != http.StatusNotFound {
		t.Fatalf("GET a missing asset = %d, want 404", code)
	}
}

// A `go build` with no `make web` before it must produce a binary that says so.
// This is the test that fails if someone ever makes the daemon require assets,
// which would break `go install ...@latest` on a machine with no node.
func TestABuildWithoutAUISaysSoInsteadOfServingABlankPage(t *testing.T) {
	if HasAssets() {
		t.Skip("this build carries a UI, so there is no missing-UI path to test")
	}
	if Assets() != nil {
		t.Fatal("Assets() returned a handler for a build with no dist/index.html")
	}

	srv := httptest.NewServer(noAssets())
	t.Cleanup(srv.Close)
	res, err := srv.Client().Get(srv.URL + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", res.StatusCode)
	}
	if !strings.Contains(string(body), "make web") {
		t.Errorf("body does not name the missing build step:\n%s", body)
	}
}

// The file server answers a directory with an index of everything in it. That
// is not a page, and no route asks for one.
func TestADirectoryIsNotServedAsAListing(t *testing.T) {
	srv := httptest.NewServer(spaHandler(testDist()))
	t.Cleanup(srv.Close)

	res, err := srv.Client().Get(srv.URL + "/assets/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("GET /assets/ = %d, want 404", res.StatusCode)
	}
	if strings.Contains(string(body), "main.js") {
		t.Errorf("the bundle was enumerated:\n%s", body)
	}
}

// Only what the build content-hashes may be cached forever. The page names
// those files, so caching it would make a rebuild invisible.
func TestOnlyHashedAssetsAreCachedForever(t *testing.T) {
	srv := httptest.NewServer(spaHandler(testDist()))
	t.Cleanup(srv.Close)

	for path, want := range map[string]string{
		"/assets/main.js":  "public, max-age=31536000, immutable",
		"/assets/main.css": "public, max-age=31536000, immutable",
		"/favicon.svg":     "no-cache",
		"/models":          "no-cache",
		"/":                "no-cache",
	} {
		res, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_ = res.Body.Close()
		if got := res.Header.Get("Cache-Control"); got != want {
			t.Errorf("%s Cache-Control = %q, want %q", path, got, want)
		}
	}
}
