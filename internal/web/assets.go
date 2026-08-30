package web

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// Go has no MIME entry for .webmanifest, so the file server sniffs it as text -
// and every response carries nosniff, so the browser then refuses it. Registered
// here rather than beside the manifest that needs it, because the failure is a
// silent refusal and the fix is not obvious from it.
func init() {
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

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
// is one bundle that routes between its screens in the browser, so /models only
// ever reaches the server as a reload, a bookmark or a link someone sent - and
// every one of those is a request the file server has nothing to match.
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
		if isDirectory(fsys, r.URL.Path) {
			// Otherwise the file server answers with an index of the bundle. It is
			// not a secret, but it is not a page either, and no route asks for one.
			http.NotFound(w, r)
			return
		}
		// Only what the build content-hashes may be cached: a changed file is
		// then a changed name, so this can never serve a stale one. Everything
		// else - the page above all, which names the hashed files - has to be
		// revalidated, or a rebuild is invisible until the browser is emptied.
		if strings.HasPrefix(strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/"), "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if isAppRoute(fsys, r.URL.Path) {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}

// isDirectory reports whether the path names a directory in the bundle. The
// root is excluded: the file server turns it into index.html, which is the page.
func isDirectory(fsys fs.FS, urlPath string) bool {
	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if name == "" || name == "." || !fs.ValidPath(name) {
		return false
	}
	info, err := fs.Stat(fsys, name)
	return err == nil && info.IsDir()
}

// rootExts are the extensions a build emits at the top of the bundle, beside
// index.html. Everything else it emits lives under assets/.
var rootExts = map[string]bool{
	".html": true, ".js": true, ".css": true, ".txt": true, ".xml": true,
	".svg": true, ".png": true, ".ico": true, ".webmanifest": true,
}

// isBuiltAsset reports whether a path names something the build produced rather
// than a screen. Such a request was made by name - by a script tag, a stylesheet
// link, the font loader - and a missing one has to 404: served as HTML it
// becomes a syntax error thrown from a script tag, which points nowhere near the
// stale page that asked for it.
//
// The rule is where the path is, not whether it has a dot, because this UI is
// organised around projects, repositories and branches. "Anything with an
// extension is a file" would 404 a screen at /p/my.project - and, worse, /p/
// three.js and /p/tsconfig.json, which are ordinary repository names in exactly
// the ecosystem this tool is used on.
func isBuiltAsset(name string) bool {
	if strings.HasPrefix(name, "assets/") {
		return true
	}
	if strings.Contains(name, "/") {
		return false
	}
	return rootExts[strings.ToLower(path.Ext(name))]
}

// isAppRoute reports whether a path should fall through to the page rather than
// 404. Only paths that could be a screen do.
//
// Two exclusions carry the weight: a name that is built output (see
// isBuiltAsset), and anything under /api/. This handler is the mux's catch-all, so
// every route the daemon does not serve lands here, and a client asking for one
// that does not exist has to be told 404 rather than handed a page that its JSON
// decoder reports as "unexpected token <".
func isAppRoute(fsys fs.FS, urlPath string) bool {
	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if name == "" || name == "." || isBuiltAsset(name) {
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

// noAssets answers every page request when the binary was built without a UI.
// A blank page would look like a bug in the app; this says why there is none.
func noAssets() http.Handler {
	const body = "aigem was built without a browser UI.\n\n" +
		"A plain `go build` or `go install` deliberately produces a binary with no UI,\n" +
		"so installing aigem never requires a node toolchain. From a checkout, build\n" +
		"one with:\n\n    make web && make build\n"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(body))
	})
}
