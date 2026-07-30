package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// A symlink inside the sandbox must never let a file tool write outside it. A
// cloned repository can ship such a link, so this is reachable without any
// cooperation from the user.
//
// The interesting case is a path with several components that do not exist yet:
// resolving only the leaf and its immediate parent leaves the link unresolved,
// after which a purely lexical containment check says "inside" and write_file
// happily creates the missing directories through the link.
func TestWriteFileCannotEscapeViaSymlinkedDirectory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workdir")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	r, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}

	// Increasing numbers of not-yet-existing components under the symlink.
	for _, target := range []string{
		"link/f.txt",
		"link/newdir/f.txt",
		"link/a/b/f.txt",
		"link/a/b/c/d/f.txt",
	} {
		t.Run(target, func(t *testing.T) {
			_, err := run(t, r, "write_file", map[string]any{
				"path": target, "content": "escaped\n",
			})
			if err == nil {
				t.Errorf("write_file %q was allowed; it must be refused as outside the sandbox", target)
			}
			leaked := filepath.Join(outside, filepath.FromSlash(target[len("link/"):]))
			if _, statErr := os.Lstat(leaked); statErr == nil {
				t.Errorf("write_file %q escaped the sandbox and created %s", target, leaked)
			}
		})
	}
}

// Reads must not escape either.
func TestReadFileCannotEscapeViaSymlinkedDirectory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workdir")
	outside := filepath.Join(base, "outside", "nested")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	r, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	out, err := run(t, r, "read_file", map[string]any{"path": "link/nested/secret.txt"})
	if err == nil {
		t.Errorf("read_file escaped the sandbox and returned: %q", out)
	}
}
