package pathgrant

import (
	"os"
	"path/filepath"
	"testing"
)

// Grants are matched by string containment, so one directory must always be
// spelled one way - including when part of the path does not exist yet.
//
// This is the macOS shape: /var is a symlink to /private/var, and t.TempDir()
// hands back a /var/... path. Resolving only paths that already exist stored the
// same directory under two spellings, and grants silently stopped matching.
func TestGrantsAreConsistentUnderASymlinkedAncestor(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// Resolved, because the assertions below compare against stored Dir values -
	// and on macOS t.TempDir() is itself reached through a symlink.
	base := tempDir(t)
	real := filepath.Join(base, "private", "store")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	// link/ -> private/, so link/store and private/store are the same directory.
	if err := os.Symlink(filepath.Join(base, "private"), filepath.Join(base, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	viaLink := filepath.Join(base, "link", "store")
	project := t.TempDir()

	// Grant a subdirectory that does not exist yet, named through the symlink.
	pending := filepath.Join(viaLink, "pkg", "inner")
	if err := Add(project, pending); err != nil {
		t.Fatal(err)
	}

	// The same directory reached by its real path must be covered.
	realPending := filepath.Join(real, "pkg", "inner")
	for _, p := range []string{realPending, filepath.Join(realPending, "f.go")} {
		ok, err := Allowed(project, p)
		if err != nil || !ok {
			t.Errorf("grant made through a symlink does not cover %s: %v %v", p, ok, err)
		}
	}

	// And a grant recorded for an existing directory must collapse a later,
	// narrower one named through the link rather than sitting alongside it.
	if err := Add(project, viaLink); err != nil {
		t.Fatal(err)
	}
	grants, err := List(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected the broader grant to subsume the narrower one, got %d: %+v", len(grants), grants)
	}
	if grants[0].Dir != real {
		t.Errorf("grant stored as %q, want the resolved %q", grants[0].Dir, real)
	}
}
