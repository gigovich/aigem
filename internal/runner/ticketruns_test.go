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
	"github.com/gigovich/aigem/internal/uisession"
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

func TestTicketDoneRecordsTheLastSummary(t *testing.T) {
	tr := &ticketRun{}
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() { tr.record(fmt.Sprint(i)) })
	}
	wg.Wait()
	if got := tr.summary.Load(); got == nil || len(*got) == 0 || len(*got) > 2 {
		t.Fatalf("after concurrent calls the summary is %v", got)
	}
	tr.record("first")
	tr.record("second")
	if got := tr.summary.Load(); got == nil || *got != "second" {
		t.Errorf("summary = %v, want the last one", got)
	}
}

func (f *fixture) ticketRun(run string) *ticketRun {
	f.t.Helper()
	f.tr.mu.Lock()
	defer f.tr.mu.Unlock()
	tr := f.tr.byRun[run]
	if tr == nil {
		f.t.Fatalf("no ticket run for %s", run)
	}
	return tr
}

func TestADuplicateTurnEndChangesNothing(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	writeFile(t, f.repo, "scratch.txt", "mine\n")
	id := f.ready("twice")
	f.script.then(edit(filepath.Join(f.worktree(id), "a.txt"), "a\n", done("Wrote a.")), say("ok"))
	v := f.start(id)
	tr := f.ticketRun(v.ID)
	got := f.next(id, 0)
	if !got.MergePending {
		t.Fatalf("ticket = %s %q, want blocked on the merge", got.Status, lastComment(got))
	}
	f.idle(v.ID)

	if err := os.Remove(filepath.Join(f.repo, "scratch.txt")); err != nil {
		t.Fatal(err)
	}
	f.tr.onTurn(tr, uisession.Event{Kind: uisession.KindTurnEnd, Text: "ok"})
	tr.record("Wrote a again.")
	f.tr.onTurn(tr, uisession.Event{Kind: uisession.KindTurnEnd, Text: "ok"})
	again, _ := f.tickets.Get(f.project, id)
	if again.Status != TicketBlocked || len(again.Comments) != 1 {
		t.Errorf("a duplicate turn end left %s with %d comments", again.Status, len(again.Comments))
	}
	if runGit(t, f.repo, "rev-list", "--count", "main") != "1" {
		t.Error("a duplicate turn end merged")
	}

	merged := f.ready("merged")
	f.script.then(edit(filepath.Join(f.worktree(merged), "b.txt"), "b\n", done("Wrote b.")), say("ok"))
	v = f.start(merged)
	tr = f.ticketRun(v.ID)
	if got := f.next(merged, 0); got.Status != TicketDone {
		t.Fatalf("ticket = %s %q", got.Status, lastComment(got))
	}
	head := runGit(t, f.repo, "rev-parse", "main")
	tr.record("Wrote b again.")
	f.tr.onTurn(tr, uisession.Event{Kind: uisession.KindTurnEnd, Text: "ok"})
	if again, _ := f.tickets.Get(f.project, merged); len(again.Comments) != 1 {
		t.Errorf("a duplicate turn end wrote %q", lastComment(again))
	}
	if runGit(t, f.repo, "rev-parse", "main") != head {
		t.Error("a duplicate turn end merged twice")
	}
}
