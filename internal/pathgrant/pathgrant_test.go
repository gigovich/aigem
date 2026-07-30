package pathgrant

import (
	"path/filepath"
	"testing"
)

func TestGrantCoversSubtreeButNotSiblings(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	project := t.TempDir()
	shared := t.TempDir()

	if ok, err := Allowed(project, filepath.Join(shared, "a.go")); err != nil || ok {
		t.Fatalf("allowed before any grant: %v %v", ok, err)
	}
	if err := Add(project, shared); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{shared, filepath.Join(shared, "a.go"), filepath.Join(shared, "deep", "b.go")} {
		ok, err := Allowed(project, p)
		if err != nil || !ok {
			t.Fatalf("granted dir does not cover %s: %v %v", p, ok, err)
		}
	}
	// A sibling whose path shares a string prefix must not be covered: the check
	// is on path elements, not on characters.
	if ok, err := Allowed(project, shared+"-old/a.go"); err != nil || ok {
		t.Fatalf("string-prefix sibling was covered: %v %v", ok, err)
	}
}

func TestGrantIsScopedToItsProject(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	a, b, shared := t.TempDir(), t.TempDir(), t.TempDir()
	if err := Add(a, shared); err != nil {
		t.Fatal(err)
	}
	if ok, _ := Allowed(a, filepath.Join(shared, "x")); !ok {
		t.Fatal("granting project lost its own grant")
	}
	if ok, _ := Allowed(b, filepath.Join(shared, "x")); ok {
		t.Fatal("another project inherited the grant")
	}
}

func TestAddCollapsesOverlappingGrants(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	project, shared := tempDir(t), tempDir(t)
	deep := filepath.Join(shared, "pkg", "inner")

	// A narrower grant added under a broader one must not create a second entry,
	// or a session that reads many files would grow the file without bound.
	if err := Add(project, shared); err != nil {
		t.Fatal(err)
	}
	if err := Add(project, deep); err != nil {
		t.Fatal(err)
	}
	got, err := List(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Dir != shared {
		t.Fatalf("grants = %+v, want just %s", got, shared)
	}

	// The reverse order collapses the other way: the broader grant replaces the
	// narrower one rather than sitting beside it.
	other := tempDir(t)
	if err := Add(project, filepath.Join(other, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := Add(project, other); err != nil {
		t.Fatal(err)
	}
	got, err = List(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("grants = %+v, want 2", got)
	}
	for _, g := range got {
		if g.Dir != shared && g.Dir != other {
			t.Fatalf("unexpected grant %s", g.Dir)
		}
	}
}

func TestForget(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	project, one, two := t.TempDir(), t.TempDir(), t.TempDir()
	if err := Add(project, one); err != nil {
		t.Fatal(err)
	}
	if err := Add(project, two); err != nil {
		t.Fatal(err)
	}
	found, err := Forget(project, one)
	if err != nil || !found {
		t.Fatalf("Forget = %v, %v", found, err)
	}
	if ok, _ := Allowed(project, filepath.Join(one, "x")); ok {
		t.Fatal("forgotten grant still allows")
	}
	if ok, _ := Allowed(project, filepath.Join(two, "x")); !ok {
		t.Fatal("Forget dropped the wrong grant")
	}
	if found, _ := Forget(project, one); found {
		t.Fatal("Forget reported a second removal")
	}
	n, err := ForgetProject(project)
	if err != nil || n != 1 {
		t.Fatalf("ForgetProject = %d, %v; want 1", n, err)
	}
	if ok, _ := Allowed(project, filepath.Join(two, "x")); ok {
		t.Fatal("ForgetProject left a grant behind")
	}
}
