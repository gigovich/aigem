package testenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultTempDirIgnoresCallerVariables(t *testing.T) {
	caller := t.TempDir()
	for _, name := range tempVars {
		t.Setenv(name, caller)
	}
	got, err := defaultTempDir()
	if err != nil {
		t.Fatal(err)
	}
	if got == caller {
		t.Fatalf("default temporary directory used caller override %q", caller)
	}
	for _, name := range tempVars {
		if value := os.Getenv(name); value != caller {
			t.Errorf("%s = %q after probe, want %q", name, value, caller)
		}
	}
}

func TestSetupSandboxIsPrivate(t *testing.T) {
	caller := t.TempDir()
	for _, name := range tempVars {
		t.Setenv(name, caller)
	}
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	sandbox, cleanup, err := setup()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "TMPDIR", "TMP", "TEMP"} {
		if got := os.Getenv(name); got != filepath.Join(sandbox, name) {
			t.Errorf("%s = %q, want private sandbox path", name, got)
		}
		dir := filepath.Join(sandbox, name)
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Errorf("%s contains unexpected files: %v", name, err)
		}
	}
	if got, err := os.Getwd(); err != nil || got != sandbox {
		t.Errorf("working directory = %q, want sandbox %q (err=%v)", got, sandbox, err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Getwd(); err != nil || got != before {
		t.Errorf("working directory after cleanup = %q, want %q (err=%v)", got, before, err)
	}
}

func TestOutermostWorktree(t *testing.T) {
	worktree := outermostWorktree(".")
	base, err := os.MkdirTemp(filepath.Dir(worktree), "testenv-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	outer := filepath.Join(base, "outer")
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(filepath.Join(inner, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outer, ".git"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, ".git"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := outermostWorktree(filepath.Join(inner, "child")); got != outer {
		t.Fatalf("outermostWorktree = %q, want %q", got, outer)
	}
	if !IsWithinWorktree(filepath.Join(inner, "child"), outer) {
		t.Fatal("nested fixture should be recognized inside worktree")
	}
	if IsWithinWorktree(filepath.Join(base, "sibling"), outer) {
		t.Fatal("sibling fixture should not be recognized inside worktree")
	}
}
