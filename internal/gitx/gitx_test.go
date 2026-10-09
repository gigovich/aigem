package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func repo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", branch)
	git(t, dir, "config", "user.name", "Test")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "commit.gpgsign", "false")
	write(t, dir, "README.md", "hello\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "first")
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, text string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func resolved(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTailKeepsTheEnd(t *testing.T) {
	tail := NewTail(4)
	for _, s := range []string{"ab", "cdef", "g"} {
		if _, err := tail.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if tail.String() != "defg" {
		t.Errorf("tail = %q, want defg", tail.String())
	}
}

func TestMainBranchIsMainOrMasterOrNothing(t *testing.T) {
	ctx := context.Background()
	if got := MainBranch(ctx, repo(t, "main")); got != "main" {
		t.Errorf("main repo = %q", got)
	}
	if got := MainBranch(ctx, repo(t, "master")); got != "master" {
		t.Errorf("master repo = %q", got)
	}
	if got := MainBranch(ctx, repo(t, "trunk")); got != "" {
		t.Errorf("trunk repo = %q, want empty", got)
	}
}

func TestIsCleanSeesUntrackedFilesButNotExcludedOnes(t *testing.T) {
	ctx := context.Background()
	r := repo(t, "main")
	if clean, err := IsClean(ctx, r); err != nil || !clean {
		t.Fatalf("fresh repo clean = %v, %v", clean, err)
	}
	write(t, r, ".aigem/worktrees/TCK-1/file.txt", "x")
	if clean, _ := IsClean(ctx, r); clean {
		t.Error("an untracked file did not make the checkout dirty")
	}
	for range 2 {
		if err := Exclude(ctx, r, "/.aigem/worktrees/"); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(r, ".git", "info", "exclude"))
	if err != nil || strings.Count(string(data), "/.aigem/worktrees/\n") != 1 {
		t.Fatalf("info/exclude = %q, %v, want the pattern once", data, err)
	}
	if clean, err := IsClean(ctx, r); err != nil || !clean {
		t.Errorf("with the worktrees excluded = %v, %v, want clean", clean, err)
	}
	write(t, r, "README.md", "changed\n")
	if clean, _ := IsClean(ctx, r); clean {
		t.Error("a modified tracked file did not make the checkout dirty")
	}
}

func TestAWorktreeIsCommittedMergedRemovedAndReused(t *testing.T) {
	ctx := context.Background()
	r := repo(t, "main")
	if err := Exclude(ctx, r, "/.aigem/worktrees/"); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(r, ".aigem", "worktrees", "TCK-1")
	if err := WorktreeAdd(ctx, r, wt, "aigem/TCK-1", "main"); err != nil {
		t.Fatal(err)
	}
	paths, err := Worktrees(ctx, r)
	if err != nil || resolved(t, paths["aigem/TCK-1"]) != resolved(t, wt) {
		t.Fatalf("worktrees = %v, %v", paths, err)
	}
	if clean, _ := IsClean(ctx, r); !clean {
		t.Error("a nested worktree made the main checkout dirty")
	}
	if !BranchExists(ctx, r, "aigem/TCK-1") || BranchExists(ctx, r, "aigem/TCK-2") {
		t.Error("BranchExists is wrong")
	}

	write(t, wt, "notes.txt", "notes\n")
	if made, err := CommitAll(ctx, wt, "aigem: notes (TCK-1, RUN-1)"); err != nil || !made {
		t.Fatalf("CommitAll = %v, %v", made, err)
	}
	if made, err := CommitAll(ctx, wt, "again"); err != nil || made {
		t.Errorf("CommitAll with nothing to commit = %v, %v", made, err)
	}
	if got, _ := Branches(ctx, r, "aigem"); !slices.Equal(got, []string{"aigem/TCK-1"}) {
		t.Errorf("branches = %v", got)
	}
	if IsMerged(ctx, r, "aigem/TCK-1", "main") {
		t.Error("an unmerged branch reads as merged")
	}
	if cur, err := CurrentBranch(ctx, r); err != nil || cur != "main" {
		t.Errorf("current = %q, %v", cur, err)
	}

	sha, err := Merge(ctx, r, "aigem/TCK-1")
	if err != nil || sha != git(t, r, "rev-parse", "HEAD") {
		t.Fatalf("Merge = %q, %v", sha, err)
	}
	if parents := strings.Fields(git(t, r, "log", "-1", "--format=%P")); len(parents) != 2 {
		t.Errorf("parents = %v, want a merge commit", parents)
	}
	if !IsMerged(ctx, r, "aigem/TCK-1", "main") {
		t.Error("a merged branch reads as unmerged")
	}

	write(t, wt, "stray.txt", "x")
	if err := WorktreeRemove(ctx, r, wt, false); err == nil {
		t.Error("a worktree with changes was removed without force")
	}
	if err := WorktreeRemove(ctx, r, wt, true); err != nil {
		t.Fatal(err)
	}
	if err := WorktreeAdd(ctx, r, wt, "aigem/TCK-1", ""); err != nil {
		t.Fatalf("checking the kept branch out again: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "notes.txt")); err != nil {
		t.Error("the reused branch lost its commit")
	}
	if err := WorktreeRemove(ctx, r, wt, false); err != nil {
		t.Fatal(err)
	}
	if err := BranchDelete(ctx, r, "aigem/TCK-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := Branches(ctx, r, "aigem"); err != nil || len(got) != 0 {
		t.Errorf("branches after delete = %v, %v", got, err)
	}
}

func TestAConflictListsItsFilesAndAborts(t *testing.T) {
	ctx := context.Background()
	r := repo(t, "main")
	wt := filepath.Join(t.TempDir(), "wt")
	if err := WorktreeAdd(ctx, r, wt, "aigem/TCK-1", "main"); err != nil {
		t.Fatal(err)
	}
	write(t, wt, "README.md", "theirs\n")
	if _, err := CommitAll(ctx, wt, "theirs"); err != nil {
		t.Fatal(err)
	}
	write(t, r, "README.md", "ours\n")
	git(t, r, "commit", "-qam", "ours")

	if _, err := Merge(ctx, r, "aigem/TCK-1"); err == nil {
		t.Fatal("a conflicting merge succeeded")
	}
	if files, err := ConflictFiles(ctx, r); err != nil || !slices.Equal(files, []string{"README.md"}) {
		t.Fatalf("conflicts = %v, %v", files, err)
	}
	if err := MergeAbort(ctx, r); err != nil {
		t.Fatal(err)
	}
	if clean, _ := IsClean(ctx, r); !clean {
		t.Error("the checkout is dirty after the abort")
	}
}

func TestAnErrorCarriesGitsOwnText(t *testing.T) {
	err := BranchDelete(context.Background(), repo(t, "main"), "nope")
	if err == nil || !strings.HasPrefix(err.Error(), "git branch: ") || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v", err)
	}
}
