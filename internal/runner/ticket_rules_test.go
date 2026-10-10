package runner

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func rows(ts ...Ticket) []Ticket { return ts }

func tk(id, status string, deps ...string) Ticket {
	return Ticket{ID: id, Status: status, DependsOn: deps}
}

func kid(id, parent, status string) Ticket {
	return Ticket{ID: id, Parent: parent, Status: status}
}

func refusal(t *testing.T, err error, want string) {
	t.Helper()
	var r *TicketRefusal
	if !errors.As(err, &r) || !strings.Contains(r.Reason, want) {
		t.Fatalf("err = %v, want a refusal containing %q", err, want)
	}
}

func TestAPersonMayMakeOnlyTheListedMoves(t *testing.T) {
	allowed := [][2]string{
		{TicketOpen, TicketReady}, {TicketReady, TicketOpen},
		{TicketBlocked, TicketOpen}, {TicketBlocked, TicketReady},
		{TicketOpen, TicketClosed}, {TicketReady, TicketClosed}, {TicketBlocked, TicketClosed},
		{TicketDone, TicketClosed}, {TicketClosed, TicketOpen},
		{TicketReady, TicketDone}, {TicketBlocked, TicketDone},
	}
	for _, m := range allowed {
		if err := personMove(m[0], m[1]); err != nil {
			t.Errorf("%s -> %s = %v, want allowed", m[0], m[1], err)
		}
	}
	refused := [][2]string{
		{TicketOpen, TicketRunning}, {TicketOpen, TicketDone}, {TicketRunning, TicketClosed},
		{TicketPlanning, TicketClosed}, {TicketReview, TicketClosed}, {TicketDone, TicketOpen},
		{TicketClosed, TicketReady}, {TicketOpen, TicketReview},
	}
	for _, m := range refused {
		refusal(t, personMove(m[0], m[1]), "cannot move")
	}
}

func TestAParentsStatusFollowsItsSubtickets(t *testing.T) {
	cases := []struct {
		kids []string
		want string
	}{
		{[]string{TicketDone, TicketDone}, TicketDone},
		{[]string{TicketDone, TicketClosed}, TicketDone},
		{[]string{TicketClosed, TicketClosed}, TicketClosed},
		{[]string{TicketDone, TicketBlocked, TicketRunning}, TicketBlocked},
		{[]string{TicketDone, TicketRunning, TicketReady}, TicketRunning},
		{[]string{TicketOpen, TicketReady}, TicketReady},
		{[]string{TicketOpen, TicketDone}, TicketOpen},
	}
	for _, c := range cases {
		var kids []Ticket
		for _, s := range c.kids {
			kids = append(kids, Ticket{Status: s})
		}
		if got := derive(kids); got != c.want {
			t.Errorf("derive(%v) = %s, want %s", c.kids, got, c.want)
		}
	}
}

func TestDependenciesAreCheckedAndCyclesRefused(t *testing.T) {
	all := rows(
		tk("TCK-1", TicketOpen),
		kid("TCK-2", "TCK-1", TicketReady),
		tk("TCK-3", TicketReady, "TCK-4"),
		tk("TCK-4", TicketReady, "TCK-5"),
		tk("TCK-5", TicketReady),
	)
	self := all[findTicket(all, "TCK-5")]
	_, err := checkDeps(all, self, []string{"TCK-3"})
	refusal(t, err, "TCK-5 -> TCK-3 -> TCK-4 -> TCK-5")
	_, err = checkDeps(all, self, []string{"TCK-5"})
	refusal(t, err, "cannot wait for itself")
	_, err = checkDeps(all, self, []string{"TCK-9"})
	refusal(t, err, "TCK-9 does not exist")
	_, err = checkDeps(all, all[1], []string{"TCK-1"})
	refusal(t, err, "its own parent")
	_, err = checkDeps(all, all[0], []string{"TCK-2"})
	refusal(t, err, "its own subticket")

	got, err := checkDeps(all, self, []string{"TCK-2", "TCK-2"})
	if err != nil || len(got) != 1 || got[0] != "TCK-2" {
		t.Errorf("duplicates = %v, %v, want one TCK-2", got, err)
	}
}

func TestRunnableNeedsReadyNoSubticketsAndDoneDependencies(t *testing.T) {
	all := rows(
		tk("TCK-1", TicketReady),
		kid("TCK-2", "TCK-1", TicketDone),
		tk("TCK-3", TicketReady, "TCK-2"),
		tk("TCK-4", TicketReady, "TCK-5"),
		tk("TCK-5", TicketRunning),
		tk("TCK-6", TicketOpen),
	)
	want := map[string]bool{"TCK-1": false, "TCK-3": true, "TCK-4": false, "TCK-6": false}
	for id, runnable := range want {
		if v := ticketView(all, all[findTicket(all, id)]); v.Runnable != runnable {
			t.Errorf("%s runnable = %v, want %v", id, v.Runnable, runnable)
		}
	}
	p := ticketView(all, all[0]).Progress
	if p == nil || p.Done != 1 || p.Total != 1 {
		t.Errorf("parent progress = %+v, want 1/1", p)
	}
	if ticketView(all, all[2]).Progress != nil {
		t.Error("a ticket without subtickets has no progress")
	}
}

