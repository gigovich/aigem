package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/store"
	"github.com/gigovich/aigem/internal/web"
)

func ticketsBackend(t *testing.T) (*webBackend, string, *[]string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	projects := testProjects(t, nil)
	added, err := projects.Add(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	notify := func(kind string, _ any) { kinds = append(kinds, kind) }
	// The registry announces through the same notify the daemon wires in webcmd.go.
	tickets := runner.NewTickets(runner.TicketsConfig{
		Dir: t.TempDir(),
		Notify: func(p string, v runner.TicketView) {
			notify("ticket.updated", map[string]string{"projectId": p, "id": v.ID})
		},
	})
	b := newWebBackend(webBackendConfig{
		projects: projects,
		tickets:  tickets,
		activity: store.NewLog[web.Activity](filepath.Join(t.TempDir(), "activity.jsonl")),
		notify:   notify,
	})
	return b, added.ID, &kinds
}

func TestTicketsAreServedPerProjectWithRulesAsConflicts(t *testing.T) {
	b, project, _ := ticketsBackend(t)
	ctx := context.Background()

	if _, err := b.Tickets(ctx, "PRJ-99"); !errors.Is(err, web.ErrNoProject) {
		t.Errorf("unknown project = %v, want web.ErrNoProject", err)
	}
	parent, err := b.CreateTicket(ctx, project, web.NewTicket{Title: "goal"})
	if err != nil || parent.ID != "TCK-1" || parent.By != "you" || parent.Status != "open" {
		t.Fatalf("create = %+v, %v", parent, err)
	}
	kid, _ := b.CreateTicket(ctx, project, web.NewTicket{Title: "kid", Parent: parent.ID})
	_, err = b.CreateTicket(ctx, project, web.NewTicket{Title: "deep", Parent: kid.ID})
	if !errors.Is(err, web.ErrConflict) {
		t.Errorf("a subticket of a subticket = %v, want a conflict", err)
	}
	if _, err := b.Ticket(ctx, project, "TCK-9"); !errors.Is(err, web.ErrNoTicket) {
		t.Errorf("unknown ticket = %v, want web.ErrNoTicket", err)
	}
	ready := "ready"
	got, err := b.UpdateTicket(ctx, project, kid.ID, web.TicketPatch{Status: &ready})
	if err != nil || got.Status != "ready" || !got.Runnable {
		t.Errorf("update = %+v, %v", got, err)
	}
	list, _ := b.Tickets(ctx, project)
	if len(list) != 2 || list[0].Progress == nil || list[0].DependsOn == nil || list[0].Comments == nil {
		t.Errorf("list = %+v, want progress on the parent and empty arrays, not null", list)
	}
}

func TestTicketChangesAreAnnouncedAndRecorded(t *testing.T) {
	b, project, kinds := ticketsBackend(t)
	ctx := context.Background()
	tk, _ := b.CreateTicket(ctx, project, web.NewTicket{Title: "t"})
	closed := "closed"
	b.UpdateTicket(ctx, project, tk.ID, web.TicketPatch{Status: &closed})
	b.UpdateTicket(ctx, project, tk.ID, web.TicketPatch{Status: &closed})

	count := map[string]int{}
	for _, k := range *kinds {
		count[k]++
	}
	if count["ticket.updated"] != 2 {
		t.Errorf("kinds = %v, want ticket.updated for the create and the close only", *kinds)
	}
	feed, _ := b.Activity(ctx, 0, 0)
	var seen []string
	for _, a := range feed {
		seen = append(seen, a.Kind)
		count[a.Kind]++
	}
	if !containsAll(seen, "ticket.created", "ticket.closed") || count["ticket.closed"] != 1 {
		t.Errorf("activity = %v, want ticket.created and one ticket.closed", seen)
	}
}

func containsAll(have []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			found = found || h == w
		}
		if !found {
			return false
		}
	}
	return true
}

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

func TestPlansNeedTheCoordinatorAndAreRecorded(t *testing.T) {
	b, project, _ := ticketsBackend(t)
	ctx := context.Background()
	goal, _ := b.CreateTicket(ctx, project, web.NewTicket{Title: "goal"})
	if _, err := b.PlanTicket(ctx, project, goal.ID); !errors.Is(err, web.ErrUnavailable) {
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
		Runs: runs, Tickets: b.tickets, Projects: b.projects, Finished: b.ticketFinished, Planned: b.ticketPlanned,
	})
	_, err = b.PlanTicket(ctx, project, goal.ID)
	if err == nil || !strings.Contains(err.Error(), "no model in this test") {
		t.Errorf("a planner that cannot open = %v", err)
	}
	kid, _ := b.CreateTicket(ctx, project, web.NewTicket{Title: "kid", Parent: goal.ID})
	_, err = b.PlanTicket(ctx, project, kid.ID)
	if !errors.Is(err, web.ErrConflict) || !strings.Contains(err.Error(), "only a top-level ticket is planned") {
		t.Errorf("plan of a subticket = %v, want a conflict", err)
	}
	for name, call := range map[string]func() error{
		"approve": func() error { _, err := b.ApproveTicket(ctx, project, goal.ID); return err },
		"reject":  func() error { _, err := b.RejectTicket(ctx, project, goal.ID, "no"); return err },
		"revise":  func() error { _, err := b.ReviseTicket(ctx, project, goal.ID, "more"); return err },
	} {
		if err := call(); !errors.Is(err, web.ErrConflict) || !strings.Contains(err.Error(), "not in review") {
			t.Errorf("%s of an open ticket = %v, want a conflict", name, err)
		}
	}

	inReview := func(title string) web.Ticket {
		t.Helper()
		tk, _ := b.CreateTicket(ctx, project, web.NewTicket{Title: title})
		if _, err := b.tickets.StartPlan(project, tk.ID, "RUN-9"); err != nil {
			t.Fatal(err)
		}
		if _, err := b.tickets.PlanReview(project, tk.ID, "RUN-9", "One step."); err != nil {
			t.Fatal(err)
		}
		if _, err := b.CreateTicket(ctx, project, web.NewTicket{Title: "step", Parent: tk.ID}); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	approved := inReview("approved")
	if v, err := b.ApproveTicket(ctx, project, approved.ID); err != nil || v.Status != "ready" {
		t.Errorf("approve = %+v, %v", v, err)
	}
	rejected := inReview("rejected")
	if v, err := b.RejectTicket(ctx, project, rejected.ID, "too big"); err != nil || v.Status != "open" {
		t.Errorf("reject = %+v, %v", v, err)
	}
	b.ticketPlanned(project, runner.TicketView{Ticket: runner.Ticket{ID: "TCK-1", Runs: []string{"RUN-2"}}},
		"Two steps.\n\nmore")

	feed, _ := b.Activity(ctx, 0, 0)
	byKind := map[string]web.Activity{}
	for _, a := range feed {
		byKind[a.Kind] = a
	}
	if a := byKind["ticket.approved"]; a.Text != "Approved the plan of "+approved.ID+": approved" {
		t.Errorf("approved = %+v", a)
	}
	if a := byKind["ticket.rejected"]; a.Text != "Rejected the plan of "+rejected.ID+": too big" {
		t.Errorf("rejected = %+v", a)
	}
	if a := byKind["ticket.planned"]; a.Text != "Plan of TCK-1 in review: Two steps. ..." || a.RunRef != "RUN-2" {
		t.Errorf("planned = %+v", a)
	}
}
