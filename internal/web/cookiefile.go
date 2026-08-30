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

// cookieChange is one mutation of the table: the sessions it adds and the ones
// it removes.
//
// The file is updated with a change rather than replaced with a snapshot, which
// is what lets two daemons share a state directory. Each held its whole table in
// memory and wrote it wholesale, so the second to write erased every session the
// first had issued - the exact failure this file exists to prevent, reached by
// the dev workflow's own advice to start a second daemon on a fixed port.
// Additions and removals of distinct ids compose in any order; whole tables do
// not compose at all.
type cookieChange struct {
	add    map[string]time.Time
	remove []string
}

func (c cookieChange) empty() bool { return len(c.add) == 0 && len(c.remove) == 0 }

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

// pendingLocked records a change for persist to write. The caller holds s.mu.
//
// The write itself happens outside s.mu, because it is not cheap: an
// inter-process lock file that a peer may hold for up to two seconds, a
// directory scan, and two fsyncs. s.mu is the mutex every authenticated request
// takes to check its cookie, so holding it across all that would queue the
// whole daemon behind one sign-in.
//
// An empty change means there is nothing to write to, or the daemon is stopping.
// Close empties the table and then keeps serving until the listener is shut, so
// a page reloading through a deploy would otherwise record a session nothing is
// going to honour.
func (s *Server) pendingLocked(c cookieChange) cookieChange {
	if s.cookieStore == nil || s.closed {
		return cookieChange{}
	}
	return c
}

// persist applies a change to the file.
//
// A parse failure is repaired rather than returned: a file this daemon cannot
// read is one no browser can be signed out of and none can be signed in to, and
// the table in memory is the best account of the sessions anyone still holds.
// That is the one path that overwrites rather than composes, and it is the same
// choice loadCookies makes at startup.
func (s *Server) persist(c cookieChange) error {
	if c.empty() {
		return nil
	}
	err := s.cookieStore.Update(func(f *cookieFile) error {
		if f.Sessions == nil {
			f.Sessions = map[string]time.Time{}
		}
		now := time.Now()
		for id, exp := range f.Sessions {
			if id == "" || now.After(exp) {
				delete(f.Sessions, id)
			}
		}
		for _, id := range c.remove {
			delete(f.Sessions, id)
		}
		for id, exp := range c.add {
			f.Sessions[id] = exp
		}
		return nil
	})
	if err == nil {
		return nil
	}
	slog.Warn("the browser sessions file could not be updated; rewriting it from this daemon's table",
		"path", s.cookieStore.Path(), "err", err)
	s.mu.Lock()
	table := make(map[string]time.Time, len(s.cookies))
	for id, exp := range s.cookies {
		table[id] = exp
	}
	s.mu.Unlock()
	return s.cookieStore.Save(cookieFile{Sessions: table})
}

// ForgetSessions removes the browser sessions kept at path, so every browser
// has to sign in again with a token from the terminal.
//
// It is the answer to a token that got out. Restarting the daemon rotates the
// token but not the sessions - that is the whole point of keeping them - so a
// cookie an attacker traded that token for would otherwise renew itself for as
// long as it was used. Stop the daemon first: a running one holds the table in
// memory, goes on honouring every cookie in it, and records the next change
// against the file this removed.
func ForgetSessions(path string) error {
	f := cookieStoreFor(path)
	if f == nil {
		return nil
	}
	return f.Delete()
}
