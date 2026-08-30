package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

type doc struct {
	Version int            `json:"version"`
	Counts  map[string]int `json:"counts"`
}

func TestLoadMissingReturnsZero(t *testing.T) {
	s := New[doc](filepath.Join(t.TempDir(), "nope.json"))
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != 0 || got.Counts != nil {
		t.Fatalf("Load of a missing file = %+v, want zero value", got)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := New[doc](filepath.Join(t.TempDir(), "sub", "doc.json"))
	want := doc{Version: 3, Counts: map[string]int{"a": 1}}
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != want.Version || got.Counts["a"] != 1 {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

// The document can hold credentials-adjacent state, so it must not be
// world-readable even when the process umask is permissive.
func TestSaveIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "doc.json")
	s := New[doc](path)
	if err := s.Save(doc{Version: 1}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %04o, want 0600", perm)
	}
}

func TestLoadRejectsCorruptDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New[doc](path).Load(); err == nil {
		t.Fatal("Load of a corrupt document succeeded, want error")
	}
}

func TestUpdateStartsFromZeroValue(t *testing.T) {
	s := New[doc](filepath.Join(t.TempDir(), "doc.json"))
	err := s.Update(func(d *doc) error {
		if d.Version != 0 {
			t.Errorf("Update saw version %d, want 0", d.Version)
		}
		d.Version = 7
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != 7 {
		t.Fatalf("version = %d, want 7", got.Version)
	}
}

// An Update that fails must leave the previous document intact, since the
// caller's next Load has to see a state it can reason about.
func TestUpdateDoesNotSaveOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.json")
	s := New[doc](path)
	if err := s.Save(doc{Version: 1}); err != nil {
		t.Fatal(err)
	}
	wantErr := os.ErrInvalid
	if err := s.Update(func(d *doc) error { d.Version = 99; return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("Update err = %v, want %v", err, wantErr)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Fatalf("version = %d, want the document untouched at 1", got.Version)
	}
}

// Concurrent writers are the reason the lock exists: every increment has to
// survive, which only holds if load-modify-save is serialized.
func TestConcurrentUpdatesAllLand(t *testing.T) {
	s := New[doc](filepath.Join(t.TempDir(), "doc.json"))
	const writers = 12
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Update(func(d *doc) error {
				if d.Counts == nil {
					d.Counts = map[string]int{}
				}
				d.Counts["n"]++
				return nil
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Counts["n"] != writers {
		t.Fatalf("counts[n] = %d, want %d", got.Counts["n"], writers)
	}
}

func TestSaveSweepsOrphanTempsButKeepsRecentOnes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.json")
	s := New[doc](path)

	stale := filepath.Join(dir, "doc-123456.json")
	fresh := filepath.Join(dir, "doc-999999.json")
	other := filepath.Join(dir, "unrelated-1.json")
	for _, p := range []string{stale, fresh, other} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * tempOrphan)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatal(err)
	}

	if err := s.Save(doc{Version: 1}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a temp older than tempOrphan survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a recent temp was swept; another process may still be writing it")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("a file belonging to another store was swept")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the document itself is missing: %v", err)
	}
}

// A lock left by a killed process must not wedge the store forever.
func TestUpdateBreaksStaleLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.json")
	s := New[doc](path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	lock := path + ".lock"
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- s.Update(func(d *doc) error { d.Version = 5; return nil }) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
	case <-time.After(lockWait + 5*time.Second):
		t.Fatal("Update blocked on a stale lock")
	}

	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 5 {
		t.Fatalf("version = %d, want 5", got.Version)
	}
}

func TestSaveWritesIndentedJSONWithTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.json")
	if err := New[doc](path).Save(doc{Version: 1, Counts: map[string]int{"a": 2}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Error("document does not end with a newline")
	}
	var back doc
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("document is not valid JSON: %v", err)
	}
}
