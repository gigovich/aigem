# Agent Tickets Part 2 Implementation Plan: Runs on Tickets

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** "Run" on a runnable ticket gives it a git worktree and an autonomous run inside it;
when the agent calls `ticket_done` the daemon commits, runs the repository's check and merges
into `main`; anything that goes wrong leaves the ticket `blocked` with a reason a person can
act on.

**Architecture:** A new `internal/gitx` package runs git. `runner.Runs` learns to open an
autonomous run tied to a ticket (record fields, extra tools, an `OnTurn` hook fed from the
session's own events, and `Stop`). `runner.Tickets` gets two daemon methods, `Start` and
`Finish`. A new coordinator, `runner.TicketRuns`, owns everything else: start, the decision at
each turn end, commit, check, merge, stop, retry merge, restart recovery, the worktree list and
discard. `cmd/aigem` adapts it to new routes on the existing `TicketsBackend` and
`RunsBackend` seams; the React UI adds the buttons, the Runs tab and the Worktrees list.

**Tech Stack:** Go 1.26 (`os/exec`, `net/http` ServeMux patterns), git >= 2.28, React 19 +
TypeScript, Tailwind v4, vitest + Testing Library.

**Spec:** `docs/superpowers/specs/2026-10-09-agent-tickets-part2-runs-design.md` (part 2;
part 1 context: `docs/superpowers/specs/2026-10-09-agent-tickets-design.md`)

## Global Constraints

- Work directly on `main` in `/Users/gigovich/work/gigovich/aigem`; commit after every task.
  Nothing pushes - not the daemon, not this plan.
- Worktree path: `<project dir>/.aigem/worktrees/<TCK-n>`; branch: `aigem/<TCK-n>`, created
  from the repository's `main` (or `master`). The ticket's repository is
  `<project dir>/<repo>`, the project directory itself when `repo` is empty.
- Commit message in the worktree: `aigem: <title> (TCK-n, RUN-m)`.
- Check config: `.aigem/project.json` in the repository, `{"check": "make test"}`, run through
  `sh -c` in the worktree with a 15 minute budget; a failure comment carries the last 4 KiB of
  output.
- Merge only in the repository's own checkout, only when it is clean and on `main`, with
  `git merge --no-ff`; one merge per repository at a time (a mutex per repository). A conflict
  runs `git merge --abort`.
- Blocked reasons, exactly: `the main checkout has uncommitted changes`,
  `the main checkout is on <branch>, not main` (`main` is the repository's main branch name),
  `stopped by a person`, `the daemon restarted`, `the run was deleted`,
  `the turn ended with an error: ...`, `interrupted`; a turn without `ticket_done` is blocked
  with the agent's last message.
- One run per ticket; a `blocked` ticket keeps its run alive; a further turn on it moves the
  ticket back to `running`.
- Minimal code, few and concise comments, follow the idioms already in the file you touch.
- Go lines <= 120 characters; Markdown lines <= 100 characters; no em dashes anywhere, use `-`.
- UI sizes in rem / Tailwind spacing only (no new px); no `cursor-*` classes (global rule);
  icon-only buttons need `aria-label` and `title`; every control is keyboard-operable. Do not
  run prettier.
- Go tests that touch state set `XDG_STATE_HOME` to `t.TempDir()`. Git tests use real
  repositories in `t.TempDir()` with `user.name`, `user.email` and `commit.gpgsign=false` set
  locally.
- Commands: `gofmt -w <touched files>`; `go test -race ./internal/... ./cmd/aigem/...`;
  lint `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./...`
  (local v1 cannot read the repo; the old QF1003 in `internal/tui/model_add.go` is not ours);
  UI in `internal/web/_ui`: `npm run lint`, `npm run check`,
  `NODE_OPTIONS=--no-experimental-webstorage npx vitest run`.
- Known failures on macOS, also on `main`, not ours:
  `TestLoadRefusesAnUnresolvableWorkingDirectory`,
  `TestLoadErrorNamesTheDirectoryAndKeepsTheCause` (runner), `TestSetupSandboxIsPrivate`
  (testenv). Flaky: `TestSpecEvictionSettingsReachTheAgent` (runner),
  `TestSkillsBrowserAndDispatch` (tui).

### Decisions and deviations

- Deviation from spec: the first-message rule reads "When the work is complete, call
  ticket_done ..." without "and committed". An autonomous session gets the `workspace-write`
  profile, which has no shell, so the agent cannot commit; the daemon commits for it.
- Deviation from spec: `Ticket` gains `mergePending` and `Tickets.Finish` takes a
  `mergePending bool`. The UI needs to know a ticket is "blocked on a merge" to offer "Retry
  merge", and the retry route needs to know the branch was committed and checked, also after a
  restart. Nothing else would carry that.
- Deviation from spec: a successful "Retry merge" comments only "Merged aigem/TCK-n into main
  as <sha>." The agent's summary is not kept anywhere after a restart, so it is written into the
  merge-blocked comment instead ("The agent's summary: ...").
- Deviation from spec: the wrong-branch reason names the repository's main branch, so a
  `master` repository reads "the main checkout is on <branch>, not master".
- Deviation from spec: "Run" on a ticket whose worktree and branch already exist (for example
  blocked after a daemon restart) reuses them and opens a new run there instead of refusing; a
  kept branch without a worktree is checked out again. Only Discard deletes work.
- Deviation from spec: two refusals the spec does not list: `<path> is in the way of the
  worktree for <id>; remove it` (a directory at the worktree path without the branch) and
  `"<repo>" is not a repository of this project` (a ticket repo that is not one plain name).
- Deviation from spec: a turn that called `ticket_done` but then ended interrupted or with an
  error counts as not done: the ticket is blocked with "interrupted" / "the turn ended with an
  error: ...". The work may be half-written, and a person decides.
- Deviation from spec: `ticket_done` is also reachable from subagents and forked skills, which
  build their tools from the run's registry. Accepted: it only records a summary, the daemon
  still acts only after the run's own turn ends normally, and only for the ticket the run drives.
- Deviation from spec: at Start the daemon adds `/.aigem/worktrees/` to the repository's
  `<git-common-dir>/info/exclude` (once), so the nested worktree never shows in the person's
  `git status` and is never caught by their `git add -A`.
- Deviation from spec: a `done` ticket's worktree is removed without `--force`. If a person's
  turn left changes in it, it is kept and the done comment says where; only Discard forces.
- Decision: the check command is read from `.aigem/project.json` in the repository's main
  checkout, so the agent cannot rewrite which command runs. The check still runs code the agent
  wrote in the worktree (tests, build scripts), with the daemon's rights and without a sandbox.
  The agent's tools are rooted at the worktree; that is not a guarantee that nothing it wrote
  runs outside it.
- Decision: a merge that has begun is never cancelled - not by Stop, not by the request that
  asked for a retry, not by shutdown - because a merge killed half-way leaves the person's
  checkout mid-merge. Every git command has its own 2 minute bound (`gitx.Timeout`).
- Decision: commit, check and merge happen only while the ticket is `running` and its last run
  is the run whose turn ended. A turn whose `turn_start` could not take the ticket back (a
  person closed it meanwhile) is ignored at its end.
- Decision: when a ticket is `done` its run is stopped (the worktree it worked in is gone).
- Decision: Stop and delete block the ticket first, then end the session, so a late turn end of
  the stopped turn never writes a second comment.
- Decision: the ticket page offers "Stop" whenever the ticket's last run is live, also while it
  is blocked, so a person can give back a run slot (ticket runs share `maxLiveRuns` with chats).
- Decision: `TicketRuns` keeps a small "starting" set: git does not refuse a second start that
  reuses a worktree, so two clicks would otherwise open two runs in one worktree.
- Decision: `gitx` has a few helpers beyond the spec list (`BranchExists`, `Branches`,
  `IsMerged`, `Exclude`, `Tail`); the worktree list carries `repo`; Discard of an unknown name
  is 404.

## Review Focus

- The main checkout cannot take the merge - uncommitted or untracked files, another branch
  checked out, or a conflict: the ticket is blocked with the exact sentence, `main` and the
  checkout are left untouched (no `MERGE_HEAD`), "Retry merge" works once fixed. A Stop during a
  slow merge never leaves the checkout mid-merge.
  (Task 6 `TestTheMainCheckoutMustBeCleanAndOnMain`, `TestAConflictIsAbortedAndNamesTheFiles`;
  Task 7 `TestRetryMergeWaitsForACleanCheckout`, `TestStoppingDuringAMergeLeavesMainConsistent`.)
- A second turn after `blocked`: the person types into the live run, the ticket goes back to
  `running`, and the next turn end is evaluated again - but not when the person closed the
  ticket meanwhile. (Task 6 `TestATurnWithoutTicketDoneBlocksAndTheNextTurnIsEvaluatedAgain`,
  `TestTicketDoneOnATicketAPersonClosedChangesNothing`.)
- Daemon restart mid-run: the ticket becomes `blocked` with "the daemon restarted", its
  worktree stays, "Run" opens a new run in the same worktree with the earlier commits; shutdown
  kills a check in flight and leaves the ticket for recovery. (Task 6
  `TestARestartBlocksTheTicketAndRunReusesItsWorktree`; Task 7
  `TestCloseKillsADeliveryInFlightAndLeavesTheTicketForRecovery`.)
- A check that hangs: it is killed with its whole process group at the budget and the ticket is
  blocked; a person who stops the run during the check gets one comment, not two. (Task 5
  `TestACheckThatHangsIsStoppedAtTheBudget`; Task 7
  `TestStoppingDuringTheCheckKillsItAndLeavesOneComment`.)
- Stopping or deleting the driving run mid-turn: the ticket is blocked with "stopped by a
  person" / "the run was deleted" exactly once, the worktree stays, a late turn end writes
  nothing. (Task 7 `TestStoppingTheDrivingRunBlocksTheTicketOnce`,
  `TestDeletingTheDrivingRunBlocksTheTicket`.)

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/gitx/gitx.go` (new) | bounded git commands with git's own error text; `Tail` |
| `internal/gitx/gitx_test.go` (new) | against real temporary repositories |
| `internal/runner/projects.go` | `Repositories` uses `gitx.MainBranch` |
| `internal/runner/env.go` | `NewToolsAt(dir)`; `NewTools` calls it |
| `internal/runner/ticketdone.go` (new) | the `ticket_done` tool |
| `internal/runner/runs.go` | ticket fields, `RunRequest.Root`, extra tools, `OnTurn`, `Stop` |
| `internal/runner/export_test.go` | `CloseRun` calls `Stop` |
| `internal/runner/runs_ticket_test.go` (new) | scripted model, ticket run and stop tests |
| `internal/runner/ticket_rules.go`, `tickets.go` | `MergePending`; `Tickets.Start`, `Tickets.Finish` |
| `internal/tools/procgroup_*.go`, `impl.go` | export `ConfigureProcessGroup` |
| `internal/runner/ticketcheck.go` (new) | `readCheck`, `runCheck`, the budget |
| `internal/runner/ticketruns.go` (new) | `TicketRuns` coordinator |
| `internal/runner/ticketruns_test.go` (new) | coordinator tests with a scripted model and real git |
| `cmd/aigem/webruns.go` | `openRun` roots the tools at `req.Root(env.Cwd)` |
| `internal/web/api_tickets.go`, `api_runs.go`, `backend.go`, `server.go` | routes, wire types |
| `cmd/aigem/webtickets.go`, `webbackend.go`, `webcmd.go` | adapter, wiring, activity, Close |
| `internal/web/_ui/src/lib/wire.ts`, `api.ts` | types and calls |
| `internal/web/_ui/src/screens/Task.tsx`, `Run.tsx`, `Worktrees.tsx` | buttons, Runs tab, list |
| `internal/web/_ui/src/test/harness.tsx` | default stub for the worktrees route |
| `docs/web.md`, `CHANGELOG.md` | docs |

---

### Task 1: `internal/gitx`

**Files:**
- Create: `internal/gitx/gitx.go`
- Test: `internal/gitx/gitx_test.go`
- Modify: `internal/runner/projects.go` (drop `mainBranch`, use `gitx.MainBranch`)

**Interfaces:**
- Produces:
  ```go
  const Timeout = 2 * time.Minute         // bound of every git command
  type Tail struct{ ... }                 // keeps the last n bytes written
  func NewTail(n int) *Tail
  func (t *Tail) Write(p []byte) (int, error)
  func (t *Tail) String() string
  func MainBranch(ctx context.Context, dir string) string                 // "main", "master" or ""
  func BranchExists(ctx context.Context, dir, branch string) bool
  func IsClean(ctx context.Context, dir string) (bool, error)             // untracked files count
  func CurrentBranch(ctx context.Context, dir string) (string, error)
  func WorktreeAdd(ctx context.Context, repo, path, branch, base string) error // base "" = existing branch
  func WorktreeRemove(ctx context.Context, repo, path string, force bool) error
  func BranchDelete(ctx context.Context, repo, branch string) error
  func CommitAll(ctx context.Context, dir, message string) (bool, error)
  func Merge(ctx context.Context, dir, branch string) (string, error)    // merge commit sha
  func MergeAbort(ctx context.Context, dir string) error
  func ConflictFiles(ctx context.Context, dir string) ([]string, error)
  func Branches(ctx context.Context, dir, under string) ([]string, error) // refs/heads/<under>/*
  func Worktrees(ctx context.Context, repo string) (map[string]string, error) // branch -> path
  func IsMerged(ctx context.Context, dir, branch, into string) bool
  func Exclude(ctx context.Context, repo, pattern string) error          // info/exclude, once
  ```
  Errors read `git <subcommand>: <git's stderr, trimmed>`. Every command is bounded by
  `Timeout` and `WaitDelay`, so a hook or a signing prompt cannot hold a caller; stdout is kept
  to 1 MiB and stderr to 4 KiB.

- [ ] **Step 1: Write the failing tests**

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/gitx/ 2>&1 | head`
Expected: FAIL, `undefined: NewTail` (and the other names).

- [ ] **Step 3: Write the implementation**

`internal/gitx/gitx.go`:

```go
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
```

In `internal/runner/projects.go`: replace the two calls `mainBranch(ctx, v.Dir)` and
`mainBranch(ctx, dir)` with `gitx.MainBranch(ctx, v.Dir)` and `gitx.MainBranch(ctx, dir)`,
delete the whole `mainBranch` function with its comment, remove the `"os/exec"` import and add
`"github.com/gigovich/aigem/internal/gitx"` next to the `store` import.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/gitx/ ./internal/runner/ -run 'Tail|MainBranch|IsClean|Worktree|Conflict|GitsOwn|Repositor' -v 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
gofmt -w internal/gitx internal/runner/projects.go
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./internal/gitx/... ./internal/runner/...
git add internal/gitx internal/runner/projects.go
git commit -m "feat(gitx): git commands for ticket worktrees, commits and merges"
```

---

### Task 2: `Env.NewToolsAt` and the `ticket_done` tool

**Files:**
- Modify: `internal/runner/env.go` (`NewTools` -> `NewToolsAt(dir)` plus a one-line `NewTools`)
- Create: `internal/runner/ticketdone.go`
- Test: `internal/runner/env_test.go` (append), `internal/runner/ticketdone_test.go` (new)

**Interfaces:**
- Produces: `func (e *Env) NewToolsAt(dir string) (*tools.Registry, error)`;
  unexported `newTicketDone(record func(summary string)) tools.Tool` named `ticket_done`,
  `NeedsConfirm() == false`, schema `{"summary": string}` required.

- [ ] **Step 1: Write the failing tests**

Append to `internal/runner/env_test.go`:

```go
func TestNewToolsAtRootsTheSandboxAtAnotherDirectory(t *testing.T) {
	env, _ := load(t, runner.Options{Cwd: project(t)})
	dir := t.TempDir()
	reg, err := env.NewToolsAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(dir); reg.Root() != want {
		t.Errorf("root = %q, want %q", reg.Root(), want)
	}
	env.Close()
	if _, err := env.NewToolsAt(dir); err == nil {
		t.Error("a closed environment handed out a registry")
	}
}
```

`internal/runner/ticketdone_test.go`:

```go
package runner

import (
	"context"
	"encoding/json"
	"testing"
)

func TestTicketDoneRecordsATrimmedSummaryAndNeedsOne(t *testing.T) {
	var got string
	tool := newTicketDone(func(s string) { got = s })
	if tool.Name() != "ticket_done" || tool.NeedsConfirm() {
		t.Fatalf("name = %q, confirm = %v", tool.Name(), tool.NeedsConfirm())
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"summary":"  "}`)); err == nil || got != "" {
		t.Errorf("an empty summary = %v, recorded %q", err, got)
	}
	out, err := tool.Run(context.Background(), json.RawMessage(`{"summary":" Added the route. "}`))
	if err != nil || out == "" || got != "Added the route." {
		t.Errorf("run = %q, %v, recorded %q", out, err, got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/runner/ -run 'NewToolsAt|TicketDone' 2>&1 | head`
Expected: FAIL, `env.NewToolsAt undefined` and `undefined: newTicketDone`.

- [ ] **Step 3: Write the implementation**

In `internal/runner/env.go` replace the whole `NewTools` function (its doc comment stays above
`NewToolsAt`) with:

```go
// NewTools builds the sandbox for one conversation, rooted at the environment's directory.
func (e *Env) NewTools() (*tools.Registry, error) { return e.NewToolsAt(e.Cwd) }

// NewToolsAt builds the sandbox for one conversation, rooted at dir.
//
// It is a constructor rather than a field because a registry is not shareable
// between sessions: the delegation and skill tools are registered into it bound
// to that session's confirmation function, so two conversations sharing one
// would have tool calls in the first asking the second's clients for approval.
//
// Persisted path grants are enabled by the session, not here.
func (e *Env) NewToolsAt(dir string) (*tools.Registry, error) {
	if e.closed.Load() {
		return nil, errors.New("runner: the environment is closed; its MCP servers are gone")
	}
	r, err := tools.NewRegistry(dir)
	if err != nil {
		return nil, err
	}
	if t := search.NewTool(e.Search); t != nil {
		r.Register(t)
	}
	if t := search.NewBrowseTool(e.Search); t != nil {
		r.Register(t)
	}
	if t := search.NewBrowserActionTool(e.Search); t != nil {
		r.Register(t)
	}
	// Every registry gets them, not just the first: MCP tools are adapters bound
	// to the server connection, which the manager owns, so a second session
	// registering them again shares the connection rather than opening one.
	if !e.MCP.Empty() {
		e.MCP.RegisterTools(r)
	}
	return r, nil
}
```

`internal/runner/ticketdone.go`:

```go
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gigovich/aigem/internal/tools"
)

// ticketDone is the tool a ticket run calls when its work is complete. It only records the
// summary; the daemon commits, checks and merges once the turn ends.
type ticketDone struct{ record func(summary string) }

func newTicketDone(record func(string)) tools.Tool { return &ticketDone{record: record} }

func (*ticketDone) Name() string       { return "ticket_done" }
func (*ticketDone) NeedsConfirm() bool { return false }

func (*ticketDone) Description() string {
	return "Call this once the ticket's work is complete, with a short summary of what changed. " +
		"When your turn ends the daemon commits the worktree, runs the repository's check and " +
		"merges into main. Do not call it when you are stuck or need a decision."
}

func (*ticketDone) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string",` +
		`"description":"What was done, in a few sentences."}},"required":["summary"]}`)
}

