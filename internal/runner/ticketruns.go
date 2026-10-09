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
}

func (tr *ticketRun) record(summary string) { tr.summary.Store(&summary) }

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
		Tools:  []tools.Tool{newTicketDone(tr.record)},
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
		t.finish(project, id, v.ID, TicketBlocked, "the ticket could not be sent to the run: "+err.Error(), false)
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
			n := len(v.Runs)
			if v.Status != TicketRunning || v.Progress != nil || n == 0 || t.live(v.Runs[n-1]) {
				continue
			}
			t.finish(p.ID, v.ID, v.Runs[n-1], TicketBlocked, "the daemon restarted", false)
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
		if _, err := t.tickets.Start(tr.project, tr.ticket, tr.run); err != nil {
			slog.Warn("a ticket run's new turn could not take its ticket back", "ticket", tr.ticket, "err", err)
		}
	case uisession.KindTurnEnd:
		summary := tr.summary.Swap(nil)
		// Only a turn that holds its ticket running decides; a turn that could not take the ticket
		// back, or a turn_end delivered twice, finds it in another state.
		tk, err := t.tickets.Get(tr.project, tr.ticket)
		switch {
		case err != nil || tk.Status != TicketRunning || !lastRun(tk.Ticket, tr.run):
		case summary == nil || ev.Interrupted || ev.Error != "":
			t.finish(tr.project, tr.ticket, tr.run, TicketBlocked, lastWords(ev), false)
		default:
			t.deliver(tr, tk.Title, *summary)
		}
	}
}

// deliver commits what the run left in its worktree, runs the repository's check and merges.
// It is called with tr.mu held. A cancelled tr.ctx means the run was stopped, deleted or the
// daemon is closing: nothing more is written, except that a merge that has begun finishes.
func (t *TicketRuns) deliver(tr *ticketRun, title, summary string) {
	ctx, pl := tr.ctx, tr.place
	block := func(reason string, merge bool) {
		if ctx.Err() == nil {
			t.finish(tr.project, tr.ticket, tr.run, TicketBlocked, reason, merge)
		}
	}
	msg := fmt.Sprintf("aigem: %s (%s, %s)", title, tr.ticket, tr.run)
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
	t.finish(project, id, run, TicketDone, comment, false)
}

func (t *TicketRuns) finish(project, id, run, status, comment string, mergePending bool) {
	v, err := t.tickets.Finish(project, id, run, status, comment, mergePending)
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
