package web

import (
	"io"
	"net/http"
	"net/http/httptest"
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

// The bundle routes /chat in the browser, so the only requests that reach the
// file server for it are the ones the browser could not answer: a reload, a
// bookmark, a link. Those have to become the page. A missing asset must not.
func TestAppRoutesFallThroughToThePageAndAssetsDoNot(t *testing.T) {
	dist := testDist()
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/chat", true},
		{"/chat/t_945dde0c47180ba8", true},
		{"/", false},
		{"/index.html", false},
		{"/assets/main.js", false},
		{"/favicon.svg", false},
		// A bundle renamed by a rebuild is gone, and saying so is the whole
		// point: served as HTML it becomes a parse error thrown from a script
		// tag, which points nowhere near the stale page that asked for it.
		{"/assets/stale.js", false},
		// Clean anchors at the root, so a traversal is only ever an unknown
		// route. It becomes the page, and it can never name a file above dist.
		{"/../secret", true},
		// Nothing under /api/ is a screen. This handler is the mux's catch-all,
		// so a route the daemon does not serve arrives here, and answering it
		// with a page turns a clean 404 into "unexpected token <" inside
		// whichever client asked.
		{"/api/sessions", false},
		{"/api/sessions/abc/socket", false},
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

	if code, body := get("/chat"); code != http.StatusOK || body != "<!doctype html>the page" {
		t.Fatalf("GET /chat = %d %q, want 200 and the page", code, body)
	}
	if code, body := get("/assets/main.js"); code != http.StatusOK || body != "// the bundle" {
		t.Fatalf("GET /assets/main.js = %d %q, want 200 and the bundle", code, body)
	}
	if code, _ := get("/assets/stale.js"); code != http.StatusNotFound {
		t.Fatalf("GET a missing asset = %d, want 404", code)
	}
}
