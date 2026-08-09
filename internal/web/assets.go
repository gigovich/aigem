package web

import (
	"embed"
	"io/fs"
	"net/http"
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
func Assets() http.Handler {
	if !HasAssets() {
		return nil
	}
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil
	}
	return http.FileServerFS(sub)
}