func (d *ticketDone) Run(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		return "", errors.New("a summary is required")
	}
	d.record(summary)
	return "Recorded. End your turn now; the daemon commits, checks and merges.", nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/runner/ -run 'NewToolsAt|TicketDone|Session' -v 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
gofmt -w internal/runner/env.go internal/runner/env_test.go internal/runner/ticketdone.go internal/runner/ticketdone_test.go
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./internal/runner/...
git add internal/runner/env.go internal/runner/env_test.go internal/runner/ticketdone.go internal/runner/ticketdone_test.go
git commit -m "feat(runner): NewToolsAt and the ticket_done tool"
```

---

### Task 3: `Runs` opens autonomous ticket runs, reports turns, and stops

**Files:**
- Modify: `internal/runner/runs.go`
- Modify: `cmd/aigem/webruns.go` (`openRun` roots the tools and hooks at `req.Root(env.Cwd)`)
- Modify: `internal/runner/export_test.go` (`CloseRun` calls `Stop`)
- Test: `internal/runner/runs_ticket_test.go` (new, `package runner`)

**Interfaces:**
- Produces:
  ```go
  type Run struct { ...; TicketID, Worktree, Branch string } // json ticketId, worktree, branch
  type RunRequest struct {
      ...
      TicketID, Worktree, Branch string
      Tools  []tools.Tool          // registered after the mode's subset is taken
      OnTurn func(uisession.Event) // every turn_start and turn_end, in order, own goroutine
  }
  func (req RunRequest) Root(dir string) string // the worktree when set, else dir
  func (r *Runs) Stop(id string) error // ErrNoRun, ErrRunClosed, ErrRunsClosed
  ```
  A replay that fails while no turn is running (the ring and the journal lost the events)
  reaches `OnTurn` as a synthetic `turn_end` carrying the error, so a ticket is never left
  `running` by a lost event.
  `Create` accepts `ModeAutonomous` only with `TicketID` and `Worktree`; otherwise
  `ErrRunMode` as before.
- Test helpers for later tasks (in `runs_ticket_test.go`): `type reply`, `scripted` (an
  `llm.Backend` answering from a queue; `then(...reply)`), `say(text)`, `call(name, args)`,
  `scriptedOpen(s, dir) OpenRun`, `nextTurn`.

- [ ] **Step 1: Write the failing tests**

`internal/runner/runs_ticket_test.go`:

```go
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/tools"
	"github.com/gigovich/aigem/internal/uisession"
)

// reply is one scripted model answer. It gets the turn's context, so a reply can wait for an
// interrupt the way a slow model would.
type reply func(context.Context) (llm.Message, error)

// scripted is a model that answers from a queue, and says "nothing more" once it is empty.
type scripted struct {
	mu      sync.Mutex
	replies []reply
}

func (s *scripted) then(r ...reply) {
	s.mu.Lock()
	s.replies = append(s.replies, r...)
	s.mu.Unlock()
}

func (s *scripted) Stream(ctx context.Context, _ []llm.Message, _ []llm.Tool, _ float64,
	_ func(llm.StreamEvent)) (llm.Message, error) {
	s.mu.Lock()
	next := say("nothing more")
	if len(s.replies) > 0 {
		next, s.replies = s.replies[0], s.replies[1:]
	}
	s.mu.Unlock()
	return next(ctx)
}

func (*scripted) Tokenize(_ context.Context, text string) (int, error) { return len(text) / 4, nil }
func (*scripted) Endpoint() string                                    { return "test" }

func (*scripted) Model() llm.ModelInfo { return llm.ModelInfo{Provider: "test", ID: "model"} }

func say(text string) reply {
	return func(context.Context) (llm.Message, error) {
		return llm.Message{Role: llm.RoleAssistant, Content: text}, nil
	}
}

var callSeq atomic.Int64

func call(name, args string) reply {
	return func(context.Context) (llm.Message, error) {
		return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: fmt.Sprintf("call-%d", callSeq.Add(1)), Type: "function",
			Function: llm.FunctionCall{Name: name, Arguments: args},
		}}}, nil
	}
}

// scriptedOpen builds real sessions against s, rooted the way the daemon roots them.
func scriptedOpen(s *scripted, dir string) OpenRun {
	return func(_ context.Context, req RunRequest) (*Session, Opened, error) {
		root := req.Root(dir)
		reg, err := tools.NewRegistry(root)
		if err != nil {
			return nil, Opened{}, err
		}
		sess := NewSession(Spec{Mode: req.Mode, Tools: reg, Backend: llm.NewRef(s), Title: req.Title})
		return sess, Opened{Model: "test/model", Root: root}, nil
	}
}

func nextTurn(t *testing.T, turns <-chan uisession.Event) uisession.Event {
	t.Helper()
	select {
	case ev := <-turns:
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("no turn event arrived")
		return uisession.Event{}
	}
}

type probe struct{ ran atomic.Bool }

func (*probe) Name() string            { return "probe" }
func (*probe) Description() string     { return "a test probe" }
func (*probe) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (*probe) NeedsConfirm() bool      { return false }
func (p *probe) Run(context.Context, json.RawMessage) (string, error) {
	p.ran.Store(true)
	return "ok", nil
}

