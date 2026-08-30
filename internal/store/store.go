// Package store persists a JSON document atomically, so two writers cannot
// leave a half-written file behind.
//
// The pattern is the one internal/trust arrived at: a mutex around
// read-modify-write, an exclusive lock file around that for other processes, a
// uniquely named temp file renamed into place, and a sweep for temps left by a
// process killed between CreateTemp and Rename. This package exists so the web
// daemon's project, ticket and worktree stores share that behaviour instead of
// each reinventing a subset of it.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
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
)

// A variable so a test can shrink it: the behaviour that matters - a waiter
// giving up and proceeding unlocked - only appears once a holder outlives it,
// and a test that waits two real seconds per case is a test nobody runs.
var lockWait = 2 * time.Second

// File is a JSON document at a fixed path, read and written atomically.
//
// A missing file loads as the zero value rather than an error: every caller
// here starts from an empty collection, and distinguishing "not created yet"
// from "empty" has no meaning for any of them.
type File[T any] struct {
	path   string
	stem   string
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
	return &File[T]{path: path, stem: stem, prefix: stem + "-*.json"}
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
	return f.withFileLock(func() error { return f.save(v) })
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
	return f.withFileLock(func() error {
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

func (f *File[T]) save(v T) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", f.path, err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f.sweepOrphanTemps(dir)

	// A unique temp name, not path+".tmp": two processes saving at once would
	// otherwise write the same file and rename a half-written one into place,
	// which corrupts the store permanently since Load refuses to parse it.
	// CreateTemp already creates at 0600.
	tmp, err := os.CreateTemp(dir, f.prefix)
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}
	if err := replace(tmp.Name(), f.path); err != nil {
		return fmt.Errorf("replace %s: %w", f.path, err)
	}
	return nil
}

// replace renames into place, retrying on Windows only.
//
// Go opens files without FILE_SHARE_DELETE, so a rename onto a path another
// goroutine is midway through reading fails with a sharing violation. The read
// window is one ReadFile, so a handful of short retries closes it; on Unix a
// rename over an open file always succeeds and a retry would only delay a real
// failure.
func replace(from, to string) error {
	err := os.Rename(from, to)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	for range 9 {
		time.Sleep(10 * time.Millisecond)
		if err = os.Rename(from, to); err == nil {
			return nil
		}
	}
	return err
}

// withFileLock runs fn while holding an exclusive lock file, so a concurrent
// writer in *another process* cannot read a value this one is about to replace.
// Goroutines in this process are already serialized by lockPath.
//
// A lock left behind by a killed process is broken after lockStale. The lock
// holds a token so that breaking one cannot make its owner delete a lock a
// third party has since taken.
func (f *File[T]) withFileLock(fn func() error) error {
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	lock := f.path + ".lock"
	token := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	for waited := time.Duration(0); ; waited += lockPoll {
		h, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := h.WriteString(token)
			cerr := h.Close()
			if werr != nil || cerr != nil {
				// An empty lock is worse than none: releaseLock would read a token
				// that is not ours and decline to remove it, wedging every writer
				// for lockStale. Drop it and hold the mutex alone.
				os.Remove(lock)
				return fn()
			}
			defer releaseLock(lock, token)
			return fn()
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("lock %s: %w", f.path, err)
		}
		if breakStaleLock(lock) {
			continue
		}
		if waited >= lockWait {
			// Proceeding unlocked risks a lost update; corrupting the store does
			// not, since save renames a fully written file into place.
			return fn()
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
// removed one.
//
// The token is re-read immediately before the remove so that an owner that
// released between the stat and here - and a third writer that took the lock in
// that gap - are not silently overridden. The gap cannot be closed entirely with
// plain files, but it is now a few instructions wide rather than a syscall, and
// what remains degrades to the same lost update the lockWait fallback already
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
	again, err := os.ReadFile(lock)
	if err != nil || string(again) != string(stale) {
		return false
	}
	os.Remove(lock)
	return true
}

// sweepOrphanTemps removes temp files left by a process killed between
// CreateTemp and Rename.
//
// The name has to match the shape CreateTemp produces - the stem, a hyphen,
// the digits it substitutes for the star, and the extension - and nothing
// looser. A prefix test alone would match a *different* store's document in
// the same directory: a store at <state>/project.json has the stem "project",
// and <state>/project-trust.json is every capability approval the user has
// ever given.
func (f *File[T]) sweepOrphanTemps(dir string) {
	// Once per document per process, not once per directory: the predicate below
	// is keyed on this store's stem, so a directory-wide flag would let the first
	// store to write it disable the sweep for every other one - and several
	// stores sharing a directory is what this package is for. Temps this process
	// creates are removed by save's own defer, so the only thing a repeat scan
	// could find is an orphan from a peer that died after we started.
	key := sweepKey(f.path)
	if alreadySwept(key) {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Not marked: a transient failure must not disable the sweep for the life
		// of the process.
		return
	}
	markSwept(key)
	for _, e := range entries {
		if e.IsDir() || !f.isTempName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < tempOrphan {
			continue
		}
		os.Remove(filepath.Join(dir, e.Name()))
	}
}

func (f *File[T]) isTempName(name string) bool {
	middle, ok := strings.CutSuffix(name, ".json")
	if !ok {
		return false
	}
	middle, ok = strings.CutPrefix(middle, f.stem+"-")
	if !ok || middle == "" {
		return false
	}
	for _, r := range middle {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

var (
	pathsMu sync.Mutex
	paths   = map[string]*sync.Mutex{}
	swept   = map[string]bool{}
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
	m := paths[key]
	if m == nil {
		// Never evicted: the set of documents a process opens is bounded by the
		// projects it knows about, and dropping one while a goroutine waits on
		// it would defeat the point.
		m = &sync.Mutex{}
		paths[key] = m
	}
	pathsMu.Unlock()
	m.Lock()
	return m.Unlock
}

// sweepKey is the document itself, canonicalised so two spellings of one path
// are one key.
func sweepKey(path string) string { return canonical(path) }

func alreadySwept(key string) bool {
	pathsMu.Lock()
	defer pathsMu.Unlock()
	return swept[key]
}

func markSwept(key string) {
	pathsMu.Lock()
	defer pathsMu.Unlock()
	swept[key] = true
}
