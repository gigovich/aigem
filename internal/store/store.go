// Package store persists state under the state directory in two shapes: File,
// a whole JSON document replaced atomically, and Log, an append-only JSONL feed
// with a cursor.
//
// The pattern is the one internal/trust arrived at: a mutex around
// read-modify-write, an exclusive lock file around that for other processes, a
// uniquely named temp file renamed into place, and a sweep for temps left by a
// process killed between CreateTemp and Rename. It is here so that the
// documents the browser daemon keeps, and the conversations internal/session
// keeps, share one implementation rather than each reinventing a subset of it.
//
// Within one process the mutex is absolute. Between processes the two types
// differ, and the difference is deliberate:
//
// File tolerates a lock it cannot take. A writer that has waited lockWait
// proceeds anyway, because a peer killed mid-write must not be able to wedge
// the daemon, and the worst case is a lost update - never a damaged document,
// since a save only ever renames a fully written file into place.
//
// Log does not, and must not. Its append writes at an offset rather than
// renaming, so two writers proceeding unlocked pick the same sequence number
// and the same offset: one record is accepted and then destroyed, two clients
// are handed one cursor, and a half-written line can end up in the middle of a
// file that then reads back as damaged for good. So Log fails instead, and the
// caller retries - a lock whose owner is gone is broken after lockStale.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	lockPoll   = 20 * time.Millisecond
	lockStale  = 30 * time.Second
	tempOrphan = time.Hour
	tempSuffix = ".json.tmp"
)

// A variable so a test can shrink it: the behaviour that matters - a waiter
// giving up and proceeding unlocked - only appears once a holder outlives it,
// and a test that waits two real seconds per case is a test nobody runs.
var lockWait = 2 * time.Second

// File is a JSON document at a fixed path, read and written atomically.
//
// A missing file loads as the zero value rather than an error, which is what a
// caller holding a collection wants: it starts empty either way. A caller that
// has to tell "never written" from "written empty" - resuming a conversation by
// id, say - reads the file itself rather than through Load, as internal/session
// does; going through Exists first would be a second syscall and a window in
// between.
type File[T any] struct {
	path string
	// prefix is the pattern os.CreateTemp is given, so a temp is recognisable as
	// this package's - see isTempName, which is what sweeps the abandoned ones.
	prefix string
}

// New returns a store for the document at path. The directory is created on the
// first write, not here, so constructing a store never touches the disk.
//
// Two stores for the same path are equivalent: the serialization is keyed on
// the path, not on the value, so callers may construct one per request.
func New[T any](path string) *File[T] {
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if stem == "" || stem == "." {
		stem = "store"
	}
	// The temp suffix is deliberately not ".json": a pattern of stem + "-*.json"
	// collides with the namespace documents live in, and the sweep below would
	// then delete a real <stem>-7.json an hour after it was written.
	return &File[T]{path: path, prefix: stem + "-*" + tempSuffix}
}

// Path reports the document's location.
func (f *File[T]) Path() string { return f.path }

// Load reads the document, returning the zero value when it does not exist.
func (f *File[T]) Load() (T, error) {
	var v T
	data, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return v, nil
	}
	if err != nil {
		return v, fmt.Errorf("read %s: %w", f.path, err)
	}
	if err := json.Unmarshal(data, &v); err != nil {
		var zero T
		return zero, fmt.Errorf("parse %s: %w", f.path, err)
	}
	return v, nil
}

// Save replaces the document. It is a last-write-wins overwrite: a caller that
// needs the current value to compute the next one must use Update, which holds
// the lock across the read as well.
//
// The bytes reach the final path only through a rename, so a reader either sees
// the previous document or this one, never a mix.
func (f *File[T]) Save(v T) error {
	unlock := lockPath(f.path)
	defer unlock()
	return withFileLock(f.path, func() error { return f.save(v) })
}

