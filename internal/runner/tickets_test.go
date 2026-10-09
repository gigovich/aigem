package runner

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestTickets(t *testing.T, dir string) (*Tickets, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var told []string
	tickets := NewTickets(TicketsConfig{
		Dir: dir,
		Notify: func(project string, v TicketView) {
			mu.Lock()
			told = append(told, project+"/"+v.ID)
			mu.Unlock()
		},
		Now: func() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC) },
	})
	return tickets, &told
}

func mustCreate(t *testing.T, ts *Tickets, n NewTicket) TicketView {
	t.Helper()
	v, err := ts.Create("PRJ-1", n)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func ptr[T any](v T) *T { return &v }

func TestTicketsSurviveARestartAndIdsAreNeverReused(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	ts, _ := newTestTickets(t, dir)
	mustCreate(t, ts, NewTicket{Title: "one", By: "you"})
	two := mustCreate(t, ts, NewTicket{Title: "two", By: "you"})
	if err := ts.Delete("PRJ-1", two.ID); err != nil {
		t.Fatal(err)
	}

	again, _ := newTestTickets(t, dir)
	list, err := again.List("PRJ-1")
	if err != nil || len(list) != 1 || list[0].Title != "one" || list[0].Status != TicketOpen {
		t.Fatalf("after restart = %+v, %v", list, err)
	}
	if three := mustCreate(t, again, NewTicket{Title: "three", By: "you"}); three.ID != "TCK-3" {
		t.Errorf("next id = %s, want TCK-3 (TCK-2 was deleted, not free)", three.ID)
	}
	if _, err := os.Stat(filepath.Join(dir, "projects", "PRJ-1", "tickets.json")); err != nil {
		t.Errorf("tickets file: %v", err)
	}
}

func TestABadProjectIdNeverReachesTheFileSystem(t *testing.T) {
	dir := t.TempDir()
	ts, _ := newTestTickets(t, dir)
	for _, id := range []string{"", "../x", "PRJ-1/..", "PRJ-x", "PRJ-01", "TCK-1"} {
		if _, err := ts.Create(id, NewTicket{Title: "t"}); !errors.Is(err, ErrNoProject) {
			t.Errorf("project %q = %v, want ErrNoProject", id, err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("state dir has %v, want nothing written", entries)
	}
}

func TestSubticketsDriveTheParentAndBothAreAnnounced(t *testing.T) {
	ts, told := newTestTickets(t, "")
	parent := mustCreate(t, ts, NewTicket{Title: "goal", By: "you"})
	a := mustCreate(t, ts, NewTicket{Title: "a", Parent: parent.ID, By: "run RUN-7"})
	b := mustCreate(t, ts, NewTicket{Title: "b", Parent: parent.ID, DependsOn: []string{a.ID}})

	_, err := ts.Create("PRJ-1", NewTicket{Title: "deep", Parent: a.ID})
	refusal(t, err, "cannot have subtickets")
	_, err = ts.Update("PRJ-1", parent.ID, TicketPatch{Status: ptr(TicketReady)})
	refusal(t, err, "follows its subtickets")

	*told = nil
	if _, err := ts.Update("PRJ-1", a.ID, TicketPatch{Status: ptr(TicketReady)}); err != nil {
		t.Fatal(err)
	}
	if len(*told) != 2 || (*told)[1] != "PRJ-1/"+parent.ID {
		t.Errorf("told = %v, want the subticket then its parent", *told)
	}
	got, _ := ts.Get("PRJ-1", parent.ID)
	if got.Status != TicketReady || got.Progress == nil || got.Progress.Total != 2 {
		t.Errorf("parent = %+v, want ready with progress over 2", got)
	}
	if bv, _ := ts.Get("PRJ-1", b.ID); bv.Runnable {
		t.Error("b waits for a, which is not done, so b is not runnable")
	}

	ts.Update("PRJ-1", a.ID, TicketPatch{Status: ptr(TicketDone)})
	ts.Update("PRJ-1", b.ID, TicketPatch{Status: ptr(TicketReady)})
	if bv, _ := ts.Get("PRJ-1", b.ID); !bv.Runnable {
		t.Error("b is ready and a is done, so b is runnable")
	}
	ts.Update("PRJ-1", b.ID, TicketPatch{Status: ptr(TicketClosed)})
	if p, _ := ts.Get("PRJ-1", parent.ID); p.Status != TicketDone {
		t.Errorf("parent = %s, want done (one done, one closed)", p.Status)
	}
	if a.By != "run RUN-7" {
		t.Errorf("a.By = %q, want the agent that made it", a.By)
	}
}

func TestDeleteRefusalsAndComments(t *testing.T) {
	ts, _ := newTestTickets(t, "")
	parent := mustCreate(t, ts, NewTicket{Title: "goal"})
	kidv := mustCreate(t, ts, NewTicket{Title: "kid", Parent: parent.ID})
	waiter := mustCreate(t, ts, NewTicket{Title: "waiter", DependsOn: []string{kidv.ID}})

	refusal(t, ts.Delete("PRJ-1", parent.ID), "has subtickets")
	refusal(t, ts.Delete("PRJ-1", kidv.ID), "waits for")
	if err := ts.Delete("PRJ-1", "TCK-99"); !errors.Is(err, ErrNoTicket) {
		t.Errorf("unknown = %v, want ErrNoTicket", err)
	}

	_, err := ts.Comment("PRJ-1", waiter.ID, "you", "  ")
	refusal(t, err, "empty")
	v, err := ts.Comment("PRJ-1", waiter.ID, "run RUN-3", "looks done")
	if err != nil || len(v.Comments) != 1 || v.Comments[0].By != "run RUN-3" {
		t.Errorf("comment = %+v, %v", v.Comments, err)
	}
	_, err = ts.Create("PRJ-1", NewTicket{Title: "   "})
	refusal(t, err, "needs a title")
	_, err = ts.Update("PRJ-1", waiter.ID, TicketPatch{Status: ptr("doing")})
	refusal(t, err, "unknown status")
}

func TestAWriteThatFailsChangesNothing(t *testing.T) {
	dir := t.TempDir()
	ts, told := newTestTickets(t, dir)
	one := mustCreate(t, ts, NewTicket{Title: "one"})
	projectDir := filepath.Join(dir, "projects", "PRJ-1")
	if err := os.Chmod(projectDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(projectDir, 0o700) })
	*told = nil

	if _, err := ts.Update("PRJ-1", one.ID, TicketPatch{Status: ptr(TicketReady)}); err == nil {
		t.Fatal("a failed write reported success")
	}
	if v, _ := ts.Get("PRJ-1", one.ID); v.Status != TicketOpen {
		t.Errorf("status = %s, want open after a failed write", v.Status)
	}
	if len(*told) != 0 {
		t.Errorf("told = %v, want nothing announced", *told)
	}
}

func TestParallelWritesKeepEveryTicket(t *testing.T) {
	ts, _ := newTestTickets(t, t.TempDir())
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ts.Create("PRJ-1", NewTicket{Title: "t"})
		}()
	}
	wg.Wait()
	if list, _ := ts.List("PRJ-1"); len(list) != 20 {
		t.Errorf("tickets = %d, want 20", len(list))
	}
}

