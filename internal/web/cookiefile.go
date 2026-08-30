package web

import (
	"fmt"
	"log/slog"
	"maps"
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

// pendingLocked takes the snapshot of the table that persist will write, and
// stamps it with a generation. The caller holds s.mu.
//
// The write itself happens outside s.mu, because it is not cheap: an
// inter-process lock file that a peer may hold for up to two seconds, a
// directory scan, and two fsyncs. s.mu is the mutex every authenticated request
// takes to check its cookie, so holding it across all that would queue the
// whole daemon behind one sign-in.
//
// A nil snapshot means there is nothing to write to, or the daemon is stopping.
// Close empties the table and then keeps serving until the listener is shut, so
// a page reloading through a deploy would otherwise write a file holding the one
// session it just minted - signing out every other browser, which is the failure
// this file exists to prevent.
func (s *Server) pendingLocked() (map[string]time.Time, uint64) {
	if s.cookieStore == nil || s.closed {
		return nil, 0
	}
	s.cookieGen++
	return maps.Clone(s.cookies), s.cookieGen
}

// persist writes a snapshot taken by pendingLocked.
//
// Snapshots are written one at a time and in order. Taking them under s.mu and
// writing them outside it means two mutations can reach here in the opposite
// order, and an older table landing last would resurrect a session a newer one
// removed - so a snapshot a newer write has already claimed is dropped rather
// than written. The claim is staked before the write, so a failed newer write
// does not let an older table win either.
func (s *Server) persist(sessions map[string]time.Time, gen uint64) error {
	if sessions == nil {
		return nil
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if gen <= s.savedGen {
		return nil
	}
	s.savedGen = gen
	if err := s.cookieStore.Save(cookieFile{Sessions: sessions}); err != nil {
		return fmt.Errorf("write %s: %w", s.cookieStore.Path(), err)
	}
	return nil
}

// ForgetSessions removes the browser sessions kept at path, so every browser
// has to sign in again with a token from the terminal.
//
// It is the answer to a token that got out. Restarting the daemon rotates the
// token but not the sessions - that is the whole point of keeping them - so a
// cookie an attacker traded that token for would otherwise renew itself for as
// long as it was used. Stop the daemon first: a running one holds the table in
// memory and would write it back.
func ForgetSessions(path string) error {
	f := cookieStoreFor(path)
	if f == nil {
		return nil
	}
	return f.Delete()
}
