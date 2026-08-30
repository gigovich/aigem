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
	lockWait   = 2 * time.Second
	lockStale  = 30 * time.Second
	tempOrphan = time.Hour
)

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
			_, _ = h.WriteString(token)
			h.Close()
			defer releaseLock(lock, token)
			return fn()
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("lock %s: %w", f.path, err)
		}
		if info, serr := os.Stat(lock); serr == nil && time.Since(info.ModTime()) > lockStale {
			os.Remove(lock)
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
	// alongside its owner.
	if data, err := os.ReadFile(lock); err == nil && string(data) != token {
		return
	}
	os.Remove(lock)
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
	if !markSwept(dir) {
		// Once per directory per process. Temps this process creates are removed
		// by save's own defer, so re-scanning the directory on every write buys
		// nothing and costs a full ReadDir inside the lock.
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
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

// lockPath serializes writers to one document within this process and returns
// the release.
//
// The file lock alone is not enough: its contended path sleeps in 20ms steps
// and, after lockWait, proceeds *unlocked* so a stuck peer cannot wedge the
// daemon. Between goroutines that fallback is a silently lost update, and the
// daemon's own concurrent request handlers are exactly that case.
func lockPath(path string) func() {
	key := path
	if abs, err := filepath.Abs(path); err == nil {
		key = filepath.Clean(abs)
	}
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

func markSwept(dir string) bool {
	pathsMu.Lock()
	defer pathsMu.Unlock()
	if swept[dir] {
		return false
	}
	swept[dir] = true
	return true
}