// Update runs fn against the current document and saves the result, holding the
// lock for the whole read-modify-write. fn may be called with the zero value
// when the document does not exist yet, and the document is left untouched when
// fn returns an error.
//
// fn must not touch any store, including this one. The lock is a plain mutex, so
// a nested Update or Save on the same path deadlocks, and two goroutines nesting
// two stores in opposite orders deadlock against each other. Read what you need
// before calling Update and write it inside fn.
func (f *File[T]) Update(fn func(*T) error) error {
	unlock := lockPath(f.path)
	defer unlock()
	return withFileLock(f.path, func() error {
		v, err := f.Load()
		if err != nil {
			return err
		}
		if err := fn(&v); err != nil {
			return err
		}
		return f.save(v)
	})
}

// Delete removes the document. A document that is not there is not an error:
// the caller is asking for it to be gone, and it is.
//
// The lock is taken the same way a write takes it, so within this process a
// Delete cannot land between an Update's read and its save - which would
// otherwise resurrect the document from the value the Update was already
// holding. Between processes that is the same best-effort exclusion everything
// else here has: a peer whose Update proceeded past lockWait can still rename
// the document back into place after this removed it.
func (f *File[T]) Delete() error {
	unlock := lockPath(f.path)
	defer unlock()
	return withFileLock(f.path, func() error {
		if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", f.path, err)
		}
		syncDir(filepath.Dir(f.path))
		return nil
	})
}

func (f *File[T]) save(v T) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", f.path, err)
	}
	data = append(data, '\n')
	return writeAtomically(f.path, f.prefix, data)
}

// writeAtomically puts data at path through a temp file and a rename, so a
// reader sees either the previous contents or all of these bytes. prefix is the
// os.CreateTemp pattern, which is what makes an abandoned temp recognisable to
// the sweep.
func writeAtomically(path, prefix string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	sweepOrphanTemps(canonical(dir))

	// A unique temp name, not path+".tmp": two processes saving at once would
	// otherwise write the same file and rename a half-written one into place,
	// which corrupts the store permanently since Load refuses to parse it.
	// CreateTemp already creates at 0600.
	tmp, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	// Flushed before the rename, or a crash can leave the directory entry
	// durable while the blocks behind it are not - a truncated document that
	// Load refuses to parse and Update can never repair, because it loads first.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}
	if err := replace(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	syncDir(dir)
	return nil
}

// syncDir makes the rename itself durable. Best-effort: Windows cannot open a
// directory this way, and a crash that loses only the rename leaves the previous
// document intact, which is the state this package promises anyway.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// renameFile is a variable so a test can reach the retry loop below, which is
// otherwise unreachable on the platforms CI runs.
var renameFile = os.Rename

// replace renames into place, retrying on Windows only.
//
// Go opens files without FILE_SHARE_DELETE, so a rename onto a path another
// goroutine is midway through reading fails with a sharing violation. On Unix a
// rename over an open file always succeeds and a retry would only delay a real
// failure.
//
// The window is one read, but a caller can hold several open in a row -
// internal/session.List reads every conversation in the directory - so the
// budget is a second rather than the 90ms it started at. A save that fails
// where os.WriteFile would have succeeded is a lost turn, which is worse than
// the wait.
const (
	replaceRetries = 50
	replacePause   = 20 * time.Millisecond
)

func replace(from, to string) error {
	return replaceRetrying(from, to, runtime.GOOS == "windows")
}

func replaceRetrying(from, to string, retry bool) error {
	err := renameFile(from, to)
	if err == nil || !retry {
		return err
	}
	for range replaceRetries {
		time.Sleep(replacePause)
		if err = renameFile(from, to); err == nil {
			return nil
		}
	}
	return err
}

// withFileLock runs fn while holding an exclusive lock file, so a concurrent
// writer in *another process* cannot read a value this one is about to replace.
// Goroutines in this process are already serialized by lockPath.
//
// A writer that has waited lockWait proceeds without the lock. See the package
// doc: that is File's tolerance, and it is wrong for anything that writes in
// place.
func withFileLock(path string, fn func() error) error {
	return holdingLock(path, false, fn)
}

// withFileLockStrict is withFileLock for a caller that cannot survive running
// unlocked. It gives up rather than proceeding, so the caller can retry; a lock
// whose owner is gone is broken after lockStale, which bounds the retrying.
func withFileLockStrict(path string, fn func() error) error {
	return holdingLock(path, true, fn)
}

// holdingLock takes the lock file and runs fn.
//
// A lock left behind by a killed process is broken after lockStale. The lock
// holds a token so that breaking one cannot make its owner delete a lock a
// third party has since taken.
func holdingLock(path string, strict bool, fn func() error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	lock := path + ".lock"
	token := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	deadline := time.Now().Add(lockWait)
	for {
		h, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := h.WriteString(token)
			cerr := h.Close()
			if werr != nil || cerr != nil {
				// An empty lock is worse than none: releaseLock would read a token
				// that is not ours and decline to remove it, and every writer would
				// then pay the full lockWait until lockStale reclaims it. Drop it
				// and hold the mutex alone.
				os.Remove(lock)
				return fn()
			}
			defer releaseLock(lock, token)
			return fn()
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("lock %s: %w", path, err)
		}
		// The deadline is checked first, and against real elapsed time. A break
		// that cannot unlink - a read-only mount, a peer's lock under a sticky
		// bit - would otherwise loop straight back with no sleep and spin a core
		// forever, holding the in-process mutex while it did.
		if time.Now().After(deadline) {
			if strict {
				return fmt.Errorf("lock %s: still held after %s, and this write cannot "+
					"proceed without it; retry - a lock whose owner is gone is broken "+
					"after %s", path, lockWait, lockStale)
			}
			// Proceeding unlocked risks a lost update; corrupting the document
			// does not, since save renames a fully written file into place. The
			// risk is worth taking and not worth hiding: this is the only path on
			// which a write this package accepted can disappear, so it says so
			// rather than leaving a support case with nothing to go on.
			slog.Warn("proceeding without the store lock; a concurrent writer may lose an update",
				"path", path, "waited", lockWait)
			return fn()
		}
		if breakStaleLock(lock) {
			continue
		}
		time.Sleep(lockPoll)
	}
}