func TestANewSubticketCannotCloseACycleThroughItsParent(t *testing.T) {
	ts, _ := newTestTickets(t, "")
	parent := mustCreate(t, ts, NewTicket{Title: "goal"})
	x := mustCreate(t, ts, NewTicket{Title: "x", DependsOn: []string{parent.ID}})
	_, err := ts.Create("PRJ-1", NewTicket{Title: "kid", Parent: parent.ID, DependsOn: []string{x.ID}})
	refusal(t, err, "that makes a cycle")
}

func TestAPatchThatChangesNothingIsNotSavedOrAnnounced(t *testing.T) {
	ts, told := newTestTickets(t, "")
	a := mustCreate(t, ts, NewTicket{Title: "a"})
	b := mustCreate(t, ts, NewTicket{Title: "b", DependsOn: []string{a.ID}})
	ts.now = func() time.Time { return b.Updated.Add(time.Hour) }
	*told = nil
	for _, p := range []TicketPatch{{}, {Status: ptr(TicketOpen)}, {DependsOn: &[]string{a.ID}}} {
		got, err := ts.Update("PRJ-1", b.ID, p)
		if err != nil || !got.Updated.Equal(b.Updated) {
			t.Errorf("Update(%+v) = %v, %v, want no change", p, got.Updated, err)
		}
	}
	if len(*told) != 0 {
		t.Errorf("told = %v, want nothing", *told)
	}
	if got, _ := ts.Update("PRJ-1", b.ID, TicketPatch{DependsOn: &[]string{}}); !got.Updated.After(b.Updated) {
		t.Error("removing a dependency is a change and must bump Updated")
	}
}
