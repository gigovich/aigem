package web

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"time"
)

// The browser sessions outlive the process when the daemon is given a file for
// them. It is its own file rather than a field in web.json or chat.json,
// because those records are removed on a clean exit - they say where a running
// daemon is - and a session signed in yesterday has to survive exactly the
// restart that removes them.
//
// The warnings go to the process logger. This package logs nowhere else, and
// threading a logger through Config for two lines that only ever say "sign in
// again" would be more plumbing than signal.

// cookieStore is the file's shape.
type cookieStore struct {
	Sessions map[string]time.Time `json:"sessions"`
}

// loadCookies reads the table back, without the entries that expired while the
// daemon was down.
//
// Every failure here is an empty table and a warning, never a refusal to start:
// the operator locked out by an unreadable file would be locked out of the only
// interface that can sign them back in.
func loadCookies(path string) map[string]time.Time {
	live := map[string]time.Time{}
	if path == "" {
		return live
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("could not read the browser sessions; every browser has to sign in again",
				"path", path, "err", err)
		}
		return live
	}
	var stored cookieStore
	if err := json.Unmarshal(b, &stored); err != nil {
		slog.Warn("the browser sessions file is unreadable; every browser has to sign in again",
			"path", path, "err", err)
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
	if s.cookieFile == "" {
		return
	}
	// Close empties the table and then keeps serving until the listener is shut,
	// so a page reloading through a deploy would otherwise reach issueCookie
	// against an empty table and write a file holding one session - signing out
	// every other browser, which is the failure this file exists to prevent.
	if s.closed {
		return
	}
	b, err := json.MarshalIndent(cookieStore{Sessions: s.cookies}, "", "  ")
	if err == nil {
		err = os.WriteFile(s.cookieFile, b, 0o600)
	}
	if err != nil {
		slog.Warn("could not record the browser sessions; a restart will sign them out",
			"path", s.cookieFile, "err", err)
	}
}