func releaseLock(lock, token string) {
	// Only ours: a holder that overran lockStale had its lock broken and
	// replaced, and removing the replacement would let a third writer in
	// alongside its owner. A read that fails for any reason other than the file
	// being gone leaves the lock alone - lockStale reclaims it, and guessing
	// wrong here is what this check exists to prevent.
	data, err := os.ReadFile(lock)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return
	case string(data) != token:
		return
	}
	os.Remove(lock)
}

// breakStaleLock reclaims a lock whose owner is gone, and reports whether it
// actually removed one. Reporting the removal is what keeps the caller's loop
// from spinning when the unlink cannot succeed.
//
// Both the modification time and the token are re-checked before the remove,
// because the decision to break was made from an earlier stat: an owner that
// released in the gap, and a third writer that took the lock in it, would
// otherwise be overridden - and a fresh lock carries a fresh mtime even when
// its token happens to match. The window cannot be closed entirely with plain
// files; what remains degrades to the lost update the lockWait fallback already
// tolerates.
func breakStaleLock(lock string) bool {
	info, err := os.Stat(lock)
	if err != nil || time.Since(info.ModTime()) <= lockStale {
		return false
	}
	stale, err := os.ReadFile(lock)
	if err != nil {
		return false
	}
	again, err := os.Stat(lock)
	if err != nil || !again.ModTime().Equal(info.ModTime()) || again.Size() != info.Size() {
		return false
	}
	current, err := os.ReadFile(lock)
	if err != nil || string(current) != string(stale) {
		return false
	}
	return os.Remove(lock) == nil
}

