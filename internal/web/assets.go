package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The built UI is embedded, but it is not committed: `go install
// github.com/gigovich/aigem/cmd/aigem@latest` is the documented way to get
// aigem and has to keep working on a machine with no node toolchain. So the
// directory holds only a .gitkeep in git, `make web` fills it, and the release
// build runs that before compiling. A plain build produces a binary that says
// it has no UI rather than one that serves a blank page.
//
//go:embed all:dist
var distFS embed.FS

// HasAssets reports whether this build carries a UI.
func HasAssets() bool {
	_, err := fs.Stat(distFS, "dist/index.html")
	return err == nil
}

// Assets serves the built UI, or nil when there is none.
//
// A path that names no file is answered with index.html instead of 404. The UI
// is one bundle with two screens and it routes between them in the browser, so
// /chat only ever reaches the server as a reload, a bookmark or a link someone
// sent - and every one of those is a request the file server has nothing to
// match.
func Assets() http.Handler {
	if !HasAssets() {
		return nil
	}
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil
	}
	return spaHandler(sub)
}

func spaHandler(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAppRoute(fsys, r.URL.Path) {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}

// isAppRoute reports whether a path should fall through to the page rather than
// 404. Only paths that could be a screen do.
//
// Two exclusions carry the weight. Anything whose last segment has an extension
// was asked for by name, and answering a missing bundle with HTML turns a plain
// 404 into a syntax error thrown from a script tag. And nothing under /api/ is
// ever a screen: this handler is the mux's catch-all, so every route the daemon
// does not serve lands here, and a client asking a session daemon for a chat
// route - or either for a typo - has to be told 404 rather than handed a page
// that its JSON decoder reports as "unexpected token <".
func isAppRoute(fsys fs.FS, urlPath string) bool {
	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if name == "" || name == "." || strings.Contains(path.Base(name), ".") {
		return false
	}
	// Case-insensitively, so a mis-cased URL gets the 404 it deserves rather
	// than a page its JSON decoder reports as "unexpected token <". Go's mux is
	// case-sensitive, so no real route is shadowed by being lenient here.
	if lower := strings.ToLower(name); lower == "api" || strings.HasPrefix(lower, "api/") {
		return false
	}
	if !fs.ValidPath(name) {
		return false
	}
	_, err := fs.Stat(fsys, name)
	return err != nil
}
