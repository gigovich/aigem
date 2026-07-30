package config

import (
	"os"
	"path/filepath"
	"testing"
)

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func rootFor(roots []SkillRoot, dir string) (SkillRoot, bool) {
	for _, r := range roots {
		if r.Dir == dir {
			return r, true
		}
	}
	return SkillRoot{}, false
}

// Whether a root is project-local is decided from its source, not from whether
// its path happens to sit under the project root.
func TestSkillRootsLabelsGlobalRootsAsGlobal(t *testing.T) {
	home := t.TempDir()
	cfg := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", cfg)

	claude := filepath.Join(home, ".claude", "skills")
	aigem := filepath.Join(cfg, "aigem", "skills")
	// A dotfiles repo rooted at $HOME makes the project root an ancestor of both.
	mkdirs(t, claude, aigem, filepath.Join(home, ".git"), filepath.Join(home, ".skills"))

	roots := SkillRoots(home)
	for _, dir := range []string{claude, aigem} {
		r, ok := rootFor(roots, dir)
		if !ok {
			t.Fatalf("%s missing from roots", dir)
		}
		if r.Project {
			t.Errorf("%s is the user's own skill dir, not the project's", dir)
		}
	}
	if r, ok := rootFor(roots, filepath.Join(home, ".skills")); !ok || !r.Project {
		t.Errorf("the repo's own .skills is project-local: %+v", r)
	}
}

// Outside a git repo the project is cwd, so the walk must not climb past it and
// pick up skill dirs belonging to no project.
func TestSkillRootsStopAtProjectRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	base := t.TempDir()
	cwd := filepath.Join(base, "sub")
	mkdirs(t, filepath.Join(base, ".skills"), filepath.Join(cwd, ".skills"))

	roots := SkillRoots(cwd)
	if _, ok := rootFor(roots, filepath.Join(base, ".skills")); ok {
		t.Error("an ancestor outside the project must not be scanned")
	}
	if r, ok := rootFor(roots, filepath.Join(cwd, ".skills")); !ok || !r.Project {
		t.Errorf("cwd's own .skills must still be scanned: %+v", r)
	}

	// With a git root above, the same ancestor is in scope again.
	mkdirs(t, filepath.Join(base, ".git"))
	if r, ok := rootFor(SkillRoots(cwd), filepath.Join(base, ".skills")); !ok || !r.Project {
		t.Errorf("inside a repo the walk must reach the repo root: %+v", r)
	}
}
