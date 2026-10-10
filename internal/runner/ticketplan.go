package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gigovich/aigem/internal/uisession"
)

const planRule = "Split this ticket into subtickets that each fit one autonomous run of a coding agent. " +
	"Link them with dependencies. Read the code as much as you need; you cannot change it. " +
	"When the plan is complete, call plan_done with a short summary."

// Plan opens a planner run on an open top-level ticket without subtickets: a read-only run at
// the ticket's repository main checkout that splits it into draft subtickets.
func (t *TicketRuns) Plan(ctx context.Context, project, id string) (RunView, error) {
	tk, err := t.tickets.Get(project, id)
	if err != nil {
		return RunView{}, err
	}
	if err := plannable(tk.Ticket, tk.Progress != nil); err != nil {
		return RunView{}, err
	}
	if n := len(tk.Runs); n > 0 && t.live(tk.Runs[n-1]) {
		return RunView{}, refuse("%s has a live run %s; stop it first", id, tk.Runs[n-1])
	}
	return t.openPlanner(ctx, project, tk, func(run string) (TicketView, error) {
		return t.tickets.StartPlan(project, id, run)
	})
}

// openPlanner opens a planner run, lets move hand it the ticket, and sends the ticket, its draft
// and the project's repositories as the first message. When move refuses (a second click finds
// the ticket already moved) the run is deleted again.
func (t *TicketRuns) openPlanner(ctx context.Context, project string, tk TicketView,
	move func(run string) (TicketView, error)) (RunView, error) {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return RunView{}, ErrRunsClosed
	}
	pl, err := t.place(ctx, project, tk.Ticket)
	if err != nil {
		return RunView{}, err
	}
	if !pathExists(pl.repo) {
		return RunView{}, refuse("%s does not exist", pl.repo)
	}
	repos, err := t.projects.Repositories(ctx, project)
	if err != nil {
		return RunView{}, err
	}
	tr := &ticketRun{project: project, ticket: tk.ID, plan: true}
	tr.ctx, tr.cancel = context.WithCancel(context.Background())
	v, err := t.runs.Create(ctx, RunRequest{
		Mode: ModeAutonomous, Profile: readOnlyProfile, Title: "Plan " + tk.ID + ": " + tk.Title,
		ProjectID: project, TicketID: tk.ID, Dir: pl.repo, Tools: t.planTools(tr),
		OnTurn: func(ev uisession.Event) { t.onTurn(tr, ev) },
	})
	if err != nil {
		tr.cancel()
		return RunView{}, err
	}
	tr.run = v.ID
	// Registered before the move, so a Stop in between finds the run and lets its ticket go.
	t.mu.Lock()
	t.byRun[v.ID] = tr
	t.mu.Unlock()
	moved, err := move(v.ID)
	if err != nil {
		t.release(v.ID)
		if rmErr := t.runs.Remove(v.ID); rmErr != nil {
			slog.Warn("a planner run that could not start was not deleted", "run", v.ID, "err", rmErr)
		}
		return RunView{}, err
	}
	all, _ := t.tickets.List(project)
	var draft []TicketView
	for _, k := range all {
		if k.Parent == tk.ID {
			draft = append(draft, k)
		}
	}
	if err := t.runs.Apply(v.ID, RunOp{Op: OpSubmit, Text: planPrompt(moved.Ticket, draft, repos)}); err != nil {
		t.review(project, tk.ID, v.ID, "the ticket could not be sent to the run: "+err.Error())
	}
	return t.runs.Get(v.ID)
}

// review hands a plan to a person with the comment the planner run left.
func (t *TicketRuns) review(project, id, run, comment string) {
	v, err := t.tickets.PlanReview(project, id, run, comment)
	if err != nil {
		slog.Warn("a plan could not be handed to review", "ticket", id, "err", err)
		return
	}
	t.planned(project, v, comment)
}

