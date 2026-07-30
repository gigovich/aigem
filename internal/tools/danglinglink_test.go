package tools

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A dangling symlink - one whose target does not exist yet - is the dangerous
// case, because filepath.EvalSymlinks reports ENOENT for it exactly as it does
// for a name that is simply absent. Treating it as absent lets write_file create
// the target outside the sandbox. git ships symlinks, so a clone is enough to
// plant one, and the confirmation box only ever shows the in-root name.
func TestWriteFileCannotEscapeViaDanglingSymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workdir")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name   string
		link   string
		target string
	}{
		{"absolute dangling target", "config.yaml", filepath.Join(outside, "authorized_keys")},
		{"relative dangling target", "rel.yaml", filepath.Join("..", "outside", "rel-victim")},
		{"dangling under a nested name", "nested.yaml", filepath.Join(outside, "deeper", "victim")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			link := filepath.Join(root, tc.link)
			if err := os.Symlink(tc.target, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			defer os.Remove(link)

			r, err := NewRegistry(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := run(t, r, "write_file", map[string]any{
				"path": tc.link, "content": "pwned\n",
			}); err == nil {
				t.Errorf("write_file through a dangling symlink was allowed")
			}

			victim := tc.target
			if !filepath.IsAbs(victim) {
				victim = filepath.Join(root, victim)
			}
			if _, err := os.Lstat(victim); err == nil {
				t.Errorf("escaped the sandbox and created %s", victim)
			}
		})
	}
}

// A dangling symlink that stays inside the root is legitimate: writing through
// it should create the in-root file it names.
func TestWriteFileThroughInRootDanglingSymlinkStillWorks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real", "notes.txt"), filepath.Join(root, "notes.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, r, "write_file", map[string]any{"path": "notes.txt", "content": "ok\n"}); err != nil {
		t.Fatalf("writing through an in-root dangling symlink was refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "real", "notes.txt")); err != nil {
		t.Errorf("the in-root target was not created: %v", err)
	}
}

// A cycle of dangling links must terminate rather than spin.
func TestDanglingSymlinkCycleTerminates(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "b"), filepath.Join(root, "a")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = run(t, r, "read_file", map[string]any{"path": "a"})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("resolving a symlink cycle did not terminate")
	}
}
