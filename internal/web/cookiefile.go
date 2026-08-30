package web

import (
	"encoding/json"
	"errors"
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
	// The cap is applied here as well as when a session is issued, because a
	// file two daemons share can hold more than either of them would keep. The
	// ones with longest left are the ones kept, which is the rule the eviction
	// on issue uses.
	for len(live) > maxCookieSessions {
		var soonest string
		var at time.Time
		for id, exp := range live {
			if soonest == "" || exp.Before(at) {
				soonest, at = id, exp
			}
		}
		delete(live, soonest)
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
// Every failure is reported except one: a file that does not parse, which no
// read-modify-write can ever get past. That one is started over from this
// change alone - never from the table in memory. Writing the table back would
// mean rewriting ids this daemon did not just touch, and an id in this table
// may be one a peer revoked, or one it issued and has since dropped: a repair
// that resurrects a revoked session, or erases a live one, is worse than the
// unreadable file it is repairing. Sessions the file loses this way stay live
// here until this daemon stops, which is what they would have been worth from
// an unreadable file anyway.
func (s *Server) persist(c cookieChange) error {
	if c.empty() {
		return nil
	}
	err := s.cookieStore.Update(func(f *cookieFile) error {
		if f.Sessions == nil {
			f.Sessions = map[string]time.Time{}
		}
		apply(f, c)
		return nil
	})
	if err == nil || !unparseable(err) {
		return err
	}
	slog.Warn("the browser sessions file does not parse; starting it again from this change",
		"path", s.cookieStore.Path(), "err", err)
	fresh := cookieFile{Sessions: map[string]time.Time{}}
	apply(&fresh, c)
	return s.cookieStore.Save(fresh)
}

// apply folds one change into a table, dropping whatever has expired on the way
// through. The expiry test is the one cookieOK and loadCookies use, so an entry
// this daemon has never heard of is judged exactly as its owner would judge it.
func apply(f *cookieFile, c cookieChange) {
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
}

// unparseable reports whether the file's contents are the problem, rather than
// the disk, the permissions or a peer holding the lock. Only the first is worth
// overwriting: the others describe a file that is still whatever it was, and a
// second write attempt is the same failure twice.
func unparseable(err error) bool {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	return errors.As(err, &syntax) || errors.As(err, &typ)
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
