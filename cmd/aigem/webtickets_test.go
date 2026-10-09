package main

import (
	"context"
	"errors"
	"path/filepath"
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

	count := map[string]int{}
	for _, k := range *kinds {
		count[k]++
	}
	if count["ticket.updated"] != 2 {
		t.Errorf("kinds = %v, want ticket.updated for the create and the close", *kinds)
	}
	feed, _ := b.Activity(ctx, 0, 0)
	var seen []string
	for _, a := range feed {
		seen = append(seen, a.Kind)
	}
	if !containsAll(seen, "ticket.created", "ticket.closed") {
		t.Errorf("activity = %v, want ticket.created and ticket.closed", seen)
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