// planWords is what a planner turn leaves on its ticket.
func planWords(ev uisession.Event, summary *string) string {
	switch {
	case ev.Interrupted || ev.Error != "":
		return lastWords(ev)
	case summary != nil:
		return *summary
	case strings.TrimSpace(ev.Text) != "":
		return ev.Text
	}
	return "the turn ended without plan_done"
}

func planPrompt(tk Ticket, draft []TicketView, repos []Repository) string {
	var b strings.Builder
	b.WriteString(ticketText(tk))
	b.WriteString("## Repositories\n\n- \"\" (the project directory)\n")
	for _, r := range repos {
		if r.Name != "" {
			fmt.Fprintf(&b, "- %s\n", r.Name)
		}
	}
	if len(draft) > 0 {
		b.WriteString("\n## Your draft\n\n")
		for _, k := range draft {
			fmt.Fprintf(&b, "- %s: %s", k.ID, k.Title)
			if k.Repo != "" {
				fmt.Fprintf(&b, " (repo %s)", k.Repo)
			}
			if len(k.DependsOn) > 0 {
				fmt.Fprintf(&b, " (waits for %s)", strings.Join(k.DependsOn, ", "))
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\n" + planRule)
	return b.String()
}

// Approve makes a reviewed plan's subtickets ready and stops its planner run.
func (t *TicketRuns) Approve(project, id string) (TicketView, error) {
	v, err := t.tickets.Approve(project, id)
	if err != nil {
		return TicketView{}, err
	}
	t.endPlanner(v)
	return v, nil
}

// Reject deletes a reviewed plan's subtickets, opens the ticket again and stops its planner run.
func (t *TicketRuns) Reject(project, id, reason string) (TicketView, error) {
	v, err := t.tickets.Reject(project, id, reason)
	if err != nil {
		return TicketView{}, err
	}
	t.endPlanner(v)
	return v, nil
}

// Revise sends a person's feedback to the planner run that is still live, or to a new one whose
// first message carries the draft and the discussion with the feedback.
func (t *TicketRuns) Revise(ctx context.Context, project, id, feedback string) (TicketView, error) {
	tk, err := t.tickets.Get(project, id)
	if err != nil {
		return TicketView{}, err
	}
	if tk.Status != TicketReview {
		return TicketView{}, refuse("%s is %s, not in review", id, tk.Status)
	}
	if n := len(tk.Runs); n > 0 && t.live(tk.Runs[n-1]) {
		run := tk.Runs[n-1]
		if _, err := t.tickets.Revise(project, id, run, feedback); err != nil {
			return TicketView{}, err
		}
		if err := t.runs.Apply(run, RunOp{Op: OpSubmit, Text: reviseNote(feedback)}); err != nil {
			t.review(project, id, run, "the feedback could not be sent to the run: "+err.Error())
		}
		return t.tickets.Get(project, id)
	}
	if _, err := t.openPlanner(ctx, project, tk, func(run string) (TicketView, error) {
		return t.tickets.Revise(project, id, run, feedback)
	}); err != nil {
		return TicketView{}, err
	}
	return t.tickets.Get(project, id)
}

// endPlanner stops the planner run of a plan a person decided. The decision is recorded first,
// so the run's last turn end finds nothing to change.
func (t *TicketRuns) endPlanner(v TicketView) {
	n := len(v.Runs)
	if n == 0 {
		return
	}
	run := v.Runs[n-1]
	if tr := t.release(run); tr != nil {
		tr.mu.Lock()
		tr.gone = true
		tr.mu.Unlock()
	}
	if err := t.runs.Stop(run); err != nil && !errors.Is(err, ErrRunClosed) && !errors.Is(err, ErrNoRun) {
		slog.Warn("a decided plan's run could not be stopped", "run", run, "err", err)
	}
}

func reviseNote(feedback string) string {
	return "A person reviewed the plan and asks for changes:\n\n" + feedback +
		"\n\nRevise the subtickets, then call plan_done again."
}
