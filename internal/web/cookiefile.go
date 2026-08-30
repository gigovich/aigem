package web

import (
	"log/slog"
	"time"

	"github.com/gigovich/aigem/internal/store"
)

// The browser sessions outlive the process when the daemon is given a file for
// them. It is its own file rather than a field in any record that says where a
// running daemon is: those are removed on a clean exit, and a session signed in
// yesterday has to survive exactly the restart that removes them.
//
// The warnings go to the process logger. This package logs nowhere else, and
// threading a logger through Config for two lines that only ever say "sign in
// again" would be more plumbing than signal.

// cookieFile is the file's shape.
type cookieFile struct {
	Sessions map[string]time.Time `json:"sessions"`
}

// cookieStoreFor returns the store for path, or nil when the daemon was given
// no file and the table lives in memory alone.
func cookieStoreFor(path string) *store.File[cookieFile] {
	if path == "" {
		return nil
	}
	return store.New[cookieFile](path)
}

// loadCookies reads the table back, without the entries that expired while the
// daemon was down.
//
// Every failure here is an empty table and a warning, never a refusal to start:
// the operator locked out by an unreadable file would be locked out of the only
// interface that can sign them back in.
func loadCookies(f *store.File[cookieFile]) map[string]time.Time {
	live := map[string]time.Time{}
	if f == nil {
		return live
	}
	stored, err := f.Load()
	if err != nil {
		slog.Warn("could not read the browser sessions; every browser has to sign in again",
			"path", f.Path(), "err", err)
		return live
	}
	now := time.Now()
	for id, exp := range stored.Sessions {
		// Both halves of the test match cookieOK - it refuses an empty value and
		// honours an expiry of exactly now - so the file restores exactly the
		// entries the table would have answered for.
		if id != "" && !now.After(exp) {
			live[id] = exp
		}
	}
	return live
}

// saveCookies writes the table. The caller holds s.mu, so what lands on disk is
// the table as it was when it changed.
//
// A failed write is served past: the session it could not record still works
// until this daemon stops, and refusing the request that caused it would turn a
// disk problem into a sign-in that fails with nothing to see.
func (s *Server) saveCookies() {
	if s.cookieStore == nil {
		return
	}
	// Close empties the table and then keeps serving until the listener is shut,
	// so a page reloading through a deploy would otherwise reach issueCookie
	// against an empty table and write a file holding one session - signing out
	// every other browser, which is the failure this file exists to prevent.
	if s.closed {
		return
	}
	if err := s.cookieStore.Save(cookieFile{Sessions: s.cookies}); err != nil {
		slog.Warn("could not record the browser sessions; a restart will sign them out",
			"path", s.cookieStore.Path(), "err", err)
	}
}
