package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func (f *fixture) setDispatch(slots int, paused bool) {
	f.t.Helper()
	if _, err := f.projects.SetDispatch(f.project, &slots, &paused); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) dispatcherConfig() DispatcherConfig {
	return DispatcherConfig{
		Projects: f.projects, Tickets: f.tickets, TicketRuns: f.tr,
		Started: func(_ string, v TicketView, run string) { f.tell("started: " + v.ID + " " + run) },
	}
}

func (f *fixture) dispatcher() *Dispatcher { return newDispatcher(f.dispatcherConfig()) }

func (f *fixture) pass(d *Dispatcher) {
	f.t.Helper()
	if !d.pass() {
		f.t.Fatal("the dispatcher stopped")
	}
}

func (f *fixture) tell(s string) {
	f.mu.Lock()
	f.told = append(f.told, s)
	f.mu.Unlock()
}

func (f *fixture) said() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.told)
}

func (f *fixture) count(prefix string) int {
	n := 0
	for _, s := range f.said() {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

func (f *fixture) ticket(id string) TicketView {
	f.t.Helper()
	v, err := f.tickets.Get(f.project, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

func TestTheDispatcherFillsFreeSlotsOldestFirst(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	first := f.ready("first")
	waits, err := f.tickets.Create(f.project, NewTicket{Title: "waits", DependsOn: []string{first}})
	if err != nil {
		t.Fatal(err)
	}
	goal := f.openTicket("goal")
	step, err := f.tickets.Create(f.project, NewTicket{Title: "step", Parent: goal})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{waits.ID, step.ID} {
		if _, err := f.tickets.Update(f.project, id, TicketPatch{Status: ptr(TicketReady)}); err != nil {
			t.Fatal(err)
		}
	}
	stuck := f.ready("stuck")
	planMe := f.openTicket("plan me")
	mine := f.ready("mine")
	last := f.ready("last")

	f.script.then(say("Which one?"))
	f.start(stuck)
	f.next(stuck, 0)
	f.script.then(hold(), hold(), hold(), hold(), hold())
	f.plan(planMe)
	f.start(mine)

	f.setDispatch(3, false)
	d := f.dispatcher()
	f.pass(d)
	for id, want := range map[string]string{
		first: TicketRunning, step.ID: TicketRunning, waits.ID: TicketReady, last: TicketReady,
		stuck: TicketBlocked, planMe: TicketPlanning, mine: TicketRunning, goal: TicketRunning,
	} {
		if got := f.ticket(id).Status; got != want {
			t.Errorf("%s is %s, want %s", id, got, want)
		}
	}

	f.setDispatch(4, false)
	f.pass(d)
	if s := f.ticket(last).Status; s != TicketRunning {
		t.Fatalf("with a fourth slot last is %s, want running", s)
	}
	for _, id := range []string{first, step.ID, last} {
		if f.count("started: "+id+" "+runIDPrefix) != 1 {
			t.Errorf("Started was not told about %s once: %v", id, f.said())
		}
	}
	if f.count("started: ") != 3 {
		t.Errorf("told %v, want only the dispatcher's three starts", f.said())
	}
}

func TestPauseStopsNewStartsAndResumeFillsTheSlots(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	one, two := f.ready("one"), f.ready("two")
	f.script.then(hold(), hold())
	d := f.dispatcher()
	after := func(slots int, paused bool, a, b string) {
		t.Helper()
		f.setDispatch(slots, paused)
		f.pass(d)
		if x, y := f.ticket(one).Status, f.ticket(two).Status; x != a || y != b {
			t.Fatalf("slots %d, paused %v: one %s, two %s; want %s and %s", slots, paused, x, y, a, b)
		}
	}
	after(1, true, TicketReady, TicketReady)
	after(1, false, TicketRunning, TicketReady)
	after(2, true, TicketRunning, TicketReady)
	after(0, false, TicketRunning, TicketReady)
	after(2, false, TicketRunning, TicketRunning)
}

func TestAFinishedTicketFreesItsSlotForTheNext(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	d := newDispatcher(f.dispatcherConfig())
	f.tickets.notify = func(string, TicketView) { d.Wake() }
	first, second := f.ready("first"), f.ready("second")
	wrote := edit(filepath.Join(f.worktree(first), "a.txt"), "a\n", done("Did first."))
	f.script.then(wrote, say("ok"), hold())
	f.setDispatch(1, false)
	go d.loop()
	t.Cleanup(d.Close)

	waitUntil(t, func() bool { return f.toldAbout("started: " + second) })
	if a, b := f.ticket(first).Status, f.ticket(second).Status; a != TicketDone || b != TicketRunning {
		t.Fatalf("first is %s, second %s; want done and running", a, b)
	}
	if !f.toldAbout("started: " + first) {
		t.Errorf("told %v", f.said())
	}

	d.Close()
	late := f.ready("late")
	f.setDispatch(2, false)
	d.Wake()
	if s := f.ticket(late).Status; s != TicketReady {
		t.Errorf("after Close the ticket is %s, want ready", s)
	}
	d.Close()
}

func TestWakeUpsCoalesce(t *testing.T) {
	d := newDispatcher(DispatcherConfig{})
	for range 3 {
		d.Wake()
	}
	if n := len(d.wake); n != 1 {
		t.Errorf("%d wake-ups pending, want 1", n)
	}
}

func TestTheDispatcherStopsWhenTheRunsClose(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	id := f.ready("never")
	f.tr.Close()
	f.setDispatch(1, false)
	if f.dispatcher().pass() {
		t.Fatal("the dispatcher went on after the ticket runs closed")
	}
	if v := f.ticket(id); v.Status != TicketReady || len(v.Comments) != 0 {
		t.Errorf("ticket = %s with %d comments, want ready and untouched", v.Status, len(v.Comments))
	}
}

func TestADispatcherBlocksABrokenTicketOnceAndGoesOn(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	broken := f.ready("in the way")
	writeFile(t, f.worktree(broken), "left.txt", "x")
	next := f.ready("next")
	f.script.then(hold())
	f.setDispatch(1, false)
	d := f.dispatcher()
	f.pass(d)

	if s := f.ticket(next).Status; s != TicketRunning {
		t.Fatalf("next is %s, want running in the same pass", s)
	}
	want := "the dispatcher could not start it: " + f.worktree(broken) +
		" is in the way of the worktree for " + broken + "; remove it"
	v := f.ticket(broken)
	if v.Status != TicketBlocked || len(v.Comments) != 1 || v.Comments[0].By != "aigem" ||
		v.Comments[0].Text != want {
		t.Fatalf("broken = %s %+v", v.Status, v.Comments)
	}
	f.pass(d)
	if n := len(f.ticket(broken).Comments); n != 1 {
		t.Errorf("the broken ticket has %d comments, want it tried once", n)
	}
	if n := f.count("blocked: the dispatcher could not start it: "); n != 1 {
		t.Errorf("finished told %d times, want once: %v", n, f.said())
	}
}

func TestTheDispatcherHoldsWhileTheDaemonIsFull(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	var chats []RunView
	for range maxLiveRuns {
		v, err := f.runs.Create(context.Background(), RunRequest{})
		if err != nil {
			t.Fatal(err)
		}
		chats = append(chats, v)
	}
	id := f.ready("waits for room")
	f.script.then(hold())
	f.setDispatch(1, false)
	d := f.dispatcher()
	f.pass(d)
	v := f.ticket(id)
	if v.Status != TicketReady || len(v.Comments) != 0 || pathExists(f.worktree(id)) || !d.held {
		t.Fatalf("while full = %s %+v, held %v; want ready, untouched, no worktree, held",
			v.Status, v.Comments, d.held)
	}
	if err := f.runs.Remove(chats[0].ID); err != nil {
		t.Fatal(err)
	}
	f.pass(d)
	if s := f.ticket(id).Status; s != TicketReady {
		t.Fatalf("a wake while held started it: %s", s)
	}
	d.held = false
	f.pass(d)
	if s := f.ticket(id).Status; s != TicketRunning {
		t.Fatalf("after the tick the ticket is %s, want running", s)
	}
}

func TestAFailureNotOfTheTicketHoldsUntilTheTick(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	one, two := f.ready("one"), f.ready("two")
	writeFile(t, f.repo, ".aigem", "not a directory")
	f.setDispatch(2, false)
	d := f.dispatcher()
	f.pass(d)
	for _, id := range []string{one, two} {
		if v := f.ticket(id); v.Status != TicketReady || len(v.Comments) != 0 {
			t.Fatalf("%s = %s %+v, want ready and untouched", id, v.Status, v.Comments)
		}
	}
	if !d.held || f.count("blocked: ") != 0 {
		t.Fatalf("held %v, told %v; want held and nothing blocked", d.held, f.said())
	}
	if err := os.Remove(filepath.Join(f.repo, ".aigem")); err != nil {
		t.Fatal(err)
	}
	f.script.then(hold(), hold())
	f.pass(d)
	if s := f.ticket(one).Status; s != TicketReady {
		t.Fatalf("a wake while held started one: %s", s)
	}
	d.held = false
	f.pass(d)
	for _, id := range []string{one, two} {
		if s := f.ticket(id).Status; s != TicketRunning {
			t.Errorf("after the tick %s is %s, want running", id, s)
		}
	}
}

func TestADispatcherLeavesATicketAPersonIsStarting(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	id := f.ready("mine")
	key := f.project + "/" + id
	f.tr.mu.Lock()
	f.tr.starting[key] = true
	f.tr.mu.Unlock()
	f.script.then(hold())
	f.setDispatch(1, false)
	d := f.dispatcher()
	f.pass(d)
	if v := f.ticket(id); v.Status != TicketReady || len(v.Comments) != 0 || d.held {
		t.Fatalf("while a person starts it = %s %+v, held %v; want ready, untouched, not held",
			v.Status, v.Comments, d.held)
	}
	f.tr.mu.Lock()
	delete(f.tr.starting, key)
	f.tr.mu.Unlock()
	f.pass(d)
	if s := f.ticket(id).Status; s != TicketRunning {
		t.Fatalf("the ticket is %s, want running", s)
	}
}

func TestAStoppedDispatcherNeverBlocksATicket(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	id := f.ready("cancelled")
	f.setDispatch(1, false)
	d := f.dispatcher()
	d.cancel()
	if d.pass() {
		t.Fatal("a pass after Close went on")
	}
	if v := f.ticket(id); v.Status != TicketReady || len(v.Comments) != 0 {
		t.Errorf("ticket = %s %+v, want ready and untouched", v.Status, v.Comments)
	}
}