// sweepOrphanTemps removes temp files left by a process killed between
// CreateTemp and Rename, wherever in this directory they came from.
//
// The name has to match the shape CreateTemp produces - a stem, a hyphen, the
// digits it substitutes for the star, and the temp suffix - and nothing looser.
// The suffix is what keeps every real document out of reach, this store's and
// its neighbours' alike: a legitimate <state>/project-trust.json is every
// capability approval the user has ever given, and no document this package
// writes ends in .json.tmp.
//
// It is deliberately not keyed on the calling store's own stem. An orphan is an
// orphan whichever store abandoned it, the process that abandoned it is gone by
// definition, and a per-store predicate meant a per-store flag - one map entry
// per document for the life of the process, which for a store per session is a
// leak that grows with uptime.
func sweepOrphanTemps(dir string) {
	// At most once per tempOrphan per directory, rather than once per process.
	// Temps this process creates are removed by save's own defer, so a repeat
	// scan can only find an orphan from a peer that died after we started - and
	// a daemon that is the only writer of its state directory would never have a
	// "next process" to collect that one.
	if sweptRecently(dir) {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Not marked: a transient failure must not disable the sweep for a whole
		// tempOrphan.
		return
	}
	markSwept(dir)
	for _, e := range entries {
		if e.IsDir() || !isTempName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < tempOrphan {
			continue
		}
		os.Remove(filepath.Join(dir, e.Name()))
	}
}

func isTempName(name string) bool {
	middle, ok := strings.CutSuffix(name, tempSuffix)
	if !ok {
		return false
	}
	// The last hyphen, not the first: a stem may hold hyphens of its own - a
	// session id is "20260830-150405-9f3c" - and only the run of digits after
	// the final one is what CreateTemp added.
	cut := strings.LastIndex(middle, "-")
	if cut <= 0 {
		return false
	}
	digits := middle[cut+1:]
	if digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// held is one document's in-process mutex and the number of callers that are
// holding or waiting on it. The count is what lets the entry be dropped when
// the last one leaves: a map that only ever grew held a mutex per document for
// the life of the process, and a store per session makes that unbounded.
type held struct {
	mu   sync.Mutex
	refs int
}

// maxSweptDirs bounds the table of directories already swept. Forgetting one
// costs a ReadDir; never forgetting is the leak the refcounting above exists to
// remove, relocated.
const maxSweptDirs = 256

var (
	pathsMu sync.Mutex
	paths   = map[string]*held{}
	swept   = map[string]time.Time{}
)

func canonical(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return path
}

// lockPath serializes writers to one document within this process and returns
// the release.
//
// The file lock alone is not enough: its contended path sleeps in 20ms steps
// and, after lockWait, proceeds *unlocked* so a stuck peer cannot wedge the
// daemon. Between goroutines that fallback is a silently lost update, and the
// daemon's own concurrent request handlers are exactly that case.
func lockPath(path string) func() {
	key := canonical(path)
	pathsMu.Lock()
	h := paths[key]
	if h == nil {
		h = &held{}
		paths[key] = h
	}
	// Counted before the mutex is taken, so a waiter keeps the entry alive
	// across the release below - dropping it while someone waits would hand the
	// next caller a second mutex for the same document, which is no mutex at all.
	h.refs++
	pathsMu.Unlock()
	h.mu.Lock()
	return func() {
		h.mu.Unlock()
		pathsMu.Lock()
		h.refs--
		if h.refs == 0 {
			delete(paths, key)
		}
		pathsMu.Unlock()
	}
}

func sweptRecently(key string) bool {
	pathsMu.Lock()
	defer pathsMu.Unlock()
	at, ok := swept[key]
	return ok && time.Since(at) < tempOrphan
}

func markSwept(key string) {
	pathsMu.Lock()
	defer pathsMu.Unlock()
	if len(swept) >= maxSweptDirs {
		// Wholesale, not one entry: this runs under the mutex every writer takes,
		// and evicting one at a time would walk the table on every new directory.
		// The cost of forgetting is a ReadDir the next writer pays.
		clear(swept)
	}
	swept[key] = time.Now()
}
