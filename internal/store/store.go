// Package store persists a JSON document atomically, so two aigem processes
// writing at once cannot leave a half-written file behind.
//
// The pattern is the one internal/trust arrived at: an exclusive lock file
// around read-modify-write, a uniquely named temp file renamed into place, and
// a sweep for temps left by a process killed between CreateTemp and Rename.
// This package exists so the web daemon's project, ticket and worktree stores
// share that behaviour instead of each reinventing a subset of it.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	prefix string
}

// New returns a store for the document at path. The directory is created on
// the first Save, not here, so constructing a store never touches the disk.
func New[T any](path string) *File[T] {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if base == "" {
		base = "store"
	}
	return &File[T]{path: path, prefix: base + "-*.json"}
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

// Save writes the document. The bytes reach the final path only through a
// rename, so a reader either sees the previous document or this one.
func (f *File[T]) Save(v T) error {
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
	tmp, err := os.CreateTemp(dir, f.prefix)
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.path)
}

// Update runs fn against the current document and saves the result, holding an
// exclusive lock for the whole read-modify-write. fn may be called with the
// zero value when the document does not exist yet.
func (f *File[T]) Update(fn func(*T) error) error {
	return f.withLock(func() error {
		v, err := f.Load()
		if err != nil {
			return err
		}
		if err := fn(&v); err != nil {
			return err
		}
		return f.Save(v)
	})
}

// withLock runs fn while holding an exclusive lock on the document, so a
// concurrent Update cannot read a value another process is about to replace. A
// lock left behind by a killed process is broken after lockStale.
func (f *File[T]) withLock(fn func() error) error {
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	lock := f.path + ".lock"
	for waited := time.Duration(0); ; waited += lockPoll {
		h, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			h.Close()
			defer os.Remove(lock)
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
			// not, since Save renames a fully written file into place.
			return fn()
		}
		time.Sleep(lockPoll)
	}
}

func (f *File[T]) sweepOrphanTemps(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	stem := strings.TrimSuffix(f.prefix, "*.json")
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), stem) || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if e.Name() == filepath.Base(f.path) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < tempOrphan {
			continue
		}
		os.Remove(filepath.Join(dir, e.Name()))
	}
}
