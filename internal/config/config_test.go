package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func markGit(t *testing.T, dir string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestProjectInstructionsWalksToGitRoot(t *testing.T) {
	root := t.TempDir()
	markGit(t, root)
	write(t, filepath.Join(root, "AGENTS.md"), "ROOT RULES")
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	out := ProjectInstructions(sub)
	if !strings.Contains(out, "ROOT RULES") {
		t.Fatalf("expected root AGENTS.md loaded from a subdir, got:\n%s", out)
	}
}

func TestProjectInstructionsNoGitReadsCwd(t *testing.T) {
	dir := t.TempDir() // no .git anywhere up the tree
	write(t, filepath.Join(dir, "CLAUDE.md"), "CWD RULES")

	out := ProjectInstructions(dir)
	if !strings.Contains(out, "CWD RULES") {
		t.Fatalf("expected cwd CLAUDE.md loaded without a git root, got:\n%s", out)
	}
}

func TestProjectInstructionsPicksNewestOfTwo(t *testing.T) {
	root := t.TempDir()
	markGit(t, root)
	write(t, filepath.Join(root, "AGENTS.md"), "AGENTS BODY")
	write(t, filepath.Join(root, "CLAUDE.md"), "CLAUDE BODY")
	old := time.Now().Add(-time.Hour)
	now := time.Now()
	os.Chtimes(filepath.Join(root, "AGENTS.md"), old, old)
	os.Chtimes(filepath.Join(root, "CLAUDE.md"), now, now)

	out := ProjectInstructions(root)
	if !strings.Contains(out, "CLAUDE BODY") || strings.Contains(out, "AGENTS BODY") {
		t.Fatalf("expected only the newer CLAUDE.md, got:\n%s", out)
	}
}

func TestProjectInstructionsSymlinkedPairLoadedOnce(t *testing.T) {
	root := t.TempDir()
	markGit(t, root)
	write(t, filepath.Join(root, "CLAUDE.md"), "SHARED BODY")
	if err := os.Symlink("CLAUDE.md", filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}

	out := ProjectInstructions(root)
	if n := strings.Count(out, "SHARED BODY"); n != 1 {
		t.Fatalf("symlinked pair should load once, got %d:\n%s", n, out)
	}
}

func TestProjectInstructionsLoadsClaudeDir(t *testing.T) {
	root := t.TempDir()
	markGit(t, root)
	write(t, filepath.Join(root, "AGENTS.md"), "ROOT BODY")
	write(t, filepath.Join(root, ".claude", "CLAUDE.md"), "CLAUDE DIR BODY")

	out := ProjectInstructions(root)
	if !strings.Contains(out, "ROOT BODY") || !strings.Contains(out, "CLAUDE DIR BODY") {
		t.Fatalf("expected both root and .claude/CLAUDE.md, got:\n%s", out)
	}
}

func TestProjectInstructionsRootSymlinkToClaudeDirNotDoubled(t *testing.T) {
	root := t.TempDir()
	markGit(t, root)
	write(t, filepath.Join(root, ".claude", "CLAUDE.md"), "ONLY BODY")
	if err := os.Symlink(filepath.Join(".claude", "CLAUDE.md"), filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}

	out := ProjectInstructions(root)
	if n := strings.Count(out, "ONLY BODY"); n != 1 {
		t.Fatalf("root symlink to .claude/CLAUDE.md should load once, got %d:\n%s", n, out)
	}
}

func TestProjectInstructionsLoadsContextMD(t *testing.T) {
	root := t.TempDir()
	markGit(t, root)
	write(t, filepath.Join(root, "AGENTS.md"), "ROOT BODY")
	write(t, filepath.Join(root, "context.md"), "MAP BODY check the .env")

	out := ProjectInstructions(root)
	if !strings.Contains(out, "ROOT BODY") || !strings.Contains(out, "MAP BODY check the .env") {
		t.Fatalf("expected both AGENTS.md and context.md loaded, got:\n%s", out)
	}
}

func TestProjectInstructionsNoneReturnsEmpty(t *testing.T) {
	if out := ProjectInstructions(t.TempDir()); out != "" {
		t.Fatalf("expected empty result, got:\n%s", out)
	}
}

// A file the prompt could not carry must not be reported as carried: the caller
// marks these as already in the model's context, and read_file then answers
// "already included in your context in full" about text nobody was shown.
func TestInstructionPathsListsOnlyWhatWasInjected(t *testing.T) {
	root := t.TempDir()
	markGit(t, root)
	write(t, filepath.Join(root, "AGENTS.md"), "ROOT BODY")
	write(t, filepath.Join(root, "context.md"), "   \n\t\n")
	claude := filepath.Join(root, ".claude", "CLAUDE.md")
	write(t, claude, "UNREADABLE BODY")
	if err := os.Chmod(claude, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(claude, 0o644) })

	out := ProjectInstructions(root)
	if !strings.Contains(out, "ROOT BODY") {
		t.Fatalf("expected the readable file in the prompt, got:\n%s", out)
	}
	if strings.Contains(out, "UNREADABLE BODY") {
		t.Fatalf("an unreadable file reached the prompt:\n%s", out)
	}

	paths := InstructionPaths(root)
	if len(paths) != 1 || filepath.Base(paths[0]) != "AGENTS.md" {
		t.Fatalf("InstructionPaths = %v, want only the file that was injected", paths)
	}
}
