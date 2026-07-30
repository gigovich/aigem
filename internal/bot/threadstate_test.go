package bot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestThreadStoreRoundTrip(t *testing.T) {
	s := &ThreadStore{path: filepath.Join(t.TempDir(), "threads-amiran.json")}
	if got := s.Load(); got != nil {
		t.Fatalf("a missing file should load as nil, got %v", got)
	}
	ids := []string{"root1", "root2", "root3"}
	if err := s.Save(1, ids); err != nil {
		t.Fatal(err)
	}
	got := s.Load()
	if len(got) != len(ids) {
		t.Fatalf("Load = %v, want %v", got, ids)
	}
	for i := range ids {
		if got[i] != ids[i] {
			t.Fatalf("Load = %v, want %v (order matters: it is the eviction order)", got, ids)
		}
	}
	if err := s.Save(2, []string{"root9"}); err != nil {
		t.Fatal(err)
	}
	if got := s.Load(); len(got) != 1 || got[0] != "root9" {
		t.Fatalf("Save should replace the set, got %v", got)
	}
	// The set is only post ids, but the file still lives in the user's state dir.
	fi, err := os.Stat(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %v, want 0600", perm)
	}
	// No temp file may survive a successful save.
	entries, err := os.ReadDir(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("left a temp file behind: %s", e.Name())
		}
	}
}

// A snapshot that arrives after a newer one must not erase the thread the newer one recorded.
func TestThreadStoreDropsStaleVersions(t *testing.T) {
	s := &ThreadStore{path: filepath.Join(t.TempDir(), "threads-amiran.json")}
	if err := s.Save(2, []string{"r1", "r2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(1, []string{"r1"}); err != nil {
		t.Fatal(err)
	}
	if got := s.Load(); len(got) != 2 {
		t.Fatalf("a stale snapshot overwrote a newer one: %v", got)
	}
	if err := s.Save(3, []string{"r1", "r2", "r3"}); err != nil {
		t.Fatal(err)
	}
	if got := s.Load(); len(got) != 3 {
		t.Fatalf("a newer snapshot should be written: %v", got)
	}
}

func TestThreadStoreIgnoresCorruptFile(t *testing.T) {
	// A bot start must not fail over an unreadable cache; it just relearns its threads.
	path := filepath.Join(t.TempDir(), "threads-lisa.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &ThreadStore{path: path}
	if got := s.Load(); got != nil {
		t.Fatalf("corrupt file should load as nil, got %v", got)
	}
	if err := s.Save(1, []string{"r1"}); err != nil {
		t.Fatalf("Save over a corrupt file should work: %v", err)
	}
	if got := s.Load(); len(got) != 1 {
		t.Fatalf("Load after repair = %v", got)
	}
}

func TestNewThreadStorePathIsPerBot(t *testing.T) {
	// Point the state dir at a temp dir: without this the test creates directories under the
	// developer's real HOME.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	a, err := NewThreadStore("amiran")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewThreadStore("lisa")
	if err != nil {
		t.Fatal(err)
	}
	if a.path == b.path {
		t.Fatalf("two bots must not share a thread file: %s", a.path)
	}
	if filepath.Base(a.path) != "threads-amiran.json" {
		t.Fatalf("unexpected file name %q", filepath.Base(a.path))
	}
}
