// Package gitx runs the git commands the daemon needs on a person's repository, bounded in
// time and output, with git's own error text.
package gitx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Timeout bounds every git command, so a hook or a signing prompt cannot hold a caller forever.
const Timeout = 2 * time.Minute

// maxOutput bounds what a command may print; the listings read here are far smaller.
const maxOutput = 1 << 20

// Tail keeps the last n bytes written to it.
type Tail struct {
	n int
	b []byte
}

func NewTail(n int) *Tail { return &Tail{n: n} }

func (t *Tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if over := len(t.b) - t.n; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	return len(p), nil
}

func (t *Tail) String() string { return string(t.b) }

func run(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	stdout, stderr := NewTail(maxOutput), NewTail(4<<10)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %s", args[0], msg)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func lines(out string) []string {
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// MainBranch is the branch runs merge into: main, else master, else "".
func MainBranch(ctx context.Context, dir string) string {
	for _, b := range []string{"main", "master"} {
		if BranchExists(ctx, dir, b) {
			return b
		}
	}
	return ""
}

func BranchExists(ctx context.Context, dir, branch string) bool {
	_, err := run(ctx, dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// IsClean reports whether the checkout has nothing to commit, untracked files included.
func IsClean(ctx context.Context, dir string) (bool, error) {
	out, err := run(ctx, dir, "status", "--porcelain", "--untracked-files=all")
	return err == nil && out == "", err
}

func CurrentBranch(ctx context.Context, dir string) (string, error) {
	return run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// WorktreeAdd checks branch out at path, creating the branch from base unless base is "".
func WorktreeAdd(ctx context.Context, repo, path, branch, base string) error {
	args := []string{"worktree", "add", path, branch}
	if base != "" {
		args = []string{"worktree", "add", "-b", branch, path, base}
	}
	_, err := run(ctx, repo, args...)
	return err
}

// WorktreeRemove removes a worktree; without force git refuses one that has changes.
func WorktreeRemove(ctx context.Context, repo, path string, force bool) error {
	args := []string{"worktree", "remove", path}
	if force {
		args = []string{"worktree", "remove", "--force", path}
	}
	_, err := run(ctx, repo, args...)
	return err
}

func BranchDelete(ctx context.Context, repo, branch string) error {
	_, err := run(ctx, repo, "branch", "-D", branch)
	return err
}

// CommitAll commits everything in the checkout and reports whether there was anything to commit.
func CommitAll(ctx context.Context, dir, message string) (bool, error) {
	if clean, err := IsClean(ctx, dir); err != nil || clean {
		return false, err
	}
	if _, err := run(ctx, dir, "add", "-A"); err != nil {
		return false, err
	}
	_, err := run(ctx, dir, "commit", "-q", "-m", message)
	return err == nil, err
}

// Merge merges branch into the current branch with a merge commit and returns that commit.
func Merge(ctx context.Context, dir, branch string) (string, error) {
	if _, err := run(ctx, dir, "merge", "--no-ff", "--no-edit", branch); err != nil {
		return "", err
	}
	return run(ctx, dir, "rev-parse", "HEAD")
}

func MergeAbort(ctx context.Context, dir string) error {
	_, err := run(ctx, dir, "merge", "--abort")
	return err
}

func ConflictFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := run(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	return lines(out), err
}

// Branches lists the local branches under refs/heads/<under>/.
func Branches(ctx context.Context, dir, under string) ([]string, error) {
	out, err := run(ctx, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/"+under)
	return lines(out), err
}

// Worktrees maps every checked-out branch of the repository to its worktree path.
func Worktrees(ctx context.Context, repo string) (map[string]string, error) {
	out, err := run(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	paths := map[string]string{}
	var path string
	for _, line := range lines(out) {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			path = p
		}
		if b, ok := strings.CutPrefix(line, "branch refs/heads/"); ok {
			paths[b] = path
		}
	}
	return paths, nil
}

func IsMerged(ctx context.Context, dir, branch, into string) bool {
	_, err := run(ctx, dir, "merge-base", "--is-ancestor", branch, into)
	return err == nil
}

// Exclude adds pattern to the repository's info/exclude unless a line already says it.
func Exclude(ctx context.Context, repo, pattern string) error {
	dir, err := run(ctx, repo, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repo, dir)
	}
	path := filepath.Join(dir, "info", "exclude")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == pattern {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		pattern = "\n" + pattern
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(pattern + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