func TestASubticketAlsoWaitsForItsParentsDependencies(t *testing.T) {
	all := rows(
		tk("TCK-1", TicketReady, "TCK-3"),
		kid("TCK-2", "TCK-1", TicketReady),
		tk("TCK-3", TicketReady),
	)
	if ticketView(all, all[1]).Runnable {
		t.Error("TCK-2 is runnable while its parent waits for TCK-3")
	}
	all[2].Status = TicketDone
	if !ticketView(all, all[1]).Runnable {
		t.Error("TCK-2 is not runnable after its parent's dependency is done")
	}
}

func TestCyclesThroughAParentAreRefused(t *testing.T) {
	all := rows(
		tk("TCK-1", TicketOpen),
		kid("TCK-2", "TCK-1", TicketOpen),
		tk("TCK-3", TicketOpen, "TCK-1"),
	)
	_, err := checkDeps(all, all[1], []string{"TCK-3"})
	refusal(t, err, "TCK-2 -> TCK-3 -> TCK-1 -> TCK-2")

	all = rows(
		tk("TCK-1", TicketOpen, "TCK-3"),
		kid("TCK-2", "TCK-1", TicketOpen),
		tk("TCK-3", TicketOpen),
		tk("TCK-4", TicketOpen, "TCK-2"),
	)
	_, err = checkDeps(all, all[2], []string{"TCK-2"})
	refusal(t, err, "TCK-3 -> TCK-2 -> TCK-3")
	_, err = checkDeps(all, all[0], []string{"TCK-4"})
	refusal(t, err, "TCK-1 -> TCK-4 -> TCK-2")
}

func TestCycleSearchStaysFastOnDeepChainsOfParents(t *testing.T) {
	const parents = 100
	var all []Ticket
	for i := 0; i < parents; i++ {
		p := tk(fmt.Sprintf("P-%d", i), TicketOpen)
		if i > 0 {
			p.DependsOn = []string{fmt.Sprintf("P-%d", i-1)}
		}
		all = append(all, p)
		for k := 0; k < 5; k++ {
			all = append(all, kid(fmt.Sprintf("P-%d-%d", i, k), p.ID, TicketOpen))
		}
	}
	all = append(all, tk("TOP", TicketOpen, fmt.Sprintf("P-%d", parents-1)), tk("LOW", TicketOpen))
	low := all[len(all)-1]

	start := time.Now()
	got, err := checkDeps(all, low, []string{"TOP"})
	if err != nil || len(got) != 1 {
		t.Fatalf("checkDeps = %v, %v; want TOP accepted", got, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("cycle search took %v", d)
	}

	_, err = checkDeps(all, all[0], []string{"TOP"})
	refusal(t, err, "that makes a cycle: P-0 -> TOP -> P-99")
}

func TestOnlyThePlannerRunChangesADraftWhilePlanning(t *testing.T) {
	plan := tk("TCK-1", TicketPlanning)
	plan.Runs = []string{"RUN-1"}
	all := rows(plan, kid("TCK-2", "TCK-1", TicketOpen), tk("TCK-3", TicketOpen))
	if err := planLock(all, "TCK-1", "RUN-1"); err != nil {
		t.Errorf("the planner run = %v", err)
	}
	refusal(t, planLock(all, "TCK-1", ""), "TCK-1 is being planned")
	refusal(t, planLock(all, "TCK-1", "RUN-2"), "RUN-2 does not plan TCK-1")
	refusal(t, planLock(all, "", "RUN-1"), "RUN-1 only changes the subtickets of the ticket it plans")
	if err := planLock(all, "TCK-3", ""); err != nil {
		t.Errorf("a person and a ticket that is not planned = %v", err)
	}
	all[0].Status = TicketReview
	if err := planLock(all, "TCK-1", ""); err != nil {
		t.Errorf("a person edits a plan in review = %v", err)
	}
	refusal(t, planLock(all, "TCK-1", "RUN-1"), "TCK-1 is review, not planning")
	if !isDraft(all, all[1]) || isDraft(all, all[2]) || isDraft(all, all[0]) {
		t.Error("only a subticket of a plan in review is a draft")
	}
	all[0].Status = TicketReady
	if isDraft(all, all[1]) {
		t.Error("an approved subticket is still a draft")
	}
}

func TestOnlyAnOpenTopLevelTicketWithoutSubticketsIsPlannable(t *testing.T) {
	refusal(t, plannable(kid("TCK-2", "TCK-1", TicketOpen), false),
		"TCK-2 is a subticket; only a top-level ticket is planned")
	refusal(t, plannable(tk("TCK-1", TicketReady), false), "TCK-1 is ready, not open")
	refusal(t, plannable(tk("TCK-1", TicketOpen), true), "TCK-1 already has subtickets")
	if err := plannable(tk("TCK-1", TicketOpen), false); err != nil {
		t.Errorf("an open ticket = %v", err)
	}
}
