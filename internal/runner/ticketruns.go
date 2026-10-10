package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gigovich/aigem/internal/gitx"
	"github.com/gigovich/aigem/internal/tools"
	"github.com/gigovich/aigem/internal/uisession"
)

const ticketRule = "Work in this worktree. When the work is complete, call ticket_done with a short " +
	"summary. If you are stuck or need a decision, explain why and stop without calling it."

var ErrNoWorktree = errors.New("runner: no such worktree")

// closeTurnsWait bounds how long Close waits for the deliveries it cancelled.
const closeTurnsWait = 30 * time.Second

// Worktree is one aigem/<ticket> branch in a project's repository. State is running while
// the ticket's run is live, merged once the branch is in main and its worktree gone, and kept
// otherwise.
type Worktree struct {
	Repo, Name, Path, Ticket, Run, State string
}

type TicketRunsConfig struct {
	Runs     *Runs
	Tickets  *Tickets
	Projects *Projects
	// Finished is told when a run leaves a ticket done or blocked, with the comment it wrote.
	Finished func(project string, v TicketView, reason string)
	// Planned is told when a planner run's ticket reaches review, with the comment it wrote.
	Planned func(project string, v TicketView, comment string)
}

// TicketRuns drives tickets with autonomous runs: a worktree per ticket, a run inside it, and
// the decision taken at the end of each of its turns.
type TicketRuns struct {
	runs     *Runs
	tickets  *Tickets
	projects *Projects
	finished func(string, TicketView, string)
	planned  func(string, TicketView, string)

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
	// plan marks a planner run: its turn end sends the ticket to review.
	plan    bool
	place   ticketPlace
	summary atomic.Pointer[string]
	ctx     context.Context
	cancel  context.CancelFunc

	mu   sync.Mutex
	gone bool
}

func (tr *ticketRun) record(summary string) { tr.summary.Store(&summary) }

type ticketPlace struct{ dir, repo, main, worktree, branch string }