func TestATicketRunIsAutonomousRootedAtItsWorktreeAndReportsItsTurns(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := &scripted{}
	s.then(call("probe", `{}`), say("finished"))
	runs, err := NewRuns(RunsConfig{Open: scriptedOpen(s, t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)

	wt := t.TempDir()
	p := &probe{}
	turns := make(chan uisession.Event, 8)
	v, err := runs.Create(context.Background(), RunRequest{
		Mode: ModeAutonomous, TicketID: "TCK-1", Worktree: wt, Branch: "aigem/TCK-1",
		Tools: []tools.Tool{p}, OnTurn: func(ev uisession.Event) { turns <- ev },
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Mode != ModeAutonomous || v.TicketID != "TCK-1" || v.Worktree != wt || v.Branch != "aigem/TCK-1" ||
		v.Root != wt {
		t.Fatalf("run = %+v", v)
	}
	if err := runs.Apply(v.ID, RunOp{Op: OpSubmit, Text: "go"}); err != nil {
		t.Fatal(err)
	}
	if ev := nextTurn(t, turns); ev.Kind != uisession.KindTurnStart {
		t.Fatalf("first event = %s, want turn_start", ev.Kind)
	}
	if ev := nextTurn(t, turns); ev.Kind != uisession.KindTurnEnd || ev.Text != "finished" {
		t.Fatalf("second event = %s %q, want turn_end with the last message", ev.Kind, ev.Text)
	}
	if !p.ran.Load() {
		t.Error("the extra tool was not offered past the autonomous tool subset")
	}
}

func TestARunIsRootedAtItsWorktreeWhenItHasOne(t *testing.T) {
	if got := (RunRequest{}).Root("/p"); got != "/p" {
		t.Errorf("a plain run = %q, want the project", got)
	}
	wt := "/p/.aigem/worktrees/TCK-1"
	if got := (RunRequest{Worktree: wt}).Root("/p"); got != wt {
		t.Errorf("a ticket run = %q, want its worktree", got)
	}
}

func TestALostReplayStandsInForTheTurnEnd(t *testing.T) {
	evs := []uisession.Event{
		{Seq: 1, Kind: uisession.KindTurnStart}, {Seq: 2, Kind: uisession.KindContent},
		{Seq: 3, Kind: uisession.KindTurnEnd, Text: "bye"},
	}
	if got := turnEvents(evs, nil, false); len(got) != 2 || got[1].Text != "bye" {
		t.Errorf("turn events = %+v", got)
	}
	if got := turnEvents(nil, uisession.ErrTruncated, true); len(got) != 0 {
		t.Errorf("a lost replay mid-turn = %+v, want nothing until the turn ends", got)
	}
	got := turnEvents(nil, uisession.ErrTruncated, false)
	if len(got) != 1 || got[0].Kind != uisession.KindTurnEnd ||
		!strings.Contains(got[0].Error, "could not be read back") {
		t.Errorf("a lost replay after the turn = %+v, want a turn_end with the error", got)
	}
}

func TestAnAutonomousRunNeedsATicketAndAWorktree(t *testing.T) {
	runs, err := NewRuns(RunsConfig{Open: scriptedOpen(&scripted{}, t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)
	for _, req := range []RunRequest{
		{Mode: ModeAutonomous, TicketID: "TCK-1"},
		{Mode: ModeAutonomous, Worktree: t.TempDir()},
	} {
		if _, err := runs.Create(context.Background(), req); !errors.Is(err, ErrRunMode) {
			t.Errorf("Create(%+v) = %v, want ErrRunMode", req, err)
		}
	}
}

func TestStopEndsTheSessionAndKeepsTheRecord(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var mu sync.Mutex
	var told []RunView
	runs, err := NewRuns(RunsConfig{
		Open: scriptedOpen(&scripted{}, t.TempDir()),
		Notify: func(v RunView) {
			mu.Lock()
			told = append(told, v)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)
	v, err := runs.Create(context.Background(), RunRequest{Title: "stop me"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runs.Stop(v.ID); err != nil {
		t.Fatal(err)
	}
	got, err := runs.Get(v.ID)
	if err != nil || got.Live || got.Status != RunClosed {
		t.Fatalf("after Stop = %+v, %v, want a closed record", got, err)
	}
	mu.Lock()
	last := told[len(told)-1]
	mu.Unlock()
	if last.ID != v.ID || last.Live || last.Status != RunClosed {
		t.Errorf("last announcement = %+v, want the closed run", last)
	}
	if err := runs.Apply(v.ID, RunOp{Op: OpSubmit, Text: "x"}); !errors.Is(err, ErrRunClosed) {
		t.Errorf("submit after Stop = %v, want ErrRunClosed", err)
	}
	if err := runs.Stop(v.ID); !errors.Is(err, ErrRunClosed) {
		t.Errorf("second Stop = %v, want ErrRunClosed", err)
	}
	if err := runs.Stop("RUN-99"); !errors.Is(err, ErrNoRun) {
		t.Errorf("Stop of an unknown run = %v, want ErrNoRun", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/runner/ -run 'TicketRun|AutonomousRunNeeds|StopEnds' 2>&1 | head`
Expected: FAIL, `unknown field TicketID in struct literal` and `runs.Stop undefined`.

- [ ] **Step 3: Write the implementation**

In `internal/runner/runs.go`:

1. In `Run`, after the `ProjectID` field:

```go
	// TicketID, Worktree and Branch are set on a run that works on a ticket.
	TicketID string `json:"ticketId,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
```

2. In `RunRequest`, after `ProjectID`:

```go
	// TicketID, Worktree and Branch tie an autonomous run to a ticket. Open roots the
	// session's tools at Worktree.
	TicketID, Worktree, Branch string
	// Tools are registered into the session once it is built, past the mode's tool subset.
	Tools []tools.Tool
	// OnTurn is called with every turn_start and turn_end, in order, on a goroutine of the
	// run's own; a slow call delays only the next one.
	OnTurn func(uisession.Event)
```

3. In `Create`, replace the mode check (the `if req.Mode != ModeInteractive { ... }` block with
   its comment) with:

```go
	if req.Mode != ModeInteractive && (req.Mode != ModeAutonomous || req.TicketID == "" || req.Worktree == "") {
		// The autonomous policy approves edits on the assumption that a ticket's worktree is
		// all the session can reach; without one there is nothing that assumption holds for.
		return RunView{}, fmt.Errorf("%w: %q", ErrRunMode, req.Mode)
	}
```

4. In `Create`, right after the `if sess == nil || sess.Local == nil { ... }` block:

```go
	for _, t := range req.Tools {
		sess.Tools.Register(t)
	}
```

5. In `Create`, extend the record literal:

```go
	rec := Run{
		ID: id, SessionID: meta.ID, ProjectID: req.ProjectID, Mode: req.Mode,
		TicketID: req.TicketID, Worktree: req.Worktree, Branch: req.Branch,
		Title: meta.Title, Model: opened.Model, Root: opened.Root,
		Status: RunOpen, Created: now, Updated: now,
	}
```

6. After `RunRequest`, add:

```go
// Root is where the session's tools are rooted: the ticket's worktree, else dir.
func (req RunRequest) Root(dir string) string {
	if req.Worktree != "" {
		return req.Worktree
	}
	return dir
}
```

7. In `Create`, after `r.watch(id, sess)`:

```go
	if req.OnTurn != nil {
		follow(sess, req.OnTurn)
	}
```

8. After the `watch` method, add:

```go
// follow hands fn every turn_start and turn_end of the session, in order. It replays from the
// last event it handed over, so a wake-up that arrives while fn is busy loses nothing.
func follow(sess *Session, fn func(uisession.Event)) {
	seen := sess.Local.Seq()
	woke, stop, err := sess.Local.Watch(uisession.KindTurnStart, uisession.KindTurnEnd)
	if err != nil {
		return
	}
	go func() {
		defer stop()
		for range woke {
			evs, err := sess.Local.Replay(seen)
			if err != nil {
				slog.Warn("a run's turns could not be read back", "err", err)
				seen = sess.Local.Seq()
			} else if n := len(evs); n > 0 {
				seen = evs[n-1].Seq
			}
			for _, ev := range turnEvents(evs, err, sess.Local.Running()) {
				fn(ev)
			}
		}
	}()
}

// turnEvents picks the turn events out of a replay. A replay that failed while no turn is
// running stands for the turn_end it may have lost, so a caller waiting on one is not left
// waiting; mid-turn, the next wake-up reads the end.
func turnEvents(evs []uisession.Event, err error, running bool) []uisession.Event {
	if err != nil {
		if running {
			return nil
		}
		return []uisession.Event{{Kind: uisession.KindTurnEnd,
			Error: "the run's events could not be read back: " + err.Error()}}
	}
	var out []uisession.Event
	for _, ev := range evs {
		if ev.Kind == uisession.KindTurnStart || ev.Kind == uisession.KindTurnEnd {
			out = append(out, ev)
		}
	}
	return out
}
```

9. After `Remove`, add:

```go
// Stop ends a run's session the way a daemon restart would: the turn is interrupted, the
// conversation saved, and the record and its journal stay.
func (r *Runs) Stop(id string) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrRunsClosed
	}
	lr := r.byID[id]
	switch {
	case lr == nil:
		r.mu.Unlock()
		return ErrNoRun
	case lr.sess == nil:
		r.mu.Unlock()
		return ErrRunClosed
	}
	r.opening.Add(1)
	defer r.opening.Done()
	sess, rel := lr.sess, lr.release
	lr.sess, lr.release = nil, nil
	r.mu.Unlock()

	meta := sess.Local.Meta()
	closeSession(sess, rel)

	r.mu.Lock()
	if r.byID[id] != lr {
		// Removed while the session was closing; Remove announced it.
		r.mu.Unlock()
		return nil
	}
	r.markClosedLocked(lr, meta)
	lr.version++
	r.saveLocked()
	rec := lr.rec
	r.mu.Unlock()
	r.notify(view(rec, nil))
	return nil
}
```

In `cmd/aigem/webruns.go` `openRun`, replace `reg, err := env.NewTools()` with:

```go
	root := req.Root(env.Cwd)
	reg, err := env.NewToolsAt(root)
```

and in the same function change `Cwd: env.Cwd,` in the `runner.Spec` literal to `Cwd: root,`
and `Root: env.Cwd,` in the returned `runner.Opened` to `Root: root,`.

In `internal/runner/export_test.go` replace the body of `CloseRun` (keep its doc comment's
first sentence) so it shares `Stop` instead of duplicating it, and add `"errors"` to its imports:

```go
// CloseRun ends a run's session and keeps its record, which is the state a run
// reaches after a restart. A run that is already closed is left as it is.
func (r *Runs) CloseRun(id string) error {
	if err := r.Stop(id); err != nil && !errors.Is(err, ErrRunClosed) {
		return err
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test -race ./internal/runner/ -v 2>&1 | grep -E '^(--- FAIL|FAIL|ok)' | head -20`
Expected: `ok` apart from the known macOS failures. The existing
`TestAModeWithNothingBehindItIsRefusedBeforeASessionIsBuilt` still gets `ErrRunMode`, and the
tests that use `CloseRun` pass on `Stop`. `Stop` announces the closed run after the session is
closed rather than before; the one test that reads the announcement
(`closing announced %+v` in `runs_test.go`) reads it after `CloseRun` returns, and `Stop` bumps
the row's version first, so no stale live view can follow it.

- [ ] **Step 5: Lint and commit**

```bash
gofmt -w internal/runner/runs.go internal/runner/runs_ticket_test.go internal/runner/export_test.go cmd/aigem/webruns.go
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./internal/runner/... ./cmd/aigem/...
git add internal/runner/runs.go internal/runner/runs_ticket_test.go internal/runner/export_test.go cmd/aigem/webruns.go
git commit -m "feat(runner): autonomous ticket runs with a turn hook, and Stop"
```

---

### Task 4: `Tickets.Start` and `Tickets.Finish`

**Files:**
- Modify: `internal/runner/ticket_rules.go` (`Ticket.MergePending`)
- Modify: `internal/runner/tickets.go` (`Start`, `Finish`)
- Test: `internal/runner/tickets_test.go` (append)

**Interfaces:**
- Produces:
  ```go
  type Ticket struct { ...; MergePending bool } // json mergePending,omitempty
  func (t *Tickets) Start(project, id, run string) (TicketView, error)
  func (t *Tickets) Finish(project, id, status, comment string, mergePending bool) (TicketView, error)
  ```
  `Start`: a run that is not yet in `Runs` needs a runnable ticket and is appended; a run already
  in `Runs` takes the ticket back from `blocked` (no-op while `running`). Refusals:
  `<id> is already running`, `<id> is not runnable`. `Finish`: only `done` or `blocked`, only
  from `running` or `blocked` (`<id> is <status>; no run drives it`); appends a comment by
  `aigem` (cut to 16 KiB); `mergePending` is kept only on `blocked`. Both bypass person moves
  and settle parents like every other change.

- [ ] **Step 1: Write the failing tests**

Append to `internal/runner/tickets_test.go` (add `"slices"` and `"strings"` to its imports):

```go
func TestARunStartsAndFinishesATicket(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ts, _ := newTestTickets(t, t.TempDir())
	a := mustCreate(t, ts, NewTicket{Title: "a"})
	_, err := ts.Start("PRJ-1", a.ID, "RUN-1")
	refusal(t, err, "TCK-1 is not runnable")
	if _, err := ts.Update("PRJ-1", a.ID, TicketPatch{Status: ptr(TicketReady)}); err != nil {
		t.Fatal(err)
	}

	v, err := ts.Start("PRJ-1", a.ID, "RUN-1")
	if err != nil || v.Status != TicketRunning || !slices.Equal(v.Runs, []string{"RUN-1"}) {
		t.Fatalf("start = %+v, %v", v, err)
	}
	_, err = ts.Start("PRJ-1", a.ID, "RUN-2")
	refusal(t, err, "TCK-1 is already running")

	v, err = ts.Finish("PRJ-1", a.ID, TicketBlocked, "the main checkout has uncommitted changes", true)
	if err != nil || v.Status != TicketBlocked || !v.MergePending || len(v.Comments) != 1 ||
		v.Comments[0].By != "aigem" {
		t.Fatalf("blocked = %+v, %v", v, err)
	}
	v, err = ts.Start("PRJ-1", a.ID, "RUN-1")
	if err != nil || v.Status != TicketRunning || v.MergePending || len(v.Runs) != 1 {
		t.Fatalf("a second turn = %+v, %v, want running again with the same run", v, err)
	}
	v, err = ts.Finish("PRJ-1", a.ID, TicketDone, "Merged.", true)
	if err != nil || v.Status != TicketDone || v.MergePending {
		t.Fatalf("done = %+v, %v", v, err)
	}
	_, err = ts.Finish("PRJ-1", a.ID, TicketBlocked, "late", false)
	refusal(t, err, "TCK-1 is done; no run drives it")
	_, err = ts.Finish("PRJ-1", a.ID, TicketOpen, "x", false)
	refusal(t, err, "cannot leave a ticket open")
}

func TestARunningSubticketDrivesItsParentAndAParentCannotRun(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ts, told := newTestTickets(t, t.TempDir())
	parent := mustCreate(t, ts, NewTicket{Title: "goal"})
	kid := mustCreate(t, ts, NewTicket{Title: "kid", Parent: parent.ID})
	if _, err := ts.Update("PRJ-1", kid.ID, TicketPatch{Status: ptr(TicketReady)}); err != nil {
		t.Fatal(err)
	}
	_, err := ts.Start("PRJ-1", parent.ID, "RUN-1")
	refusal(t, err, "TCK-1 is not runnable")
	*told = nil
	if _, err := ts.Start("PRJ-1", kid.ID, "RUN-1"); err != nil {
		t.Fatal(err)
	}
	if p, _ := ts.Get("PRJ-1", parent.ID); p.Status != TicketRunning {
		t.Errorf("parent = %s, want running", p.Status)
	}
	if !slices.Equal(*told, []string{"PRJ-1/TCK-2", "PRJ-1/TCK-1"}) {
		t.Errorf("announced = %v, want the subticket then its parent", *told)
	}
}

func TestALongRunCommentIsCut(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ts, _ := newTestTickets(t, t.TempDir())
	a := mustCreate(t, ts, NewTicket{Title: "a"})
	if _, err := ts.Update("PRJ-1", a.ID, TicketPatch{Status: ptr(TicketReady)}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Start("PRJ-1", a.ID, "RUN-1"); err != nil {
		t.Fatal(err)
	}
	v, err := ts.Finish("PRJ-1", a.ID, TicketBlocked, strings.Repeat("é", 20<<10), false)
	if err != nil {
		t.Fatal(err)
	}
	if c := v.Comments[0].Text; len(c) > 16<<10+len("…") || !strings.HasSuffix(c, "…") {
		t.Errorf("comment is %d bytes, want at most 16 KiB and a mark that it was cut", len(c))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/runner/ -run 'RunStartsAndFinishes|RunningSubticket|LongRunComment' 2>&1 | head`
Expected: FAIL, `ts.Start undefined`.

- [ ] **Step 3: Write the implementation**

In `internal/runner/ticket_rules.go`, in `Ticket` after `Runs`:

```go
	// MergePending marks a blocked ticket whose branch is committed and checked and only waits
	// for the merge into main.
	MergePending bool `json:"mergePending,omitempty"`
```

In `internal/runner/tickets.go`, after `Comment`:

```go
// maxRunComment caps what a run writes into a ticket's discussion.
const maxRunComment = 16 << 10

// Start hands a ticket to a run. A new run needs a runnable ticket; the run that already
// drives it takes it back from blocked when a person typed into it.
func (t *Tickets) Start(project, id, run string) (TicketView, error) {
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		tk := &tab.Tickets[i]
		owns := slices.Contains(tk.Runs, run)
		switch {
		case owns && tk.Status == TicketRunning:
			return nil, nil
		case owns && tk.Status == TicketBlocked:
		case tk.Status == TicketRunning:
			return nil, refuse("%s is already running", id)
		case !ticketView(tab.Tickets, *tk).Runnable:
			return nil, refuse("%s is not runnable", id)
		}
		if !owns {
			tk.Runs = append(tk.Runs, run)
		}
		tk.Status, tk.MergePending, tk.Updated = TicketRunning, false, t.now()
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	if len(views) == 0 {
		return t.Get(project, id)
	}
	return views[0], nil
}

// Finish records how a run left a ticket: done, or blocked with the reason as a comment.
func (t *Tickets) Finish(project, id, status, comment string, mergePending bool) (TicketView, error) {
	if status != TicketDone && status != TicketBlocked {
		return TicketView{}, refuse("a run cannot leave a ticket %s", status)
	}
	if len(comment) > maxRunComment {
		comment = strings.ToValidUTF8(comment[:maxRunComment], "") + "…"
	}
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		tk := &tab.Tickets[i]
		if tk.Status != TicketRunning && tk.Status != TicketBlocked {
			return nil, refuse("%s is %s; no run drives it", id, tk.Status)
		}
		now := t.now()
		tk.Status, tk.MergePending, tk.Updated = status, mergePending && status == TicketBlocked, now
		if comment != "" {
			tk.Comments = append(tk.Comments, Comment{At: now, By: "aigem", Text: comment})
		}
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/runner/ -run 'Ticket|Subticket|Run' -v 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
gofmt -w internal/runner/ticket_rules.go internal/runner/tickets.go internal/runner/tickets_test.go
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./internal/runner/...
git add internal/runner/ticket_rules.go internal/runner/tickets.go internal/runner/tickets_test.go
git commit -m "feat(runner): tickets are started and finished by runs"
```

---

### Task 5: The repository check, with a budget and a process group

**Files:**
- Modify: `internal/tools/procgroup_unix.go`, `internal/tools/procgroup_windows.go`,
  `internal/tools/impl.go` (rename `configureProcessGroup` to `ConfigureProcessGroup`)
- Create: `internal/runner/ticketcheck.go`
- Test: `internal/runner/ticketcheck_test.go`

**Interfaces:**
- Produces: `tools.ConfigureProcessGroup(cmd *exec.Cmd)`; unexported
  `var checkBudget = 15 * time.Minute`, `readCheck(repo string) (string, error)` (`""` when the
  file or the field is missing), `runCheck(ctx, dir, check string) (string, error)` returning
  the last 4 KiB of combined output; past the budget the error reads `it ran past <budget>`.

- [ ] **Step 1: Write the failing tests**

`internal/runner/ticketcheck_test.go`:

```go
package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadCheckIsOptionalAndStrict(t *testing.T) {
	dir := t.TempDir()
	if got, err := readCheck(dir); got != "" || err != nil {
		t.Fatalf("no file = %q, %v", got, err)
	}
	cfg := filepath.Join(dir, ".aigem", "project.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"check": " make test "}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readCheck(dir); got != "make test" || err != nil {
		t.Errorf("check = %q, %v", got, err)
	}
	if err := os.WriteFile(cfg, []byte(`{"check": 3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCheck(dir); err == nil {
		t.Error("a malformed project.json was accepted")
	}
}

func TestRunCheckKeepsTheEndOfTheOutputAndTheExitStatus(t *testing.T) {
	ctx := context.Background()
	out, err := runCheck(ctx, t.TempDir(), "yes x | head -c 10000; echo; echo tail-line >&2; exit 2")
	if err == nil || !strings.Contains(err.Error(), "exit status 2") {
		t.Errorf("err = %v, want the exit status", err)
	}
	if len(out) > checkOutput || !strings.Contains(out, "tail-line") {
		t.Errorf("output is %d bytes and ends %q", len(out), out[max(0, len(out)-20):])
	}
	if _, err := runCheck(ctx, t.TempDir(), "true"); err != nil {
		t.Errorf("a passing check = %v", err)
	}
}

func TestACheckThatHangsIsStoppedAtTheBudget(t *testing.T) {
	old := checkBudget
	checkBudget = 300 * time.Millisecond
	t.Cleanup(func() { checkBudget = old })
	began := time.Now()
	out, err := runCheck(context.Background(), t.TempDir(), "echo started; sleep 30 & wait")
	if err == nil || !strings.Contains(err.Error(), "ran past") {
		t.Errorf("err = %v, want the budget named", err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("a hanging check took %s to stop", took)
	}
	if !strings.Contains(out, "started") {
		t.Errorf("output = %q, want what it printed before it hung", out)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/runner/ -run 'Check' 2>&1 | head`
Expected: FAIL, `undefined: readCheck`.

- [ ] **Step 3: Write the implementation**

In `internal/tools/procgroup_unix.go` and `internal/tools/procgroup_windows.go` rename
`configureProcessGroup` to `ConfigureProcessGroup` (function and the first word of its doc
comment); in `internal/tools/impl.go` change the call `configureProcessGroup(cmd)` to
`ConfigureProcessGroup(cmd)`.

`internal/runner/ticketcheck.go`:

```go
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gigovich/aigem/internal/gitx"
	"github.com/gigovich/aigem/internal/tools"
)

// checkBudget bounds a repository's check. A variable so a test can shrink it.
var checkBudget = 15 * time.Minute

// checkOutput is how much of a failed check's output a ticket comment keeps.
const checkOutput = 4 << 10

// readCheck reads the check a repository declares in .aigem/project.json; "" when none.
func readCheck(repo string) (string, error) {
	data, err := os.ReadFile(filepath.Join(repo, ".aigem", "project.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var cfg struct {
		Check string `json:"check"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf(".aigem/project.json: %w", err)
	}
	return strings.TrimSpace(cfg.Check), nil
}

// runCheck runs check through the shell in dir within checkBudget, killing its whole process
// group when the budget or ctx ends, and returns the end of its output.
func runCheck(ctx context.Context, dir, check string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, checkBudget)
	defer cancel()
	out := gitx.NewTail(checkOutput)
	cmd := exec.CommandContext(ctx, "sh", "-c", check)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, out, out
	tools.ConfigureProcessGroup(cmd)
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("it ran past %s", checkBudget)
	}
	return out.String(), err
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/runner/ ./internal/tools/ -run 'Check|Bash' -v 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
gofmt -w internal/tools internal/runner/ticketcheck.go internal/runner/ticketcheck_test.go
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./internal/runner/... ./internal/tools/...
git add internal/tools internal/runner/ticketcheck.go internal/runner/ticketcheck_test.go
git commit -m "feat(runner): repository check with a budget and a process group"
```

---

### Task 6: `TicketRuns` - start, end of turn, merge, restart recovery

**Files:**
- Create: `internal/runner/ticketruns.go`
- Test: `internal/runner/ticketruns_test.go`

**Interfaces:**
- Consumes: `Runs.Create/Apply/Get/Remove/Stop` and `RunRequest.Tools/OnTurn` (Task 3),
  `Tickets.Get/List/Start/Finish` (Task 4), `Projects.Get/List`, `isCheckout`, `refuse`,
  `waitFor`, `newTicketDone` (Task 2), `readCheck/runCheck` (Task 5), `gitx` (Task 1).
- Produces:
  ```go
  type TicketRunsConfig struct {
      Runs *Runs; Tickets *Tickets; Projects *Projects
      Finished func(project string, v TicketView, reason string) // after a done/blocked write
  }
  func NewTicketRuns(cfg TicketRunsConfig) *TicketRuns
  func (t *TicketRuns) Start(ctx context.Context, project, id string) (RunView, error)
  func (t *TicketRuns) Recover()
  ```
  Start refusals (`*TicketRefusal`): `<id> is already starting`, `<id> is already running`,
  `<id> is not runnable`, `<repo> is not a git checkout`, `<repo> has no main or master branch`,
  `<path> is in the way of the worktree for <id>; remove it`,
  `"<repo>" is not a repository of this project`, `could not create the worktree: <git>`.
  An existing worktree on its branch is reused; a kept branch without a worktree is checked out
  again. Done comment: `<summary>\n\nMerged aigem/<id> into <main> as <sha12>.`; merge-blocked
  comment: `<reason>\n\nThe agent's summary: <summary>` with `mergePending`; check failure:
  ``the check `<cmd>` failed: <err>`` plus the fenced output.
- Test helpers for Task 7 (in `ticketruns_test.go`): `fixture`, `newFixture`, `gitRepo`,
  `runGit`, `writeFile`, `commitCheck`, `edit`, `done`, `hold`, `waitUntil`, `lastComment`,
  `scripted.left`.

- [ ] **Step 1: Write the failing tests**

`internal/runner/ticketruns_test.go`:

```go
package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/gitx"
	"github.com/gigovich/aigem/internal/llm"
)

// fixture is one project whose directory is the repository, with a scripted model behind
// every run.
type fixture struct {
	t       *testing.T
	repo    string
	project string
	script  *scripted
	runs    *Runs
	tickets *Tickets
	tr      *TicketRuns

	mu   sync.Mutex
	told []string
}

func newFixture(t *testing.T, repo string) *fixture {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fixture{t: t, repo: repo, script: &scripted{}}
	projects, err := NewProjects(ProjectsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(projects.Close)
	pv, err := projects.Add(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	f.project = pv.ID
	f.tickets = NewTickets(TicketsConfig{Dir: t.TempDir()})
	if f.runs, err = NewRuns(RunsConfig{Open: scriptedOpen(f.script, repo)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.runs.Close)
	f.tr = NewTicketRuns(TicketRunsConfig{
		Runs: f.runs, Tickets: f.tickets, Projects: projects,
		Finished: func(_ string, v TicketView, reason string) {
			f.mu.Lock()
			f.told = append(f.told, v.Status+": "+reason)
			f.mu.Unlock()
		},
	})
	return f
}

func gitRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", branch)
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, dir, "README.md", "hello\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "first")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, text string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commitCheck declares a check in the repository and commits it, so main stays clean.
func commitCheck(t *testing.T, repo, check string) {
	t.Helper()
	writeFile(t, repo, ".aigem/project.json", `{"check": "`+check+`"}`)
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "check")
}

// edit is a reply that writes a file before it answers.
func edit(path, text string, then reply) reply {
	return func(ctx context.Context) (llm.Message, error) {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			return llm.Message{}, err
		}
		return then(ctx)
	}
}

func done(summary string) reply { return call("ticket_done", `{"summary":"`+summary+`"}`) }

// hold is a reply that waits until the turn is cancelled, the way a slow model would.
func hold() reply {
	return func(ctx context.Context) (llm.Message, error) {
		<-ctx.Done()
		return llm.Message{}, ctx.Err()
	}
}

func (s *scripted) left() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.replies)
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func lastComment(v TicketView) string {
	if len(v.Comments) == 0 {
		return ""
	}
	return v.Comments[len(v.Comments)-1].Text
}

func (f *fixture) worktree(id string) string { return filepath.Join(f.repo, ".aigem", "worktrees", id) }

// ready makes a runnable ticket and returns its id.
func (f *fixture) ready(title string) string {
	f.t.Helper()
	return f.readyIn("", title)
}

func (f *fixture) readyIn(repo, title string) string {
	f.t.Helper()
	v, err := f.tickets.Create(f.project, NewTicket{Repo: repo, Title: title, Body: "Do " + title + "."})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.tickets.Update(f.project, v.ID, TicketPatch{Status: ptr(TicketReady)}); err != nil {
		f.t.Fatal(err)
	}
	return v.ID
}

func (f *fixture) start(id string) RunView {
	f.t.Helper()
	v, err := f.tr.Start(context.Background(), f.project, id)
	if err != nil {
		f.t.Fatalf("Start %s: %v", id, err)
	}
	return v
}

// next waits until the ticket has more than n comments and is no longer running.
func (f *fixture) next(id string, n int) TicketView {
	f.t.Helper()
	var v TicketView
	waitUntil(f.t, func() bool {
		var err error
		if v, err = f.tickets.Get(f.project, id); err != nil {
			f.t.Fatal(err)
		}
		return v.Status != TicketRunning && len(v.Comments) > n
	})
	return v
}

// idle waits until the scripted model has answered everything and the run's turn has ended.
func (f *fixture) idle(run string) {
	f.t.Helper()
	waitUntil(f.t, func() bool {
		rv, _ := f.runs.Get(run)
		return f.script.left() == 0 && !rv.Running
	})
	time.Sleep(300 * time.Millisecond)
}

func (f *fixture) toldAbout(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.told {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func TestRunRefusesATicketThatCannotStart(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, gitRepo(t, "main"))
	open, err := f.tickets.Create(f.project, NewTicket{Title: "not ready"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.tr.Start(ctx, f.project, open.ID)
	refusal(t, err, "TCK-1 is not runnable")

	inWay := f.ready("in the way")
	writeFile(t, f.worktree(inWay), "left.txt", "x")
	_, err = f.tr.Start(ctx, f.project, inWay)
	refusal(t, err, "is in the way of the worktree for TCK-2")

	_, err = f.tr.Start(ctx, f.project, f.readyIn("../elsewhere", "outside"))
	refusal(t, err, `"../elsewhere" is not a repository of this project`)

	plain := newFixture(t, t.TempDir())
	_, err = plain.tr.Start(ctx, plain.project, plain.ready("x"))
	refusal(t, err, "is not a git checkout")

	trunk := newFixture(t, gitRepo(t, "trunk"))
	_, err = trunk.tr.Start(ctx, trunk.project, trunk.ready("x"))
	refusal(t, err, "has no main or master branch")
}

func TestADoneTicketIsCommittedCheckedAndMergedIntoMain(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	commitCheck(t, f.repo, "test -f notes.txt")
	id := f.ready("add notes")
	// Through the real write tool and a relative path: the file must land in the worktree.
	f.script.then(call("write_file", `{"path":"notes.txt","content":"notes\n"}`), done("Added notes."),
		say("Finished."))

	v := f.start(id)
	if v.TicketID != id || v.Worktree != f.worktree(id) || v.Branch != "aigem/"+id || v.Mode != ModeAutonomous {
		t.Fatalf("run = %+v", v)
	}
	if _, err := f.tr.Start(context.Background(), f.project, id); err == nil {
		t.Error("a second Run of the same ticket was not refused")
	}
	got := f.next(id, 0)
	if c := lastComment(got); got.Status != TicketDone || !strings.HasPrefix(c, "Added notes.") ||
		!strings.Contains(c, "Merged aigem/TCK-1 into main as ") {
		t.Fatalf("ticket = %s %q", got.Status, c)
	}
	if runGit(t, f.repo, "show", "aigem/"+id+":notes.txt") != "notes" {
		t.Error("the agent's file is not on the ticket's branch")
	}
	if parents := strings.Fields(runGit(t, f.repo, "log", "-1", "--format=%P")); len(parents) != 2 {
		t.Errorf("main's head has parents %v, want a merge commit", parents)
	}
	if subjects := runGit(t, f.repo, "log", "--format=%s", "main"); !strings.Contains(subjects,
		"aigem: add notes (TCK-1, RUN-1)") {
		t.Errorf("main's log = %q, want the run's commit", subjects)
	}
	if runGit(t, f.repo, "show", "main:notes.txt") != "notes" {
		t.Error("the agent's file did not reach main")
	}
	if pathExists(f.worktree(id)) {
		t.Error("the worktree was kept after the merge")
	}
	if runGit(t, f.repo, "branch", "--list", "aigem/"+id) == "" {
		t.Error("the branch was deleted after the merge")
	}
	if rv, _ := f.runs.Get(v.ID); rv.Live {
		t.Error("the run is still live after the merge")
	}
	if !f.toldAbout("done: Added notes.") {
		t.Errorf("finished callbacks = %v", f.told)
	}
}

func TestATurnWithoutTicketDoneBlocksAndTheNextTurnIsEvaluatedAgain(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	id := f.ready("pick a database")
	f.script.then(say("Which database should I use?"))
	v := f.start(id)
	got := f.next(id, 0)
	if got.Status != TicketBlocked || got.MergePending || lastComment(got) != "Which database should I use?" {
		t.Fatalf("ticket = %s %q", got.Status, lastComment(got))
	}
	if rv, _ := f.runs.Get(v.ID); !rv.Live || !pathExists(f.worktree(id)) {
		t.Fatal("a blocked ticket lost its run or its worktree")
	}
	if st := runGit(t, f.repo, "status", "--porcelain"); st != "" {
		t.Errorf("the nested worktree shows in the main checkout: %q", st)
	}

	f.script.then(edit(filepath.Join(f.worktree(id), "db.txt"), "sqlite\n", done("Used SQLite.")), say("Done."))
	if err := f.runs.Apply(v.ID, RunOp{Op: OpSubmit, Text: "Use SQLite."}); err != nil {
		t.Fatal(err)
	}
	got = f.next(id, 1)
	if got.Status != TicketDone || len(got.Runs) != 1 {
		t.Fatalf("after the second turn = %s, runs %v", got.Status, got.Runs)
	}
}

func TestTicketDoneOnATicketAPersonClosedChangesNothing(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	id := f.ready("closed under it")
	f.script.then(say("Which one?"))
	v := f.start(id)
	f.next(id, 0)
	if _, err := f.tickets.Update(f.project, id, TicketPatch{Status: ptr(TicketClosed)}); err != nil {
		t.Fatal(err)
	}

	f.script.then(edit(filepath.Join(f.worktree(id), "x.txt"), "x\n", done("Did it anyway.")), say("ok"))
	if err := f.runs.Apply(v.ID, RunOp{Op: OpSubmit, Text: "go on"}); err != nil {
		t.Fatal(err)
	}
	f.idle(v.ID)
	got, _ := f.tickets.Get(f.project, id)
	if got.Status != TicketClosed || len(got.Comments) != 1 {
		t.Errorf("ticket = %s with %d comments, want closed and untouched", got.Status, len(got.Comments))
	}
	if runGit(t, f.repo, "rev-list", "--count", "main") != "1" {
		t.Error("main moved for a closed ticket")
	}
	if strings.Contains(runGit(t, f.repo, "log", "--format=%s", "aigem/"+id), "aigem:") {
		t.Error("the run's work was committed for a closed ticket")
	}
}

func TestTicketDoneThenAnInterruptedTurnIsNotDone(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	id := f.ready("half")
	f.script.then(done("Done, I think."), hold())
	v := f.start(id)
	waitUntil(t, func() bool { return f.script.left() == 0 })
	if err := f.runs.Apply(v.ID, RunOp{Op: OpInterrupt}); err != nil {
		t.Fatal(err)
	}
	got := f.next(id, 0)
	if got.Status != TicketBlocked || lastComment(got) != "interrupted" {
		t.Fatalf("ticket = %s %q", got.Status, lastComment(got))
	}
	if runGit(t, f.repo, "rev-list", "--count", "main") != "1" {
		t.Error("an interrupted turn was merged")
	}
}

func TestAFailingCheckBlocksWithTheEndOfItsOutput(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	commitCheck(t, f.repo, "echo checking; echo broken >&2; exit 3")
	id := f.ready("break it")
	f.script.then(done("Tried."), say("ok"))
	f.start(id)
	got := f.next(id, 0)
	if c := lastComment(got); got.Status != TicketBlocked || got.MergePending ||
		!strings.Contains(c, "exit status 3") || !strings.Contains(c, "broken") {
		t.Fatalf("ticket = %s %q", got.Status, c)
	}
	if strings.Contains(runGit(t, f.repo, "log", "--format=%s", "main"), "Merge") {
		t.Error("a failed check was merged")
	}
}

func TestTheMainCheckoutMustBeCleanAndOnMain(t *testing.T) {
	dirty := newFixture(t, gitRepo(t, "main"))
	writeFile(t, dirty.repo, "scratch.txt", "mine\n")
	id := dirty.ready("a")
	dirty.script.then(edit(filepath.Join(dirty.worktree(id), "a.txt"), "a\n", done("Wrote a.")), say("ok"))
	dirty.start(id)
	got := dirty.next(id, 0)
	if c := lastComment(got); got.Status != TicketBlocked || !got.MergePending ||
		!strings.HasPrefix(c, "the main checkout has uncommitted changes") ||
		!strings.Contains(c, "The agent's summary: Wrote a.") {
		t.Fatalf("dirty = %s %v %q", got.Status, got.MergePending, c)
	}
	if runGit(t, dirty.repo, "rev-list", "--count", "main") != "1" {
		t.Error("main moved although the checkout was dirty")
	}

	moved := newFixture(t, gitRepo(t, "main"))
	runGit(t, moved.repo, "checkout", "-q", "-b", "feature")
	id = moved.ready("b")
	moved.script.then(edit(filepath.Join(moved.worktree(id), "b.txt"), "b\n", done("Wrote b.")), say("ok"))
	moved.start(id)
	got = moved.next(id, 0)
	if !got.MergePending || !strings.HasPrefix(lastComment(got), "the main checkout is on feature, not main") {
		t.Fatalf("moved = %q", lastComment(got))
	}
}

func TestAConflictIsAbortedAndNamesTheFiles(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	id := f.ready("reword")
	wt := f.worktree(id)
	f.script.then(func(ctx context.Context) (llm.Message, error) {
		if err := os.WriteFile(filepath.Join(wt, "README.md"), []byte("theirs\n"), 0o644); err != nil {
			return llm.Message{}, err
		}
		if err := os.WriteFile(filepath.Join(f.repo, "README.md"), []byte("ours\n"), 0o644); err != nil {
			return llm.Message{}, err
		}
		if out, err := exec.Command("git", "-C", f.repo, "commit", "-qam", "ours").CombinedOutput(); err != nil {
			return llm.Message{}, fmt.Errorf("%w: %s", err, out)
		}
		return done("Reworded.")(ctx)
	}, say("ok"))
	f.start(id)
	got := f.next(id, 0)
	want := "merging aigem/TCK-1 into main conflicts in: README.md"
	if !got.MergePending || !strings.HasPrefix(lastComment(got), want) {
		t.Fatalf("ticket = %q, want it to start with %q", lastComment(got), want)
	}
	if st := runGit(t, f.repo, "status", "--porcelain"); st != "" {
		t.Errorf("the main checkout is left with %q", st)
	}
	if pathExists(filepath.Join(f.repo, ".git", "MERGE_HEAD")) {
		t.Error("the merge was not aborted")
	}
}

func TestARestartBlocksTheTicketAndRunReusesItsWorktree(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, gitRepo(t, "main"))
	id := f.ready("left behind")
	if err := gitx.WorktreeAdd(ctx, f.repo, f.worktree(id), "aigem/"+id, "main"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.worktree(id), "half.txt", "half\n")
	runGit(t, f.worktree(id), "add", "-A")
	runGit(t, f.worktree(id), "commit", "-q", "-m", "half done")
	if _, err := f.tickets.Start(f.project, id, "RUN-7"); err != nil {
		t.Fatal(err)
	}

	f.tr.Recover()
	got, err := f.tickets.Get(f.project, id)
	if err != nil || got.Status != TicketBlocked || lastComment(got) != "the daemon restarted" {
		t.Fatalf("after recovery = %s %q, %v", got.Status, lastComment(got), err)
	}
	if !pathExists(f.worktree(id)) {
		t.Error("recovery removed the worktree")
	}

	if _, err := f.tickets.Update(f.project, id, TicketPatch{Status: ptr(TicketReady)}); err != nil {
		t.Fatal(err)
	}
	f.script.then(say("Picking up where it stopped."))
	v := f.start(id)
	if v.Worktree != f.worktree(id) {
		t.Errorf("worktree = %q, want the one the earlier run left", v.Worktree)
	}
	if !strings.Contains(runGit(t, f.repo, "log", "--format=%s", "aigem/"+id), "half done") {
		t.Error("the earlier run's commit is gone")
	}
	got = f.next(id, 1)
	if !slices.Equal(got.Runs, []string{"RUN-7", v.ID}) {
		t.Errorf("runs = %v", got.Runs)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/runner/ -run 'RunRefuses|DoneTicket|TurnWithout|ClosedChanges|Interrupted|FailingCheck|MainCheckout|Conflict|Restart' 2>&1 | head`
Expected: FAIL, `undefined: TicketRuns` (and `NewTicketRuns`, `pathExists`).

- [ ] **Step 3: Write the implementation**

`internal/runner/ticketruns.go`:

```go
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gigovich/aigem/internal/gitx"
	"github.com/gigovich/aigem/internal/tools"
	"github.com/gigovich/aigem/internal/uisession"
)

const ticketRule = "Work in this worktree. When the work is complete, call ticket_done with a short " +
	"summary. If you are stuck or need a decision, explain why and stop without calling it."

type TicketRunsConfig struct {
	Runs     *Runs
	Tickets  *Tickets
	Projects *Projects
	// Finished is told when a run leaves a ticket done or blocked, with the comment it wrote.
	Finished func(project string, v TicketView, reason string)
}

// TicketRuns drives tickets with autonomous runs: a worktree per ticket, a run inside it, and
// the decision taken at the end of each of its turns.
type TicketRuns struct {
	runs     *Runs
	tickets  *Tickets
	projects *Projects
	finished func(string, TicketView, string)

	mu    sync.Mutex
	byRun map[string]*ticketRun
	// starting guards Start: git refuses a second new worktree, not a second run in a reused one.
	starting map[string]bool
	merging  map[string]*sync.Mutex
	closed   bool
	// turns counts the turn evaluations in flight, so Close can wait for them.
	turns sync.WaitGroup
}

// ticketRun is one live run on a ticket. mu serialises its turn ends with stop and delete;
// cancel stops a commit or a check in flight.
type ticketRun struct {
	project, ticket, run string
	place                ticketPlace
	summary              atomic.Pointer[string]
	ctx                  context.Context
	cancel               context.CancelFunc

	mu   sync.Mutex
	gone bool
	// lost is set when a turn could not take its ticket back; that turn's end is ignored.
	lost bool
}

type ticketPlace struct{ repo, main, worktree, branch string }

func NewTicketRuns(cfg TicketRunsConfig) *TicketRuns {
	t := &TicketRuns{
		runs: cfg.Runs, tickets: cfg.Tickets, projects: cfg.Projects, finished: cfg.Finished,
		byRun: map[string]*ticketRun{}, starting: map[string]bool{}, merging: map[string]*sync.Mutex{},
	}
	if t.finished == nil {
		t.finished = func(string, TicketView, string) {}
	}
	return t
}

// Start gives a runnable ticket its worktree - the one an earlier run left, or a new one - and
// an autonomous run inside it, and sends the ticket as the run's first message.
func (t *TicketRuns) Start(ctx context.Context, project, id string) (RunView, error) {
	key := project + "/" + id
	t.mu.Lock()
	switch {
	case t.closed:
		t.mu.Unlock()
		return RunView{}, ErrRunsClosed
	case t.starting[key]:
		t.mu.Unlock()
		return RunView{}, refuse("%s is already starting", id)
	}
	t.starting[key] = true
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.starting, key)
		t.mu.Unlock()
	}()

	tk, err := t.tickets.Get(project, id)
	if err != nil {
		return RunView{}, err
	}
	switch {
	case tk.Status == TicketRunning:
		return RunView{}, refuse("%s is already running", id)
	case !tk.Runnable:
		return RunView{}, refuse("%s is not runnable", id)
	}
	pl, err := t.place(ctx, project, tk.Ticket)
	if err != nil {
		return RunView{}, err
	}
	undo, err := t.prepare(ctx, id, pl)
	if err != nil {
		return RunView{}, err
	}

	tr := &ticketRun{project: project, ticket: id, place: pl}
	tr.ctx, tr.cancel = context.WithCancel(context.Background())
	v, err := t.runs.Create(ctx, RunRequest{
		Mode: ModeAutonomous, Title: id + ": " + tk.Title, ProjectID: project,
		TicketID: id, Worktree: pl.worktree, Branch: pl.branch,
		Tools:  []tools.Tool{newTicketDone(func(s string) { tr.summary.Store(&s) })},
		OnTurn: func(ev uisession.Event) { t.onTurn(tr, ev) },
	})
	if err != nil {
		tr.cancel()
		undo()
		return RunView{}, err
	}
	tr.run = v.ID
	if _, err := t.tickets.Start(project, id, v.ID); err != nil {
		tr.cancel()
		if rmErr := t.runs.Remove(v.ID); rmErr != nil {
			slog.Warn("a ticket run that could not start was not deleted", "run", v.ID, "err", rmErr)
		}
		undo()
		return RunView{}, err
	}
	t.mu.Lock()
	t.byRun[v.ID] = tr
	t.mu.Unlock()
	if err := t.runs.Apply(v.ID, RunOp{Op: OpSubmit, Text: ticketPrompt(tk.Ticket)}); err != nil {
		t.finish(project, id, TicketBlocked, "the ticket could not be sent to the run: "+err.Error(), false)
	}
	return t.runs.Get(v.ID)
}

// prepare gives the ticket its worktree on its branch, reusing what an earlier run left, and
// returns how to take back what it created.
func (t *TicketRuns) prepare(ctx context.Context, id string, pl ticketPlace) (func(), error) {
	switch {
	case !isCheckout(pl.repo):
		return nil, refuse("%s is not a git checkout", pl.repo)
	case pl.main == "":
		return nil, refuse("%s has no main or master branch", pl.repo)
	}
	branch, dir := gitx.BranchExists(ctx, pl.repo, pl.branch), pathExists(pl.worktree)
	switch {
	case branch && dir:
		return func() {}, nil
	case dir:
		return nil, refuse("%s is in the way of the worktree for %s; remove it", pl.worktree, id)
	}
	if strings.HasPrefix(pl.worktree, pl.repo+string(filepath.Separator)) {
		if err := gitx.Exclude(ctx, pl.repo, "/.aigem/worktrees/"); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(pl.worktree), 0o755); err != nil {
		return nil, err
	}
	base := pl.main
	if branch {
		base = ""
	}
	if err := gitx.WorktreeAdd(ctx, pl.repo, pl.worktree, pl.branch, base); err != nil {
		return nil, refuse("could not create the worktree: %v", err)
	}
	return func() { t.unplace(pl, !branch) }, nil
}

// Recover blocks the tickets a previous daemon left running: their runs did not survive it.
func (t *TicketRuns) Recover() {
	for _, p := range t.projects.List() {
		views, err := t.tickets.List(p.ID)
		if err != nil {
			slog.Warn("a project's tickets could not be read on start", "project", p.ID, "err", err)
			continue
		}
		for _, v := range views {
			if v.Status != TicketRunning || v.Progress != nil {
				continue
			}
			if n := len(v.Runs); n > 0 && t.live(v.Runs[n-1]) {
				continue
			}
			t.finish(p.ID, v.ID, TicketBlocked, "the daemon restarted", false)
		}
	}
}

func (t *TicketRuns) onTurn(tr *ticketRun, ev uisession.Event) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.turns.Add(1)
	t.mu.Unlock()
	defer t.turns.Done()

	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.gone {
		return
	}
	switch ev.Kind {
	case uisession.KindTurnStart:
		_, err := t.tickets.Start(tr.project, tr.ticket, tr.run)
		tr.lost = err != nil
		if err != nil {
			slog.Warn("a ticket run's new turn could not take its ticket back", "ticket", tr.ticket, "err", err)
		}
	case uisession.KindTurnEnd:
		summary := tr.summary.Swap(nil)
		switch {
		case tr.lost:
			tr.lost = false
		case summary == nil || ev.Interrupted || ev.Error != "":
			t.finish(tr.project, tr.ticket, TicketBlocked, lastWords(ev), false)
		default:
			t.deliver(tr, *summary)
		}
	}
}

// deliver commits what the run left in its worktree, runs the repository's check and merges.
// It is called with tr.mu held. A cancelled tr.ctx means the run was stopped, deleted or the
// daemon is closing: nothing more is written, except that a merge that has begun finishes.
func (t *TicketRuns) deliver(tr *ticketRun, summary string) {
	ctx, pl := tr.ctx, tr.place
	block := func(reason string, merge bool) {
		if ctx.Err() == nil {
			t.finish(tr.project, tr.ticket, TicketBlocked, reason, merge)
		}
	}
	tk, err := t.tickets.Get(tr.project, tr.ticket)
	if err != nil || tk.Status != TicketRunning || len(tk.Runs) == 0 || tk.Runs[len(tk.Runs)-1] != tr.run {
		slog.Warn("a run finished a ticket it no longer drives", "ticket", tr.ticket, "run", tr.run)
		return
	}
	msg := fmt.Sprintf("aigem: %s (%s, %s)", tk.Title, tr.ticket, tr.run)
	if _, err := gitx.CommitAll(ctx, pl.worktree, msg); err != nil {
		block("could not commit the worktree: "+err.Error(), false)
		return
	}
	if reason := checkWorktree(ctx, pl); reason != "" {
		block(reason, false)
		return
	}
	if ctx.Err() != nil {
		return
	}
	sha, reason := t.merge(ctx, pl)
	if reason != "" {
		block(reason+"\n\nThe agent's summary: "+summary, true)
		return
	}
	tr.gone = true
	t.forget(tr.run)
	tr.cancel()
	t.complete(tr.project, tr.ticket, pl, summary+"\n\n"+merged(pl, sha), tr.run)
}

// merge brings the ticket's branch into main in the repository's own checkout, one merge per
// repository at a time. Once begun it is not cancelled - a merge killed half-way would leave
// the person's checkout mid-merge - and each git command is bounded by gitx.Timeout instead.
func (t *TicketRuns) merge(ctx context.Context, pl ticketPlace) (string, string) {
	ctx = context.WithoutCancel(ctx)
	t.mu.Lock()
	mu := t.merging[pl.repo]
	if mu == nil {
		mu = &sync.Mutex{}
		t.merging[pl.repo] = mu
	}
	t.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()

	clean, err := gitx.IsClean(ctx, pl.repo)
	if err != nil {
		return "", "could not read the main checkout: " + err.Error()
	}
	if !clean {
		return "", "the main checkout has uncommitted changes"
	}
	cur, err := gitx.CurrentBranch(ctx, pl.repo)
	if err != nil {
		return "", "could not read the main checkout: " + err.Error()
	}
	if cur != pl.main {
		return "", fmt.Sprintf("the main checkout is on %s, not %s", cur, pl.main)
	}
	sha, err := gitx.Merge(ctx, pl.repo, pl.branch)
	if err == nil {
		return sha, ""
	}
	files, _ := gitx.ConflictFiles(ctx, pl.repo)
	if len(files) == 0 {
		return "", "the merge failed: " + err.Error()
	}
	if err := gitx.MergeAbort(ctx, pl.repo); err != nil {
		slog.Error("a conflicting merge could not be aborted", "repo", pl.repo, "err", err)
	}
	return "", fmt.Sprintf("merging %s into %s conflicts in: %s", pl.branch, pl.main, strings.Join(files, ", "))
}

// complete records a merged ticket: the run ends, the worktree goes unless a person's turn
// left changes in it, the branch stays.
func (t *TicketRuns) complete(project, id string, pl ticketPlace, comment, run string) {
	if err := t.runs.Stop(run); err != nil && !errors.Is(err, ErrRunClosed) && !errors.Is(err, ErrNoRun) {
		slog.Warn("a merged ticket's run could not be stopped", "run", run, "err", err)
	}
	if err := gitx.WorktreeRemove(context.Background(), pl.repo, pl.worktree, false); err != nil {
		comment += fmt.Sprintf("\n\nThe worktree was kept at %s: %v", pl.worktree, err)
	}
	t.finish(project, id, TicketDone, comment, false)
}

func (t *TicketRuns) finish(project, id, status, comment string, mergePending bool) {
	v, err := t.tickets.Finish(project, id, status, comment, mergePending)
	if err != nil {
		slog.Warn("a ticket run's outcome could not be recorded", "ticket", id, "err", err)
		return
	}
	t.finished(project, v, comment)
}

func (t *TicketRuns) forget(run string) *ticketRun {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr := t.byRun[run]
	delete(t.byRun, run)
	return tr
}

func (t *TicketRuns) live(run string) bool {
	v, err := t.runs.Get(run)
	return err == nil && v.Live
}

// place is where a ticket's work happens: its repository, the branch it merges into, its
// worktree and its branch.
func (t *TicketRuns) place(ctx context.Context, project string, tk Ticket) (ticketPlace, error) {
	pv, err := t.projects.Get(project)
	if err != nil {
		return ticketPlace{}, err
	}
	if tk.Repo != "" && (!filepath.IsLocal(tk.Repo) || filepath.Base(tk.Repo) != tk.Repo) {
		return ticketPlace{}, refuse("%q is not a repository of this project", tk.Repo)
	}
	repo := filepath.Join(pv.Dir, tk.Repo)
	return ticketPlace{
		repo: repo, main: gitx.MainBranch(ctx, repo),
		worktree: filepath.Join(pv.Dir, ".aigem", "worktrees", tk.ID), branch: "aigem/" + tk.ID,
	}, nil
}

// unplace removes a ticket's worktree and, when asked, its branch, logging what git refuses.
func (t *TicketRuns) unplace(pl ticketPlace, branch bool) {
	ctx := context.Background()
	if err := gitx.WorktreeRemove(ctx, pl.repo, pl.worktree, true); err != nil {
		slog.Warn("a ticket's worktree could not be removed", "path", pl.worktree, "err", err)
	}
	if !branch {
		return
	}
	if err := gitx.BranchDelete(ctx, pl.repo, pl.branch); err != nil {
		slog.Warn("a ticket's branch could not be deleted", "branch", pl.branch, "err", err)
	}
}

// checkWorktree runs the repository's declared check in the worktree and returns why it
// failed, or "". The command is read from the main checkout; the code it runs is the run's.
func checkWorktree(ctx context.Context, pl ticketPlace) string {
	check, err := readCheck(pl.repo)
	if err != nil {
		return "could not read the check: " + err.Error()
	}
	if check == "" {
		return ""
	}
	out, err := runCheck(ctx, pl.worktree, check)
	if err == nil {
		return ""
	}
	return fmt.Sprintf("the check `%s` failed: %v\n\n```\n%s\n```", check, err, out)
}

func merged(pl ticketPlace, sha string) string {
	return fmt.Sprintf("Merged %s into %s as %s.", pl.branch, pl.main, sha[:min(12, len(sha))])
}

// lastWords is what a turn that did not finish its ticket leaves on it.
func lastWords(ev uisession.Event) string {
	switch {
	case ev.Interrupted:
		return "interrupted"
	case ev.Error != "":
		return "the turn ended with an error: " + ev.Error
	case strings.TrimSpace(ev.Text) != "":
		return ev.Text
	}
	return "the turn ended without ticket_done"
}

func ticketPrompt(tk Ticket) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s: %s\n\n", tk.ID, tk.Title)
	if tk.Body != "" {
		b.WriteString(tk.Body + "\n\n")
	}
	if len(tk.Comments) > 0 {
		b.WriteString("## Discussion\n\n")
		for _, c := range tk.Comments {
			fmt.Fprintf(&b, "%s: %s\n\n", c.By, c.Text)
		}
	}
	b.WriteString(ticketRule)
	return b.String()
}

func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/runner/ -run 'RunRefuses|DoneTicket|TurnWithout|ClosedChanges|Interrupted|FailingCheck|MainCheckout|Conflict|Restart' -v 2>&1 | tail -30`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
gofmt -w internal/runner/ticketruns.go internal/runner/ticketruns_test.go
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./internal/runner/...
git add internal/runner/ticketruns.go internal/runner/ticketruns_test.go
git commit -m "feat(runner): ticket runs - start, end of turn, commit, check, merge, recovery"
```

---

### Task 7: Stop, delete, retry merge, worktrees, discard and close

**Files:**
- Modify: `internal/runner/ticketruns.go`
- Test: `internal/runner/ticketruns_test.go` (append)

**Interfaces:**
- Produces:
  ```go
  var ErrNoWorktree = errors.New("runner: no such worktree")
  type Worktree struct{ Repo, Name, Path, Ticket, Run, State string } // State: running|kept|merged
  func (t *TicketRuns) Stop(run string) error   // any run; a ticket it drove: "stopped by a person"
  func (t *TicketRuns) Remove(run string) error // any run; a ticket it drove: "the run was deleted"
  func (t *TicketRuns) Merge(ctx context.Context, project, id string) (TicketView, error)
  func (t *TicketRuns) Worktrees(ctx context.Context, project string) ([]Worktree, error)
  func (t *TicketRuns) Discard(ctx context.Context, project, name string) error
  func (t *TicketRuns) Close() // cancels every delivery (a check is killed), waits up to 30 s
  ```
  `Merge` refusals: `<id> is not waiting for a merge` and the merge reasons from Task 6; it runs
  even when the request that asked for it is cancelled. `Discard`: `ErrNoWorktree`; refusal
  `<name> is worked on by the live run <run>; stop it first`; it is the only `--force` removal.

- [ ] **Step 1: Write the failing tests**

Append to `internal/runner/ticketruns_test.go` (add `"errors"` to its imports):

```go
func TestStoppingTheDrivingRunBlocksTheTicketOnce(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	id := f.ready("long")
	f.script.then(hold())
	v := f.start(id)
	waitUntil(t, func() bool { rv, _ := f.runs.Get(v.ID); return rv.Running })

	if err := f.tr.Stop(v.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := f.tickets.Get(f.project, id)
	if got.Status != TicketBlocked || len(got.Comments) != 1 || lastComment(got) != "stopped by a person" {
		t.Fatalf("ticket = %s %v", got.Status, got.Comments)
	}
	if rv, _ := f.runs.Get(v.ID); rv.Live || rv.Status != RunClosed {
		t.Errorf("run = %+v, want closed", rv)
	}
	if !pathExists(f.worktree(id)) {
		t.Error("Stop removed the worktree")
	}
	time.Sleep(200 * time.Millisecond)
	if again, _ := f.tickets.Get(f.project, id); len(again.Comments) != 1 {
		t.Errorf("a late turn end wrote %q", lastComment(again))
	}
}

func TestStoppingDuringTheCheckKillsItAndLeavesOneComment(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	commitCheck(t, f.repo, "sleep 30")
	id := f.ready("slow check")
	f.script.then(edit(filepath.Join(f.worktree(id), "s.txt"), "s\n", done("Slow.")), say("ok"))
	v := f.start(id)
	waitUntil(t, func() bool {
		out, _ := exec.Command("git", "-C", f.repo, "log", "-1", "--format=%s", "aigem/"+id).Output()
		return strings.HasPrefix(string(out), "aigem: ")
	})

	began := time.Now()
	if err := f.tr.Stop(v.ID); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Errorf("Stop during a check took %s", took)
	}
	got, _ := f.tickets.Get(f.project, id)
	if got.Status != TicketBlocked || len(got.Comments) != 1 || lastComment(got) != "stopped by a person" {
		t.Fatalf("ticket = %s %v", got.Status, got.Comments)
	}
}

func TestStoppingDuringAMergeLeavesMainConsistent(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	marker := filepath.Join(t.TempDir(), "merging")
	hooks := filepath.Join(f.repo, ".git", "hooks")
	writeFile(t, hooks, "pre-merge-commit", "#!/bin/sh\ntouch '"+marker+"'\nsleep 2\n")
	if err := os.Chmod(filepath.Join(hooks, "pre-merge-commit"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.repo, "config", "core.hooksPath", hooks)
	id := f.ready("slow merge")
	f.script.then(edit(filepath.Join(f.worktree(id), "m.txt"), "m\n", done("Merged slowly.")), say("ok"))
	v := f.start(id)
	waitUntil(t, func() bool { return pathExists(marker) })

	if err := f.tr.Stop(v.ID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"MERGE_HEAD", "index.lock"} {
		if pathExists(filepath.Join(f.repo, ".git", name)) {
			t.Errorf("%s was left in the main checkout", name)
		}
	}
	got, _ := f.tickets.Get(f.project, id)
	merged := runGit(t, f.repo, "rev-list", "--count", "main") != "1"
	if merged != (got.Status == TicketDone) {
		t.Errorf("main merged = %v, but the ticket is %s", merged, got.Status)
	}
}

func TestDeletingTheDrivingRunBlocksTheTicket(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	id := f.ready("ask")
	f.script.then(hold())
	v := f.start(id)
	waitUntil(t, func() bool { rv, _ := f.runs.Get(v.ID); return rv.Running })

	if err := f.tr.Remove(v.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := f.tickets.Get(f.project, id)
	if got.Status != TicketBlocked || len(got.Comments) != 1 || lastComment(got) != "the run was deleted" {
		t.Fatalf("ticket = %s %v", got.Status, got.Comments)
	}
	if _, err := f.runs.Get(v.ID); !errors.Is(err, ErrNoRun) {
		t.Errorf("run after delete = %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if again, _ := f.tickets.Get(f.project, id); len(again.Comments) != 1 {
		t.Errorf("a late turn end wrote %q", lastComment(again))
	}
}

func TestRetryMergeWaitsForACleanCheckout(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, gitRepo(t, "main"))
	writeFile(t, f.repo, "scratch.txt", "mine\n")
	id := f.ready("retry")
	f.script.then(edit(filepath.Join(f.worktree(id), "r.txt"), "r\n", done("Wrote r.")), say("ok"))
	v := f.start(id)
	f.next(id, 0)

	_, err := f.tr.Merge(ctx, f.project, id)
	refusal(t, err, "the main checkout has uncommitted changes")
	if err := os.Remove(filepath.Join(f.repo, "scratch.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.worktree(id), "stray.txt", "a person's edit\n")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got, err := f.tr.Merge(cancelled, f.project, id)
	if err != nil || got.Status != TicketDone ||
		!strings.HasPrefix(lastComment(got), "Merged aigem/TCK-1 into main as ") {
		t.Fatalf("retry = %s %q, %v", got.Status, lastComment(got), err)
	}
	if !strings.Contains(lastComment(got), "The worktree was kept at ") || !pathExists(f.worktree(id)) {
		t.Errorf("a worktree with changes was not kept: %q", lastComment(got))
	}
	if rv, _ := f.runs.Get(v.ID); rv.Live {
		t.Error("the run is still live after the retried merge")
	}
	_, err = f.tr.Merge(ctx, f.project, id)
	refusal(t, err, "TCK-1 is not waiting for a merge")
}

func TestAMergedBranchIsListedAsMerged(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, gitRepo(t, "main"))
	id := f.ready("merge me")
	f.script.then(edit(filepath.Join(f.worktree(id), "a.txt"), "a\n", done("A.")), say("ok"))
	v := f.start(id)
	f.next(id, 0)
	list, err := f.tr.Worktrees(ctx, f.project)
	if err != nil || len(list) != 1 {
		t.Fatalf("worktrees = %+v, %v", list, err)
	}
	if w := list[0]; w.State != "merged" || w.Path != "" || w.Run != v.ID {
		t.Errorf("worktree = %+v, want the kept branch as merged", w)
	}
}

func TestWorktreesAreListedAndDiscardedOnceTheirRunIsGone(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, gitRepo(t, "main"))
	id := f.ready("discard me")
	f.script.then(say("Stuck."))
	v := f.start(id)
	f.next(id, 0)

	list, err := f.tr.Worktrees(ctx, f.project)
	if err != nil || len(list) != 1 {
		t.Fatalf("worktrees = %+v, %v", list, err)
	}
	if w := list[0]; w.Name != id || w.Ticket != id || w.Run != v.ID || w.State != "running" || w.Path == "" {
		t.Fatalf("worktree = %+v", w)
	}
	refusal(t, f.tr.Discard(ctx, f.project, id), "stop it first")
	if err := f.tr.Stop(v.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ = f.tr.Worktrees(ctx, f.project); list[0].State != "kept" {
		t.Errorf("state after Stop = %q, want kept", list[0].State)
	}
	if err := f.tr.Discard(ctx, f.project, id); err != nil {
		t.Fatal(err)
	}
	if list, _ = f.tr.Worktrees(ctx, f.project); len(list) != 0 || pathExists(f.worktree(id)) {
		t.Errorf("after discard = %+v", list)
	}
	if err := f.tr.Discard(ctx, f.project, "TCK-9"); !errors.Is(err, ErrNoWorktree) {
		t.Errorf("discard of an unknown name = %v", err)
	}

	if _, err := f.tickets.Update(f.project, id, TicketPatch{Status: ptr(TicketReady)}); err != nil {
		t.Fatal(err)
	}
	f.script.then(say("Stuck again."))
	if again := f.start(id); again.ID == v.ID {
		t.Error("a new Run reused the old run")
	}
}

func TestCloseKillsADeliveryInFlightAndLeavesTheTicketForRecovery(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	commitCheck(t, f.repo, "sleep 30")
	id := f.ready("closing")
	f.script.then(edit(filepath.Join(f.worktree(id), "c.txt"), "c\n", done("Closing.")), say("ok"))
	f.start(id)
	waitUntil(t, func() bool {
		out, _ := exec.Command("git", "-C", f.repo, "log", "-1", "--format=%s", "aigem/"+id).Output()
		return strings.HasPrefix(string(out), "aigem: ")
	})

	began := time.Now()
	f.tr.Close()
	if took := time.Since(began); took > 10*time.Second {
		t.Errorf("Close waited %s for a check", took)
	}
	if got, _ := f.tickets.Get(f.project, id); got.Status != TicketRunning || len(got.Comments) != 0 {
		t.Fatalf("after Close = %s %v, want running and untouched", got.Status, got.Comments)
	}
	if _, err := f.tr.Start(context.Background(), f.project, f.ready("late")); !errors.Is(err, ErrRunsClosed) {
		t.Errorf("Start after Close = %v", err)
	}
	f.runs.Close()
	f.tr.Recover()
	got, _ := f.tickets.Get(f.project, id)
	if got.Status != TicketBlocked || lastComment(got) != "the daemon restarted" {
		t.Errorf("after recovery = %s %q", got.Status, lastComment(got))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/runner/ -run 'Stopping|DeletingTheDriving|RetryMerge|MergedBranch|WorktreesAreListed|CloseKills' 2>&1 | head`
Expected: FAIL, `f.tr.Stop undefined` (and `Remove`, `Merge`, `Worktrees`, `Discard`, `Close`,
`ErrNoWorktree`).

- [ ] **Step 3: Write the implementation**

In `internal/runner/ticketruns.go` add `"slices"` and `"time"` to the imports, and after the
`ticketRule` constant:

```go
var ErrNoWorktree = errors.New("runner: no such worktree")

// closeTurnsWait bounds how long Close waits for the deliveries it cancelled.
const closeTurnsWait = 30 * time.Second

// Worktree is one aigem/<ticket> branch in a project's repository. State is running while
// the ticket's run is live, merged once the branch is in main and its worktree gone, and kept
// otherwise.
type Worktree struct {
	Repo, Name, Path, Ticket, Run, State string
}
```

After `Recover`:

```go
// Close stops every delivery in flight - a check is killed, a merge that has begun finishes -
// and waits for them, bounded. A ticket left running is blocked by Recover on the next start.
func (t *TicketRuns) Close() {
	t.mu.Lock()
	t.closed = true
	trs := make([]*ticketRun, 0, len(t.byRun))
	for _, tr := range t.byRun {
		trs = append(trs, tr)
	}
	t.mu.Unlock()
	for _, tr := range trs {
		tr.cancel()
	}
	if !waitFor(&t.turns, closeTurnsWait) {
		slog.Warn("a ticket run was still delivering when the daemon stopped waiting for it")
	}
}

// Stop ends a run's session and keeps its record; a ticket it drove becomes blocked. The ticket
// is blocked first, so the end of the interrupted turn finds the run already let go.
func (t *TicketRuns) Stop(run string) error {
	drove := t.detach(run, "stopped by a person")
	err := t.runs.Stop(run)
	if drove && errors.Is(err, ErrRunClosed) {
		// It finished its ticket, which ends the run, while this was waiting for it.
		return nil
	}
	return err
}

// Remove deletes a run; a ticket it drove becomes blocked.
func (t *TicketRuns) Remove(run string) error {
	t.detach(run, "the run was deleted")
	return t.runs.Remove(run)
}

// detach lets go of a run's ticket and reports whether the run drove one.
func (t *TicketRuns) detach(run, reason string) bool {
	tr := t.forget(run)
	if tr == nil {
		return false
	}
	tr.cancel()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.gone {
		return true
	}
	tr.gone = true
	v, err := t.tickets.Get(tr.project, tr.ticket)
	if err == nil && (v.Status == TicketRunning || v.Status == TicketBlocked) {
		t.finish(tr.project, tr.ticket, TicketBlocked, reason, v.MergePending)
	}
	return true
}

// Merge repeats the merge for a ticket blocked on one; a refusal carries the same reasons. It
// is not tied to the request that asked: a merge is not cancelled once it begins.
func (t *TicketRuns) Merge(ctx context.Context, project, id string) (TicketView, error) {
	ctx = context.WithoutCancel(ctx)
	v, err := t.tickets.Get(project, id)
	if err != nil {
		return TicketView{}, err
	}
	if v.Status != TicketBlocked || !v.MergePending {
		return TicketView{}, refuse("%s is not waiting for a merge", id)
	}
	pl, err := t.place(ctx, project, v.Ticket)
	if err != nil {
		return TicketView{}, err
	}
	sha, reason := t.merge(ctx, pl)
	if reason != "" {
		return TicketView{}, refuse("%s", reason)
	}
	run := ""
	if n := len(v.Runs); n > 0 {
		run = v.Runs[n-1]
	}
	if tr := t.forget(run); tr != nil {
		tr.cancel()
		tr.mu.Lock()
		tr.gone = true
		tr.mu.Unlock()
	}
	t.complete(project, id, pl, merged(pl, sha), run)
	return t.tickets.Get(project, id)
}

// Worktrees lists the aigem/* branches of every repository in the project.
func (t *TicketRuns) Worktrees(ctx context.Context, project string) ([]Worktree, error) {
	repos, err := t.projects.Repositories(ctx, project)
	if err != nil {
		return nil, err
	}
	views, err := t.tickets.List(project)
	if err != nil {
		return nil, err
	}
	out := []Worktree{}
	for _, r := range repos {
		branches, err := gitx.Branches(ctx, r.Dir, "aigem")
		if err != nil {
			return nil, err
		}
		paths, err := gitx.Worktrees(ctx, r.Dir)
		if err != nil {
			return nil, err
		}
		for _, b := range branches {
			w := Worktree{Repo: r.Name, Name: strings.TrimPrefix(b, "aigem/"), Path: paths[b], State: "kept"}
			if i := slices.IndexFunc(views, func(v TicketView) bool { return v.ID == w.Name }); i >= 0 {
				w.Ticket = w.Name
				if n := len(views[i].Runs); n > 0 {
					w.Run = views[i].Runs[n-1]
				}
			}
			switch {
			case t.live(w.Run):
				w.State = "running"
			case w.Path == "" && r.Main != "" && gitx.IsMerged(ctx, r.Dir, b, r.Main):
				w.State = "merged"
			}
			out = append(out, w)
		}
	}
	return out, nil
}

// Discard removes a ticket's worktree, changes and all, and its branch, unless its run is live.
func (t *TicketRuns) Discard(ctx context.Context, project, name string) error {
	pv, err := t.projects.Get(project)
	if err != nil {
		return err
	}
	list, err := t.Worktrees(ctx, project)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(list, func(w Worktree) bool { return w.Name == name })
	if i < 0 {
		return ErrNoWorktree
	}
	w := list[i]
	if w.State == "running" {
		return refuse("%s is worked on by the live run %s; stop it first", name, w.Run)
	}
	repo := filepath.Join(pv.Dir, w.Repo)
	if w.Path != "" {
		if err := gitx.WorktreeRemove(ctx, repo, w.Path, true); err != nil {
			return err
		}
	}
	return gitx.BranchDelete(ctx, repo, "aigem/"+name)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/runner/ -v 2>&1 | grep -E '^(--- FAIL|FAIL|ok)' | head -20`
Expected: `ok`, apart from the known macOS failures listed in Global Constraints.

- [ ] **Step 5: Lint and commit**

```bash
gofmt -w internal/runner/ticketruns.go internal/runner/ticketruns_test.go
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./internal/runner/...
git add internal/runner/ticketruns.go internal/runner/ticketruns_test.go
git commit -m "feat(runner): stop, delete, retry merge, worktrees, discard and close for ticket runs"
```

---

### Task 8: HTTP routes and daemon wiring

**Files:**
- Modify: `internal/web/backend.go` (`RunsBackend.StopRun`; `Run` gains `ticketId`, `worktree`,
  `branch`)
- Modify: `internal/web/api_runs.go` (`handleStopRun`, `ErrNoWorktree` in `writeRunError`)
- Modify: `internal/web/api_tickets.go` (four methods on `TicketsBackend`, `Worktree`,
  `ErrNoWorktree`, `Ticket.MergePending`, four handlers)
- Modify: `internal/web/server.go` (routes)
- Modify: `cmd/aigem/webbackend.go` (`ticketRuns` field, `StopRun`, `RemoveRun`, `webRun`)
- Modify: `cmd/aigem/webtickets.go` (adapter methods, `ticketFinished`, `ticketRunError`)
- Modify: `cmd/aigem/webcmd.go` (build the coordinator, `Recover`)
- Test: `internal/web/api_tickets_test.go`, `internal/web/api_runs_test.go`,
  `internal/web/backend_test.go` (fake `StopRun`), `cmd/aigem/webtickets_test.go`

**Interfaces:**
- Consumes: `runner.TicketRuns` and friends (Tasks 6, 7), `runner.Runs.Stop` (Task 3).
- Produces:
  ```go
  // RunsBackend
  StopRun(ctx context.Context, id string) error
  // TicketsBackend
  RunTicket(ctx context.Context, project, id string) (Run, error)
  MergeTicket(ctx context.Context, project, id string) (Ticket, error)
  Worktrees(ctx context.Context, project string) ([]Worktree, error)
  DiscardWorktree(ctx context.Context, project, name string) error
  type Worktree struct{ Repo, Name, Path, Ticket, Run, State string } // json repo, name, path,
                                                                      // ticket, run, state
  var ErrNoWorktree // 404 "no such worktree"
  ```
  Routes: `POST /api/runs/{id}/stop` (204), `POST /api/projects/{id}/tickets/{tid}/run` (201
  with the run), `POST /api/projects/{id}/tickets/{tid}/merge` (200 with the ticket),
  `GET /api/projects/{id}/worktrees` (200, `[]` when empty),
  `DELETE /api/projects/{id}/worktrees/{name}` (204). Refusals are 409, unknown project /
  ticket / worktree 404, a daemon without the coordinator 501. Activity `ticket.done`
  (`Ticket TCK-n done: <title>`) and `ticket.blocked` (`Ticket TCK-n blocked: <first line>`),
  both with `runRef`.

- [ ] **Step 1: Write the failing tests**

In `internal/web/backend_test.go`, after `RemoveRun` of `fakeBackend`:

```go
func (b *fakeBackend) StopRun(_ context.Context, id string) error {
	b.mu.Lock()
	fr := b.runs[id]
	if fr == nil {
		b.mu.Unlock()
		return ErrNoRun
	}
	if !fr.run.Live {
		b.mu.Unlock()
		return ErrRunClosed
	}
	fr.run.Status, fr.run.Live, fr.run.Running = "closed", false, false
	run := fr.run
	b.mu.Unlock()
	b.announce(run)
	return nil
}
```

Append to `internal/web/api_runs_test.go`:

```go
func TestStoppingARunEndsItsSessionAndKeepsTheRecord(t *testing.T) {
	srv := newTestServer(t, Config{Backend: &fakeBackend{}})
	run := decode[Run](t, api(t, srv, http.MethodPost, "/api/runs", `{}`))
	if res := api(t, srv, http.MethodPost, "/api/runs/"+run.ID+"/stop", ""); res.StatusCode != http.StatusNoContent {
		t.Fatalf("stop = %d, want 204", res.StatusCode)
	}
	if got := decode[Run](t, api(t, srv, http.MethodGet, "/api/runs/"+run.ID, "")); got.Live || got.Status != "closed" {
		t.Errorf("after stop = %+v", got)
	}
	if res := api(t, srv, http.MethodPost, "/api/runs/"+run.ID+"/stop", ""); res.StatusCode != http.StatusConflict {
		t.Errorf("second stop = %d, want 409", res.StatusCode)
	}
	if res := api(t, srv, http.MethodPost, "/api/runs/RUN-99/stop", ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown run = %d, want 404", res.StatusCode)
	}
	res := api(t, srv, http.MethodGet, "/api/runs/"+run.ID+"/stop", "")
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET stop = %d, want 405", res.StatusCode)
	}
}
```

In `internal/web/api_tickets_test.go` add to the fake:

```go
func (b *ticketsBackend) RunTicket(_ context.Context, project, id string) (Run, error) {
	switch id {
	case "TCK-3":
		return Run{}, Conflict("TCK-3 is not runnable")
	case "TCK-7":
		return Run{}, ErrNoTicket
	}
	return Run{ID: "RUN-5", Mode: "autonomous", TicketID: id, Status: "open", Live: true}, nil
}

func (b *ticketsBackend) MergeTicket(_ context.Context, project, id string) (Ticket, error) {
	if id == "TCK-1" {
		return Ticket{}, Conflict("the main checkout has uncommitted changes")
	}
	return Ticket{ID: id, Status: "done"}, nil
}

func (b *ticketsBackend) Worktrees(_ context.Context, project string) ([]Worktree, error) {
	if project != "PRJ-1" {
		return nil, ErrNoProject
	}
	return nil, nil
}

func (b *ticketsBackend) DiscardWorktree(_ context.Context, project, name string) error {
	switch name {
	case "TCK-2":
		return Conflict("TCK-2 is worked on by the live run RUN-5; stop it first")
	case "TCK-9":
		return ErrNoWorktree
	}
	return nil
}
```

and the test:

```go
func TestTheTicketRunRoutesAnswerAndRefuse(t *testing.T) {
	srv, _ := newTicketsServer(t)
	res := api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets/TCK-2/run", "")
	if res.StatusCode != http.StatusCreated || decode[Run](t, res).TicketID != "TCK-2" {
		t.Errorf("run = %d", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets/TCK-3/run", "")
	if res.StatusCode != http.StatusConflict || !strings.Contains(readBody(t, res), "not runnable") {
		t.Errorf("a refused run = %d, want 409 with the sentence", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets/TCK-7/run", "")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown ticket = %d, want 404", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets/TCK-2/merge", "")
	if res.StatusCode != http.StatusOK || decode[Ticket](t, res).Status != "done" {
		t.Errorf("merge = %d", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets/TCK-1/merge", "")
	if res.StatusCode != http.StatusConflict || !strings.Contains(readBody(t, res), "uncommitted changes") {
		t.Errorf("a refused merge = %d, want 409", res.StatusCode)
	}
	res = api(t, srv, http.MethodGet, "/api/projects/PRJ-1/worktrees", "")
	if body := strings.TrimSpace(readBody(t, res)); res.StatusCode != http.StatusOK || body != "[]" {
		t.Errorf("worktrees = %d %s, want 200 []", res.StatusCode, body)
	}
	if res := api(t, srv, http.MethodGet, "/api/projects/PRJ-9/worktrees", ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown project = %d, want 404", res.StatusCode)
	}
	for name, want := range map[string]int{
		"TCK-1": http.StatusNoContent, "TCK-2": http.StatusConflict, "TCK-9": http.StatusNotFound,
	} {
		if res := api(t, srv, http.MethodDelete, "/api/projects/PRJ-1/worktrees/"+name, ""); res.StatusCode != want {
			t.Errorf("discard %s = %d, want %d", name, res.StatusCode, want)
		}
	}
	for path, method := range map[string]string{
		"/api/projects/PRJ-1/tickets/TCK-2/run":   http.MethodGet,
		"/api/projects/PRJ-1/tickets/TCK-2/merge": http.MethodGet,
		"/api/projects/PRJ-1/worktrees":           http.MethodPost,
		"/api/projects/PRJ-1/worktrees/TCK-1":     http.MethodGet,
	} {
		if res := api(t, srv, method, path, ""); res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", method, path, res.StatusCode)
		}
	}
}
```

Append to `cmd/aigem/webtickets_test.go` (add `"strings"` to its imports):

```go
func TestTicketRunsNeedTheCoordinatorAndRefuseWhatCannotRun(t *testing.T) {
	b, project, _ := ticketsBackend(t)
	ctx := context.Background()
	kid, _ := b.CreateTicket(ctx, project, web.NewTicket{Title: "kid"})
	ready := "ready"
	if _, err := b.UpdateTicket(ctx, project, kid.ID, web.TicketPatch{Status: &ready}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RunTicket(ctx, project, kid.ID); !errors.Is(err, web.ErrUnavailable) {
		t.Errorf("without the coordinator = %v, want ErrUnavailable", err)
	}

	runs, err := runner.NewRuns(runner.RunsConfig{
		Open: func(context.Context, runner.RunRequest) (*runner.Session, runner.Opened, error) {
			return nil, runner.Opened{}, errors.New("no model in this test")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)
	b.runs = runs
	b.ticketRuns = runner.NewTicketRuns(runner.TicketRunsConfig{
		Runs: runs, Tickets: b.tickets, Projects: b.projects, Finished: b.ticketFinished,
	})
	_, err = b.RunTicket(ctx, project, kid.ID)
	if !errors.Is(err, web.ErrConflict) || !strings.Contains(err.Error(), "not a git checkout") {
		t.Errorf("a project that is no checkout = %v, want a conflict", err)
	}
	if _, err := b.RunTicket(ctx, project, "TCK-9"); !errors.Is(err, web.ErrNoTicket) {
		t.Errorf("unknown ticket = %v", err)
	}
	if _, err := b.MergeTicket(ctx, project, kid.ID); !errors.Is(err, web.ErrConflict) {
		t.Errorf("merge of a ready ticket = %v, want a conflict", err)
	}
	if list, err := b.Worktrees(ctx, project); err != nil || len(list) != 0 {
		t.Errorf("worktrees = %v, %v", list, err)
	}
	if err := b.DiscardWorktree(ctx, project, kid.ID); !errors.Is(err, web.ErrNoWorktree) {
		t.Errorf("discard = %v, want ErrNoWorktree", err)
	}
	if err := b.StopRun(ctx, "RUN-9"); !errors.Is(err, web.ErrNoRun) {
		t.Errorf("stop of an unknown run = %v", err)
	}
}

func TestAFinishedTicketRunIsInTheActivityFeed(t *testing.T) {
	b, project, _ := ticketsBackend(t)
	b.ticketFinished(project, runner.TicketView{Ticket: runner.Ticket{
		ID: "TCK-1", Title: "notes", Status: runner.TicketDone, Runs: []string{"RUN-2"},
	}}, "Added.\n\nMerged aigem/TCK-1 into main as abc.")
	b.ticketFinished(project, runner.TicketView{Ticket: runner.Ticket{
		ID: "TCK-2", Status: runner.TicketBlocked, Runs: []string{"RUN-3"},
	}}, "the main checkout has uncommitted changes\n\nThe agent's summary: x")
	feed, _ := b.Activity(context.Background(), 0, 0)
	byKind := map[string]web.Activity{}
	for _, a := range feed {
		byKind[a.Kind] = a
	}
	if d := byKind["ticket.done"]; d.Text != "Ticket TCK-1 done: notes" || d.RunRef != "RUN-2" {
		t.Errorf("done = %+v", d)
	}
	if bl := byKind["ticket.blocked"]; bl.RunRef != "RUN-3" ||
		bl.Text != "Ticket TCK-2 blocked: the main checkout has uncommitted changes ..." {
		t.Errorf("blocked = %+v", bl)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/web/ ./cmd/aigem/ 2>&1 | head`
Expected: FAIL, `unknown field TicketID in struct literal of type Run` and
`b.ticketRuns undefined`.

- [ ] **Step 3: Write the implementation**

`internal/web/backend.go`: in `RunsBackend`, after `RemoveRun`:

```go
	// StopRun ends a live run's session and keeps its record and timeline. A run with no
	// session is ErrRunClosed.
	StopRun(ctx context.Context, id string) error
```

and in `Run`, after `ProjectID`:

```go
	TicketID  string    `json:"ticketId,omitempty"`
	Worktree  string    `json:"worktree,omitempty"`
	Branch    string    `json:"branch,omitempty"`
```

`internal/web/api_runs.go`: after `handleRemoveRun`:

```go
// handleStopRun ends a run's session and keeps its record.
func (s *Server) handleStopRun(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[RunsBackend](s, w, "runs")
	if !ok {
		return
	}
	if err := b.StopRun(r.Context(), r.PathValue("id")); err != nil {
		writeRunError(w, "stopping a run", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

and in `writeRunError`, after the `ErrNoTicket` case:

```go
	case errors.Is(err, ErrNoWorktree):
		http.Error(w, "no such worktree", http.StatusNotFound)
```

`internal/web/api_tickets.go`: add to `TicketsBackend`:

```go
	RunTicket(ctx context.Context, project, id string) (Run, error)
	MergeTicket(ctx context.Context, project, id string) (Ticket, error)
	Worktrees(ctx context.Context, project string) ([]Worktree, error)
	DiscardWorktree(ctx context.Context, project, name string) error
```

in `Ticket` after `Progress`:

```go
	MergePending bool            `json:"mergePending,omitempty"`
```

after `TicketPatch`:

```go
// Worktree is one aigem/<ticket> branch in a project's repository; state is running, kept or
// merged.
type Worktree struct {
	Repo   string `json:"repo"`
	Name   string `json:"name"`
	Path   string `json:"path,omitempty"`
	Ticket string `json:"ticket,omitempty"`
	Run    string `json:"run,omitempty"`
	State  string `json:"state"`
}
```

after `ErrNoTicket`:

```go
// ErrNoWorktree is returned for a worktree name the project does not hold.
var ErrNoWorktree = errors.New("web: no such worktree")
```

and at the end of the file:

```go
func (s *Server) handleRunTicket(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	run, err := b.RunTicket(r.Context(), r.PathValue("id"), r.PathValue("tid"))
	if err != nil {
		writeRunError(w, "running a ticket", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, run)
}

func (s *Server) handleMergeTicket(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	t, err := b.MergeTicket(r.Context(), r.PathValue("id"), r.PathValue("tid"))
	if err != nil {
		writeRunError(w, "merging a ticket", err)
		return
	}
	writeJSON(w, t)
}

func (s *Server) handleWorktrees(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	items, err := b.Worktrees(r.Context(), r.PathValue("id"))
	if err != nil {
		writeRunError(w, "listing worktrees", err)
		return
	}
	if items == nil {
		items = []Worktree{}
	}
	writeJSON(w, items)
}

func (s *Server) handleDiscardWorktree(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	if err := b.DiscardWorktree(r.Context(), r.PathValue("id"), r.PathValue("name")); err != nil {
		writeRunError(w, "discarding a worktree", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

`internal/web/server.go`, in `routes()`: after the `/api/runs/{id}/artifacts` pair:

```go
	s.api("POST /api/runs/{id}/stop", s.handleStopRun)
	s.mux.HandleFunc("/api/runs/{id}/stop", methodNotAllowed("POST"))
```

and after the `/api/projects/{id}/tickets/{tid}/comments` pair:

```go
	s.api("POST /api/projects/{id}/tickets/{tid}/run", s.handleRunTicket)
	s.mux.HandleFunc("/api/projects/{id}/tickets/{tid}/run", methodNotAllowed("POST"))
	s.api("POST /api/projects/{id}/tickets/{tid}/merge", s.handleMergeTicket)
	s.mux.HandleFunc("/api/projects/{id}/tickets/{tid}/merge", methodNotAllowed("POST"))
	s.api("GET /api/projects/{id}/worktrees", s.handleWorktrees)
	s.mux.HandleFunc("/api/projects/{id}/worktrees", methodNotAllowed("GET, HEAD"))
	s.api("DELETE /api/projects/{id}/worktrees/{name}", s.handleDiscardWorktree)
	s.mux.HandleFunc("/api/projects/{id}/worktrees/{name}", methodNotAllowed("DELETE"))
```

`cmd/aigem/webbackend.go`: add `_ web.TicketsBackend = (*webBackend)(nil)` to the `var` block
of interface assertions, and the field to `webBackend` after `tickets`:

```go
	// ticketRuns drives tickets with runs. It is set after construction, because its
	// Finished callback records activity through this backend.
	ticketRuns *runner.TicketRuns
```

replace `RemoveRun` with:

```go
func (b *webBackend) RemoveRun(_ context.Context, id string) error {
	if err := b.haveRuns(); err != nil {
		return err
	}
	remove := b.runs.Remove
	if b.ticketRuns != nil {
		remove = b.ticketRuns.Remove
	}
	if err := remove(id); err != nil {
		return webRunError(err)
	}
	// No RunRef: the run it would link to is gone.
	b.recordActivity(web.Activity{Kind: "run.removed", Text: "Deleted run " + id})
	return nil
}

func (b *webBackend) StopRun(_ context.Context, id string) error {
	if err := b.haveRuns(); err != nil {
		return err
	}
	stop := b.runs.Stop
	if b.ticketRuns != nil {
		stop = b.ticketRuns.Stop
	}
	return webRunError(stop(id))
}
```

and in `webRun` add `TicketID: v.TicketID, Worktree: v.Worktree, Branch: v.Branch,` to the
literal.

`cmd/aigem/webtickets.go`: append:

```go
func (b *webBackend) RunTicket(ctx context.Context, project, id string) (web.Run, error) {
	if err := b.ticketRunsReady(project); err != nil {
		return web.Run{}, err
	}
	v, err := b.ticketRuns.Start(ctx, project, id)
	if err != nil {
		return web.Run{}, ticketRunError(err)
	}
	return webRun(v), nil
}

func (b *webBackend) MergeTicket(ctx context.Context, project, id string) (web.Ticket, error) {
	if err := b.ticketRunsReady(project); err != nil {
		return web.Ticket{}, err
	}
	v, err := b.ticketRuns.Merge(ctx, project, id)
	if err != nil {
		return web.Ticket{}, ticketRunError(err)
	}
	return webTicket(v), nil
}

func (b *webBackend) Worktrees(ctx context.Context, project string) ([]web.Worktree, error) {
	if err := b.ticketRunsReady(project); err != nil {
		return nil, err
	}
	list, err := b.ticketRuns.Worktrees(ctx, project)
	if err != nil {
		return nil, ticketRunError(err)
	}
	out := make([]web.Worktree, 0, len(list))
	for _, w := range list {
		out = append(out, web.Worktree(w))
	}
	return out, nil
}

func (b *webBackend) DiscardWorktree(ctx context.Context, project, name string) error {
	if err := b.ticketRunsReady(project); err != nil {
		return err
	}
	return ticketRunError(b.ticketRuns.Discard(ctx, project, name))
}

// ticketFinished records a ticket a run left done or blocked in the activity feed.
func (b *webBackend) ticketFinished(_ string, v runner.TicketView, reason string) {
	a := web.Activity{Kind: "ticket.blocked", Text: "Ticket " + v.ID + " blocked: " + firstLine(reason)}
	if v.Status == runner.TicketDone {
		a = web.Activity{Kind: "ticket.done", Text: "Ticket " + v.ID + " done: " + v.Title}
	}
	if n := len(v.Runs); n > 0 {
		a.RunRef = v.Runs[n-1]
	}
	b.recordActivity(a)
}

func (b *webBackend) ticketRunsReady(project string) error {
	if err := b.ticketProject(project); err != nil {
		return err
	}
	if b.ticketRuns == nil {
		return web.ErrUnavailable
	}
	return nil
}

// ticketRunError classifies what the coordinator reports: ticket rules as conflicts, the
// rest the way a run's errors are.
func ticketRunError(err error) error {
	var refusal *runner.TicketRefusal
	switch {
	case errors.Is(err, runner.ErrNoWorktree):
		return web.ErrNoWorktree
	case errors.As(err, &refusal), errors.Is(err, runner.ErrNoTicket), errors.Is(err, runner.ErrNoProject):
		return webTicketError(err)
	default:
		return webRunError(err)
	}
}
```

and in `webTicket` add `MergePending: v.MergePending,` to the literal.

`cmd/aigem/webcmd.go`: right after `backend := newWebBackend(...)`:

```go
	if tickets != nil && projects != nil {
		backend.ticketRuns = runner.NewTicketRuns(runner.TicketRunsConfig{
			Runs: runs, Tickets: tickets, Projects: projects, Finished: backend.ticketFinished,
		})
		backend.ticketRuns.Recover()
		// Deferred after runs.Close, so it runs first: deliveries stop before the sessions do.
		defer backend.ticketRuns.Close()
	}
```

and in the signal branch, right before `runs.Close()`:

```go
		if backend.ticketRuns != nil {
			backend.ticketRuns.Close()
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test -race ./internal/web/... ./cmd/aigem/... 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
gofmt -w internal/web cmd/aigem
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./internal/web/... ./cmd/aigem/...
git add internal/web cmd/aigem
git commit -m "feat(web): run, stop, retry merge and worktree routes wired to ticket runs"
```

---

### Task 9: UI - ticket page actions, Runs tab, Stop on the run screen

**Files:**
- Modify: `internal/web/_ui/src/lib/wire.ts`, `src/lib/api.ts`
- Modify: `internal/web/_ui/src/screens/Task.tsx`, `src/screens/Run.tsx`
- Test: `internal/web/_ui/src/screens/tickets.test.tsx`, `src/screens/screens.test.tsx`

**Interfaces:**
- Produces: `Run.ticketId/worktree/branch?`, `Ticket.mergePending?`, `Worktree` type;
  `api.stopRun`, `api.runTicket`, `api.mergeTicket`, `api.worktrees`, `api.discardWorktree`.
  Ticket page: "Run" when `runnable`, "Stop" whenever the last run is live (also while
  blocked, to give back a run slot), "Retry merge" when
  `blocked` and `mergePending`; refusals in the page's alert. Runs tab: a list `Runs` of the
  ticket's runs, newest first, with state and a link to `/run/<id>`. Run screen: "Stop" while
  the run is live.

- [ ] **Step 1: Write the failing tests**

In `src/screens/tickets.test.tsx` change only two import lines and keep the rest
(`userEvent`, `vitest`, `navigate`, `selectProject, store` from `@/state/app` and
`DAEMON_PROJECT, mountApp, waitFor` from `@/test/harness`) as they are:

```ts
import { act, fireEvent, screen, within } from '@testing-library/react'
import type { Run, Ticket } from '@/lib/wire'
```

change `openTask` to take the runs:

```ts
async function openTask(
  id: string,
  tickets: Ticket[],
  routes: Record<string, () => Response | Promise<Response>> = {},
  runs: Run[] = [],
) {
  const h = await mountApp({
    meta: META_TICKETS,
    projects: [DAEMON_PROJECT, PRJ],
    path: `/task/${id}`,
    runs,
    routes: { '/api/projects/PRJ-1/tickets': () => json(tickets), ...routes },
  })
  act(() => selectProject('PRJ-1'))
  await screen.findByRole('heading', { level: 1, name: tickets.find((t) => t.id === id)!.title })
  return h
}
```

and append:

```ts
const RUN5: Run = {
  id: 'RUN-5', mode: 'autonomous', title: 'TCK-4: Runner change', status: 'open', live: true,
  running: true, ticketId: 'TCK-4', created: '2026-10-09T10:00:00Z', updated: '2026-10-09T10:00:00Z',
}

test('Run is offered only on a runnable ticket and starts it', async () => {
  const tickets = PLAN.map((t) => (t.id === 'TCK-4' ? { ...t, status: 'ready' as const, runnable: true } : t))
  const h = await openTask('TCK-4', tickets, {
    'POST /api/projects/PRJ-1/tickets/TCK-4/run': () => new Response(JSON.stringify(RUN5), { status: 201 }),
  })
  await userEvent.click(screen.getByRole('button', { name: 'Run' }))
  await waitFor(() =>
    expect(h.sent.some((s) => s.method === 'POST' && s.path === '/api/projects/PRJ-1/tickets/TCK-4/run')).toBe(true),
  )
  act(() => navigate({ screen: 'task', id: 'TCK-3' }))
  await screen.findByRole('heading', { level: 1, name: 'HTTP endpoint' })
  expect(screen.queryByRole('button', { name: 'Run' })).not.toBeInTheDocument()
})

test('a running ticket stops its run, and the runs tab links each run', async () => {
  const tickets = PLAN.map((t) => (t.id === 'TCK-4' ? { ...t, runs: ['RUN-5'] } : t))
  const h = await openTask(
    'TCK-4',
    tickets,
    { 'POST /api/runs/RUN-5/stop': () => new Response(null, { status: 204 }) },
    [RUN5],
  )
  await userEvent.click(screen.getByRole('button', { name: 'Stop' }))
  await waitFor(() => expect(h.sent.some((s) => s.path === '/api/runs/RUN-5/stop')).toBe(true))
  await userEvent.click(screen.getByRole('radio', { name: /Runs/ }))
  const list = screen.getByRole('list', { name: 'Runs' })
  expect(within(list).getByText('running')).toBeInTheDocument()
  await userEvent.click(within(list).getByRole('link', { name: 'RUN-5' }))
  expect(window.location.pathname).toBe('/run/RUN-5')
})

test('a ticket blocked on a merge offers Retry merge and Stop, and shows why it still cannot', async () => {
  const tickets = PLAN.map((t) =>
    t.id === 'TCK-4' ? { ...t, status: 'blocked' as const, mergePending: true, runs: ['RUN-5'] } : t,
  )
  await openTask(
    'TCK-4',
    tickets,
    {
      'POST /api/projects/PRJ-1/tickets/TCK-4/merge': () =>
        new Response('the main checkout has uncommitted changes', { status: 409 }),
    },
    [{ ...RUN5, running: false }],
  )
  expect(screen.getByRole('button', { name: 'Stop' })).toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'Retry merge' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('uncommitted changes')
  act(() => navigate({ screen: 'task', id: 'TCK-2' }))
  await screen.findByRole('heading', { level: 1, name: 'Journal helper' })
  expect(screen.queryByRole('button', { name: 'Retry merge' })).not.toBeInTheDocument()
})
```

In `src/screens/screens.test.tsx` replace the comment and test
`'the run screen has no stop button in this phase'` with:

```ts
test('the run screen stops a live run', async () => {
  const h = await mountApp({
    runs: [RUN],
    routes: { 'POST /api/runs/r-1/stop': () => new Response(null, { status: 204 }) },
  })
  act(() => navigate({ screen: 'run', id: 'r-1' }))
  await screen.findByRole('heading', { name: /Rotate the signing keys/ })
  await userEvent.click(screen.getByRole('button', { name: 'Stop' }))
  await waitFor(() => expect(h.sent.some((s) => s.method === 'POST' && s.path === '/api/runs/r-1/stop')).toBe(true))
})
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd internal/web/_ui && NODE_OPTIONS=--no-experimental-webstorage npx vitest run src/screens/tickets.test.tsx src/screens/screens.test.tsx 2>&1 | tail -20`
Expected: FAIL, no button named "Run", "Stop" or "Retry merge".

- [ ] **Step 3: Write the implementation**

`src/lib/wire.ts`: in `Run` after `projectId?: string`:

```ts
  /** Set on a run that works on a ticket. */
  ticketId?: string
  worktree?: string
  branch?: string
```

in `Ticket` after `progress`:

```ts
  /** A blocked ticket whose branch is committed and checked and only waits for the merge. */
  mergePending?: boolean
```

after `TicketPatch`:

```ts
/** An `aigem/<name>` branch in one of a project's repositories. */
export type Worktree = {
  repo: string
  name: string
  path?: string
  ticket?: string
  run?: string
  state: 'running' | 'kept' | 'merged'
}
```

`src/lib/api.ts`: add `Worktree` to the type import, and to `api`, after `removeRun`:

```ts
  stopRun: async (id: string, signal?: AbortSignal) => {
    await send(`/api/runs/${encodeURIComponent(id)}/stop`, { method: 'POST', signal })
  },
```

after `deleteTicket`:

```ts
  runTicket: (project: string, id: string, signal?: AbortSignal) =>
    json<Run>(`/api/projects/${encodeURIComponent(project)}/tickets/${encodeURIComponent(id)}/run`, {
      method: 'POST',
      signal,
    }),
  mergeTicket: (project: string, id: string, signal?: AbortSignal) =>
    json<Ticket>(`/api/projects/${encodeURIComponent(project)}/tickets/${encodeURIComponent(id)}/merge`, {
      method: 'POST',
      signal,
    }),
  worktrees: (project: string, signal?: AbortSignal) =>
    json<Worktree[]>(`/api/projects/${encodeURIComponent(project)}/worktrees`, { signal }),
  discardWorktree: async (project: string, name: string, signal?: AbortSignal) => {
    await send(`/api/projects/${encodeURIComponent(project)}/worktrees/${encodeURIComponent(name)}`, {
      method: 'DELETE',
      signal,
    })
  },
```

`src/screens/Task.tsx`:

1. Imports: `import type { Run, Ticket, TicketStatus } from '@/lib/wire'`.
2. Add `runs: s.runs,` to the object the `useApp` selector at the top returns (and `runs` to
   the destructuring), and after `const [adding, setAdding] = useState(false)` add
   `const [busy, setBusy] = useState(false)`.
3. After the `change` function add:

```tsx
  const act = async (call: () => Promise<unknown>) => {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await call()
      await Promise.all([refresh.tickets(), refresh.runs()])
    } catch (err) {
      setError(explain(err))
    } finally {
      setBusy(false)
    }
  }
  const lastRun = t.runs[t.runs.length - 1]
  const lastLive = runs.some((r) => r.id === lastRun && r.live)
```

4. Replace the `{!isParent && ( <div className="ml-auto flex gap-1.5"> ... </div> )}` block with:

```tsx
          {!isParent && (
            <div className="ml-auto flex gap-1.5">
              {t.runnable && (
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void act(() => api.runTicket(project, t.id))}
                  className={BUTTON}
                >
                  Run
                </button>
              )}
              {lastRun && lastLive && (
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void act(() => api.stopRun(lastRun))}
                  className={BUTTON}
                >
                  Stop
                </button>
              )}
              {t.status === 'blocked' && t.mergePending && (
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void act(() => api.mergeTicket(project, t.id))}
                  className={BUTTON}
                >
                  Retry merge
                </button>
              )}
              {personMoves(t.status).map((to) => (
                <button key={to} type="button" onClick={() => void change({ status: to })} className={BUTTON}>
                  {MOVE_LABEL[to]}
                </button>
              ))}
            </div>
          )}
```

5. Replace the Runs tab line (the `<p>` that says "Runs on tickets arrive in the next part.")
   with:

```tsx
          {tab === 'runs' && <TicketRuns ids={t.runs} />}
```

6. After the `TicketLink` component add:

```tsx
function runState(r?: Run): string {
  if (!r) return 'deleted'
  if (r.running) return 'running'
  return r.live ? 'live' : r.status
}

function TicketRuns({ ids }: { ids: string[] }) {
  const runs = useApp((s) => s.runs)
  if (ids.length === 0) return <p className="mt-3 text-fg-subtle">No runs yet. Run starts one on a runnable ticket.</p>
  return (
    <ul aria-label="Runs" className="m-0 mt-3 list-none p-0">
      {[...ids].reverse().map((id) => {
        const r = runs.find((x) => x.id === id)
        return (
          <li key={id} className="flex items-center gap-2.5 border-b border-line py-1.5">
            <a
              href={format({ screen: 'run', id })}
              onClick={(e) => {
                e.preventDefault()
                navigate({ screen: 'run', id })
              }}
              className="font-mono text-[0.75rem] text-fg-muted hover:text-fg"
            >
              {id}
            </a>
            <span className="flex-1 truncate">{r?.title ?? ''}</span>
            <span className="font-mono text-[0.71875rem] text-fg-subtle">{runState(r)}</span>
          </li>
        )
      })}
    </ul>
  )
}
```

`src/screens/Run.tsx`:

1. Replace the paragraph of the component's doc comment that starts "There is no \"Stop run\"
   button" (three lines) with: ` * "Stop" ends the session and keeps the record and the timeline.`
2. Imports: add `import { api } from '@/lib/api'` and change the app import to
   `import { explain, refresh, setBanner, useApp } from '@/state/app'`.
3. After `const [agent, setAgent] = useState('root')` add:

```tsx
  const [stopping, setStopping] = useState(false)
  const stop = async () => {
    setStopping(true)
    try {
      await api.stopRun(runId)
      await refresh.runs()
    } catch (err) {
      setBanner(explain(err))
    } finally {
      setStopping(false)
    }
  }
```

4. After the Interrupt `<button>` add:

```tsx
            <button
              type="button"
              disabled={!record.live || stopping}
              onClick={() => void stop()}
              className="h-6.5 rounded-md border border-line px-2.5 text-[0.78125rem] text-fg-muted enabled:hover:border-line-strong enabled:hover:text-fg disabled:opacity-50"
            >
              Stop
            </button>
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd internal/web/_ui && npm run lint && npm run check && NODE_OPTIONS=--no-experimental-webstorage npx vitest run`
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add internal/web/_ui/src/lib/wire.ts internal/web/_ui/src/lib/api.ts \
  internal/web/_ui/src/screens/Task.tsx internal/web/_ui/src/screens/Run.tsx \
  internal/web/_ui/src/screens/tickets.test.tsx internal/web/_ui/src/screens/screens.test.tsx
git commit -m "feat(web): Run, Stop and Retry merge on the ticket page, Runs tab, Stop on runs"
```

---

### Task 10: UI - Worktrees screen

**Files:**
- Modify: `internal/web/_ui/src/screens/Worktrees.tsx` (whole file below)
- Modify: `internal/web/_ui/src/test/harness.tsx` (default stub for the worktrees route)
- Test: `internal/web/_ui/src/screens/projects.test.tsx`

**Interfaces:**
- Consumes: `api.worktrees`, `api.discardWorktree`, `Worktree` (Task 9), `Modal`, `DataGrid`.
- Produces: grid `Repositories` (its Worktrees column counts the repository's branches) and,
  when the daemon serves `tickets`, grid `Worktrees` with Branch, State, Worktree path and the
  actions "Open run", "Open ticket", "Discard" (disabled while running, confirmed by a dialog
  `Discard <name>?`). The list is read again when the project's tickets change and after a
  discard.

- [ ] **Step 1: Write the failing tests**

In `src/test/harness.tsx`, in the fetch stub after the `repos` line:

```ts
      if (/^\/api\/projects\/[^/]+\/worktrees$/.test(path)) return Promise.resolve(ok([]))
```

In `src/screens/projects.test.tsx`: import `META` from `@/test/harness` as well, change the
last assertion of `'the worktrees screen lists the repositories of the chosen project'` to
`expect(within(grid).getAllByText('no worktrees')).toHaveLength(2)`, and append:

```ts
test('the worktrees screen lists the aigem branches and discards one after asking', async () => {
  const user = userEvent.setup()
  let trees = [
    { repo: '', name: 'TCK-4', path: '/home/dev/work/.aigem/worktrees/TCK-4', ticket: 'TCK-4', run: 'RUN-5', state: 'running' },
    { repo: '', name: 'TCK-2', ticket: 'TCK-2', run: 'RUN-3', state: 'merged' },
    { repo: '', name: 'TCK-3', path: '/home/dev/work/.aigem/worktrees/TCK-3', ticket: 'TCK-3', run: 'RUN-4', state: 'kept' },
  ]
  const h = await mountApp({
    meta: { features: { ...META.features, tickets: true } },
    projects: PROJECTS,
    routes: {
      '/api/projects/PRJ-1/repos': () =>
        new Response(JSON.stringify([{ name: '', dir: '/home/dev/work', main: 'main' }]), { status: 200 }),
      '/api/projects/PRJ-1/worktrees': () => new Response(JSON.stringify(trees), { status: 200 }),
      'DELETE /api/projects/PRJ-1/worktrees/TCK-3': () => {
        trees = trees.filter((w) => w.name !== 'TCK-3')
        return new Response(null, { status: 204 })
      },
    },
  })
  act(() => navigate({ screen: 'repos' }))
  await screen.findByText(/no project record/)
  await user.click(within(screen.getByRole('list', { name: 'Projects' })).getByRole('button', { name: /work/ }))

  const grid = await screen.findByRole('grid', { name: 'Worktrees' })
  expect(await within(grid).findByText('aigem/TCK-4')).toBeInTheDocument()
  expect(within(grid).getByText('removed')).toBeInTheDocument()
  expect(within(screen.getByRole('grid', { name: 'Repositories' })).getByText('3 worktrees')).toBeInTheDocument()
  expect(within(grid).getByRole('button', { name: 'Discard aigem/TCK-4' })).toBeDisabled()

  await user.click(within(grid).getByRole('button', { name: 'Discard aigem/TCK-3' }))
  const dialog = await screen.findByRole('dialog', { name: 'Discard TCK-3?' })
  await user.click(within(dialog).getByRole('button', { name: 'Discard' }))
  await waitFor(() =>
    expect(h.sent.some((s) => s.method === 'DELETE' && s.path === '/api/projects/PRJ-1/worktrees/TCK-3')).toBe(true),
  )
  await waitFor(() => expect(screen.queryByText('aigem/TCK-3')).not.toBeInTheDocument())

  await user.click(within(screen.getByRole('grid', { name: 'Worktrees' })).getByRole('button', { name: 'Open run RUN-5' }))
  expect(window.location.pathname).toBe('/run/RUN-5')
})
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd internal/web/_ui && NODE_OPTIONS=--no-experimental-webstorage npx vitest run src/screens/projects.test.tsx 2>&1 | tail -20`
Expected: FAIL, no grid named "Worktrees" and no text "no worktrees".

- [ ] **Step 3: Write the implementation**

`src/screens/Worktrees.tsx`:

```tsx
import { useEffect, useState } from 'react'
import { api } from '@/lib/api'
import { navigate } from '@/lib/route'
import type { Repository, Worktree } from '@/lib/wire'
import { currentProject, explain, flash, setBanner, useApp } from '@/state/app'
import { DataGrid } from '@/ui/DataGrid'
import type { Column } from '@/ui/DataGrid'
import { EmptyState } from '@/ui/EmptyState'
import { Modal } from '@/ui/Modal'

const BUTTON =
  'h-6 rounded-md border border-line px-2 text-[0.75rem] text-fg-muted enabled:hover:border-line-strong enabled:hover:text-fg disabled:opacity-50'

function count(n: number): string {
  return n === 0 ? 'no worktrees' : `${n} worktree${n === 1 ? '' : 's'}`
}

/**
 * A project's repositories, with the branch a run merges into, and the `aigem/*` branches
 * ticket runs left in them.
 */
export function Worktrees() {
  const { project, name, tickets, served } = useApp((s) => ({
    project: s.project,
    name: currentProject(s)?.name ?? '',
    tickets: s.tickets,
    served: s.meta?.features?.tickets === true,
  }))
  const [repos, setRepos] = useState<{ project: string; items: Repository[] } | null>(null)
  const [trees, setTrees] = useState<{ project: string; items: Worktree[] } | null>(null)
  const [discarding, setDiscarding] = useState<Worktree | null>(null)
  const [reload, setReload] = useState(0)

  useEffect(() => {
    if (!project) return
    const abort = new AbortController()
    void api
      .projectRepos(project, abort.signal)
      .then((items) => setRepos({ project, items }))
      .catch((err: unknown) => {
        if (abort.signal.aborted) return
        setBanner(explain(err))
        // An answer, so the screen stops saying it is still reading.
        setRepos({ project, items: [] })
      })
    return () => abort.abort()
  }, [project])

  // Read again when the project's tickets change: a run starting or finishing is what moves
  // a worktree.
  useEffect(() => {
    if (!project || !served) return
    const abort = new AbortController()
    void api
      .worktrees(project, abort.signal)
      .then((items) => setTrees({ project, items }))
      .catch((err: unknown) => {
        if (!abort.signal.aborted) setBanner(explain(err))
      })
    return () => abort.abort()
  }, [project, served, tickets, reload])

  const shown = trees?.project === project ? trees.items : []
  const discard = async (w: Worktree) => {
    setDiscarding(null)
    try {
      await api.discardWorktree(project, w.name)
      flash(`Discarded aigem/${w.name}`)
      setReload((n) => n + 1)
    } catch (err) {
      setBanner(explain(err))
    }
  }

  const columns: Column<Repository>[] = [
    { key: 'name', header: 'Repository', width: '12.5rem', cell: (r) => r.name || `${name} (the project itself)` },
    { key: 'main', header: 'Main branch', width: '8.75rem', cell: (r) => r.main || 'neither main nor master' },
    { key: 'dir', header: 'Directory', width: 'minmax(12.5rem, 1fr)', cell: (r) => r.dir },
    {
      key: 'worktrees',
      header: 'Worktrees',
      width: '8.75rem',
      cell: (r) => count(shown.filter((w) => w.repo === r.name).length),
    },
  ]
  const treeColumns: Column<Worktree>[] = [
    { key: 'repo', header: 'Repository', width: '10rem', cell: (w) => w.repo || name },
    { key: 'branch', header: 'Branch', width: '9rem', cell: (w) => `aigem/${w.name}` },
    { key: 'state', header: 'State', width: '5.5rem', cell: (w) => w.state },
    { key: 'path', header: 'Worktree', width: 'minmax(12.5rem, 1fr)', cell: (w) => w.path || 'removed' },
    {
      key: 'actions',
      header: 'Actions',
      width: '17rem',
      cell: (w) => (
        <span className="flex gap-1.5">
          {w.run && (
            <button
              type="button"
              aria-label={`Open run ${w.run}`}
              onClick={() => navigate({ screen: 'run', id: w.run })}
              className={BUTTON}
            >
              Open run
            </button>
          )}
          {w.ticket && (
            <button
              type="button"
              aria-label={`Open ticket ${w.ticket}`}
              onClick={() => navigate({ screen: 'task', id: w.ticket })}
              className={BUTTON}
            >
              Open ticket
            </button>
          )}
          <button
            type="button"
            aria-label={`Discard aigem/${w.name}`}
            disabled={w.state === 'running'}
            onClick={() => setDiscarding(w)}
            className={BUTTON}
          >
            Discard
          </button>
        </span>
      ),
    },
  ]

  const loaded = repos?.project === project ? repos.items : null
  return (
    <>
      <div className="flex-none border-b border-line px-4.5 pt-3.5 pb-3">
        <h1 className="m-0 text-[1.0625rem] font-semibold tracking-[-0.015em]">Repositories & worktrees</h1>
      </div>
      {!project ? (
        <EmptyState
          title="This daemon's directory has no project record."
          detail="Choose or add a project in the sidebar to list its repositories and worktrees."
        />
      ) : loaded === null ? (
        <p className="px-4.5 py-4 text-[0.8125rem] text-fg-subtle">Reading the repositories…</p>
      ) : (
        <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
          <DataGrid
            label="Repositories"
            columns={columns}
            rows={loaded}
            rowKey={(r) => r.dir}
            minWidth={680}
            empty={
              <EmptyState
                title="No repositories."
                detail={`${name} holds no git checkout, at its root or one level down.`}
              />
            }
          />
          {served && (
            <>
              <h2 className="m-0 border-y border-line px-4.5 py-2 text-[0.6875rem] font-semibold tracking-[.07em] text-fg-subtle uppercase">
                Worktrees
              </h2>
              <DataGrid
                label="Worktrees"
                columns={treeColumns}
                rows={shown}
                rowKey={(w) => `${w.repo}/${w.name}`}
                minWidth={680}
                empty={
                  <EmptyState
                    title="No worktrees."
                    detail="Running a ticket makes one; its branch stays after the merge."
                  />
                }
              />
            </>
          )}
        </div>
      )}
      {discarding && (
        <Modal
          title={`Discard ${discarding.name}?`}
          subtitle={`aigem/${discarding.name}`}
          onClose={() => setDiscarding(null)}
          confirm={{ label: 'Discard', danger: true, onClick: () => void discard(discarding) }}
        >
          The worktree and the branch are deleted. Work that was not merged into main is lost.
        </Modal>
      )}
    </>
  )
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd internal/web/_ui && npm run lint && npm run check && NODE_OPTIONS=--no-experimental-webstorage npx vitest run`
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add internal/web/_ui/src/screens/Worktrees.tsx internal/web/_ui/src/test/harness.tsx \
  internal/web/_ui/src/screens/projects.test.tsx
git commit -m "feat(web): worktrees screen lists and discards aigem branches"
```

---

### Task 11: Docs, full check, manual check

**Files:**
- Modify: `docs/web.md` (routes table, a "Runs on tickets" section, activity kinds, the
  "screens" paragraph)
- Modify: `CHANGELOG.md` (one line under Unreleased/Added)

- [ ] **Step 1: Document**

In `docs/web.md`:

- Routes table, after the `DELETE /api/projects/{id}/tickets/{tid}` row:

```markdown
| `POST /api/projects/{id}/tickets/{tid}/run` | starts a run on a runnable ticket; 201 with the run, 409 with why not |
| `POST /api/projects/{id}/tickets/{tid}/merge` | retries the merge of a ticket blocked on one; 200 with the ticket, 409 |
| `GET /api/projects/{id}/worktrees` | the `aigem/*` branches: `repo`, `name`, `path`, `ticket`, `run`, `state` |
| `DELETE /api/projects/{id}/worktrees/{name}` | removes the worktree and its branch; 204, 409 while its run is live |
```

  and after the `DELETE /api/runs/{id}` row (or the last `/api/runs/...` row):

```markdown
| `POST /api/runs/{id}/stop` | interrupts the turn and ends the session; the record stays; 204 |
```

- After the "Tickets" section add:

```markdown
## Runs on tickets

"Run" on a runnable ticket creates the worktree `<project dir>/.aigem/worktrees/<TCK-n>` on a
new branch `aigem/<TCK-n>` from `main` (or `master`) in the ticket's repository, and opens an
autonomous run whose tools are rooted there. A worktree and branch an earlier run left are
reused, so a ticket blocked by a restart continues where it stopped. The daemon adds
`/.aigem/worktrees/` to the repository's `info/exclude` once. The run's first message is the
ticket, the discussion and the rule: call `ticket_done` with a summary when the work is
complete.

The agent's tools are rooted at the worktree; that is all the sandbox does. The repository's
check runs code the agent wrote there (tests, build scripts) with the daemon's rights and no
sandbox, so review what a ticket run may touch before you give a repository a check.

At the end of every turn the daemon decides. Without `ticket_done` the ticket is `blocked`
with the agent's last message. With it, the daemon commits the worktree as
`aigem: <title> (TCK-n, RUN-m)`, runs the check from `.aigem/project.json` in the main
checkout (`{"check": "make test"}`, 15 minutes, the last 4 KiB of output on failure) and
merges with `--no-ff` in the repository's own checkout - only when it is clean and on `main`,
one merge per repository at a time; a merge that has begun is never cancelled. This happens
only while the ticket is `running` under that run. A turn that ends interrupted or with an
error is not done, even after `ticket_done`. Then the ticket is `done`, the run is stopped,
the worktree is removed (kept, and the comment says so, when it has changes) and the branch is
kept. A blocked merge sets `mergePending`, and "Retry
merge" repeats it. Nothing is pushed.

A blocked ticket keeps its run: typing into it moves the ticket back to `running`. Stopping
the run blocks the ticket with "stopped by a person", deleting it with "the run was deleted",
and a daemon restart blocks every running ticket with "the daemon restarted". The worktree
stays in all three; "Run" reuses it and the Worktrees screen discards it. "Stop" is offered
whenever the ticket's last run is live, because ticket runs share the daemon's limit on live
runs with chats. Activity: `ticket.done` and
`ticket.blocked`.
```

- In the opening "screens" paragraph replace "Tasks are drawn as an empty state that says what
  it is waiting for: it arrives with the next phase." with "A runnable ticket can be run; the
  worktrees screen lists the branches runs left behind."

In `CHANGELOG.md` under `## [Unreleased]` / `### Added` add:

```markdown
- Web UI: runs on tickets (part 2 of agent tickets). "Run" gives a ticket a git worktree and
  an autonomous run; `ticket_done` makes the daemon commit, check and merge into `main`;
  anything else blocks the ticket with a reason. Stop, Retry merge and a Worktrees list.
```

Max 100 characters per prose line; table rows may be longer, as the existing rows are.

- [ ] **Step 2: Full check**

```bash
go build ./... && go test -race ./... 2>&1 | grep -v '^ok' | head -20
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./...
cd internal/web/_ui && npm run lint && npm run check && \
  NODE_OPTIONS=--no-experimental-webstorage npx vitest run && cd -
make web && make build
```

Expected: only the known macOS failures and the old QF1003.

- [ ] **Step 3: Manual check in the browser**

Start `bin/aigem web --addr 127.0.0.1:7799` (stop an older one on that port first). Add a
throwaway git repository with a `main` branch as a project, add
`.aigem/project.json` `{"check": "true"}` and commit it. Create a ticket, mark it ready, press
"Run", watch the run reach `done` and the merge commit appear on `main`. Create a second
ticket, leave an untracked file in the checkout, run it: the ticket is blocked with "the main
checkout has uncommitted changes"; remove the file and press "Retry merge". Run a third, press
"Stop" on the ticket page, then Discard its worktree on the Worktrees screen.

- [ ] **Step 4: Commit**

```bash
git add docs/web.md CHANGELOG.md
git commit -m "docs: runs on tickets"
```