func NewTicketRuns(cfg TicketRunsConfig) *TicketRuns {
	t := &TicketRuns{
		runs: cfg.Runs, tickets: cfg.Tickets, projects: cfg.Projects, finished: cfg.Finished,
		planned: cfg.Planned,
		byRun:   map[string]*ticketRun{}, starting: map[string]bool{}, merging: map[string]*sync.Mutex{},
	}
	if t.finished == nil {
		t.finished = func(string, TicketView, string) {}
	}
	if t.planned == nil {
		t.planned = func(string, TicketView, string) {}
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
	case len(tk.Runs) > 0 && t.live(tk.Runs[len(tk.Runs)-1]):
		return RunView{}, refuse("%s has a live run %s; stop it first", id, tk.Runs[len(tk.Runs)-1])
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
	if isCheckout(pl.dir) {
		if err := gitx.Exclude(ctx, pl.dir, "/.aigem/worktrees/"); err != nil {
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

// Recover blocks the tickets a previous daemon left running and sends the plans it left planning
// to review: their runs did not survive it.
func (t *TicketRuns) Recover() {
	for _, p := range t.projects.List() {
		views, err := t.tickets.List(p.ID)
		if err != nil {
			slog.Warn("a project's tickets could not be read on start", "project", p.ID, "err", err)
			continue
		}
		for _, v := range views {
			n := len(v.Runs)
			if n == 0 || t.live(v.Runs[n-1]) {
				continue
			}
			switch {
			case v.Status == TicketRunning && v.Progress == nil:
				t.finish(p.ID, v.ID, v.Runs[n-1], TicketBlocked, "the daemon restarted", false)
			case v.Status == TicketPlanning:
				t.review(p.ID, v.ID, v.Runs[n-1], "the daemon restarted")
			}
		}
	}
}

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
	tr := t.release(run)
	if tr == nil {
		return false
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.gone {
		return true
	}
	tr.gone = true
	v, err := t.tickets.Get(tr.project, tr.ticket)
	switch {
	case err != nil:
	case tr.plan && (v.Status == TicketPlanning || v.Status == TicketReview):
		t.review(tr.project, tr.ticket, run, reason)
	case !tr.plan && (v.Status == TicketRunning || v.Status == TicketBlocked):
		t.finish(tr.project, tr.ticket, run, TicketBlocked, reason, v.MergePending)
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
	waiting := func(v TicketView) bool { return v.Status == TicketBlocked && v.MergePending && len(v.Runs) > 0 }
	if !waiting(v) {
		return TicketView{}, refuse("%s is not waiting for a merge", id)
	}
	run := v.Runs[len(v.Runs)-1]
	pl, err := t.place(ctx, project, v.Ticket)
	if err != nil {
		return TicketView{}, err
	}
	sha, reason := t.merge(ctx, pl, func() bool {
		v, err := t.tickets.Get(project, id)
		return err == nil && waiting(v) && lastRun(v.Ticket, run)
	})
	switch {
	case reason != "":
		return TicketView{}, refuse("%s", reason)
	case sha == "":
		return TicketView{}, refuse("%s is not waiting for a merge", id)
	}
	if tr := t.release(run); tr != nil {
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
	if v, err := t.tickets.Get(project, w.Ticket); err == nil && v.Status == TicketBlocked && v.MergePending {
		return refuse("%s is waiting for a merge; close the ticket or retry the merge first", name)
	}
	repo := filepath.Join(pv.Dir, w.Repo)
	if w.Path != "" {
		if err := gitx.WorktreeRemove(ctx, repo, w.Path, true); err != nil {
			return err
		}
	}
	return gitx.BranchDelete(ctx, repo, "aigem/"+name)
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
		start := t.tickets.Start
		if tr.plan {
			start = t.tickets.StartPlan
		}
		if _, err := start(tr.project, tr.ticket, tr.run); err != nil {
			slog.Warn("a ticket run's new turn could not take its ticket back", "ticket", tr.ticket, "err", err)
		}
	case uisession.KindTurnEnd:
		summary := tr.summary.Swap(nil)
		// Only a turn that holds its ticket running (planning, for a planner) decides; a turn that
		// could not take the ticket back, or a turn_end delivered twice, finds it in another state.
		tk, err := t.tickets.Get(tr.project, tr.ticket)
		switch {
		case err != nil || !lastRun(tk.Ticket, tr.run):
		case tr.plan:
			if tk.Status == TicketPlanning {
				t.review(tr.project, tr.ticket, tr.run, planWords(ev, summary))
			}
		case tk.Status != TicketRunning:
		case summary == nil || ev.Interrupted || ev.Error != "":
			t.finish(tr.project, tr.ticket, tr.run, TicketBlocked, lastWords(ev), false)
		default:
			t.deliver(tr, tk.Title, *summary)
		}
	}
}

// deliver commits what the run left in its worktree, runs the repository's check and merges.
// It is called with tr.mu held. A cancelled tr.ctx means the run was stopped, deleted or the
// daemon is closing: nothing more is written, except that a commit or a merge that has begun
// finishes.
func (t *TicketRuns) deliver(tr *ticketRun, title, summary string) {
	ctx, pl := tr.ctx, tr.place
	block := func(reason string, merge bool) {
		if ctx.Err() == nil {
			t.finish(tr.project, tr.ticket, tr.run, TicketBlocked, reason, merge)
		}
	}
	msg := fmt.Sprintf("aigem: %s (%s, %s)", title, tr.ticket, tr.run)
	if _, err := gitx.CommitAll(context.WithoutCancel(ctx), pl.worktree, msg); err != nil {
		block("could not commit the worktree: "+err.Error(), false)
		return
	}
	if reason := checkWorktree(ctx, pl); reason != "" {
		block(reason, false)
		return
	}
	sha, reason := t.merge(ctx, pl, func() bool {
		tk, err := t.tickets.Get(tr.project, tr.ticket)
		return ctx.Err() == nil && err == nil && tk.Status == TicketRunning && lastRun(tk.Ticket, tr.run)
	})
	if reason != "" {
		block(reason+"\n\nThe agent's summary: "+summary, true)
		return
	}
	if sha == "" {
		return
	}
	tr.gone = true
	t.release(tr.run)
	t.complete(tr.project, tr.ticket, pl, summary+"\n\n"+merged(pl, sha), tr.run)
}

// merge brings the ticket's branch into main in the repository's own checkout, one merge per
// repository at a time. Once begun it is not cancelled - a merge killed half-way would leave
// the person's checkout mid-merge - and each git command is bounded by gitx.Timeout instead.
// It merges nothing and returns no reason when still, asked under the lock, says the ticket
// no longer waits for this merge.
func (t *TicketRuns) merge(ctx context.Context, pl ticketPlace, still func() bool) (string, string) {
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
	if !still() {
		return "", ""
	}

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

// release lets go of a run's ticket run and cancels what it has in flight.
func (t *TicketRuns) release(run string) *ticketRun {
	t.mu.Lock()
	tr := t.byRun[run]
	delete(t.byRun, run)
	t.mu.Unlock()
	if tr != nil {
		tr.cancel()
	}
	return tr
}

func (t *TicketRuns) live(run string) bool {
	v, err := t.runs.Get(run)
	return err == nil && v.Live
}

// place is where a ticket's work happens: its project directory, its repository, the branch it
// merges into, its worktree and its branch.
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
		dir: pv.Dir, repo: repo, main: gitx.MainBranch(ctx, repo),
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

func ticketPrompt(tk Ticket) string { return ticketText(tk) + ticketRule }

// ticketText is a ticket as a run reads it: its title, body and discussion.
func ticketText(tk Ticket) string {
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
	return b.String()
}

func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
