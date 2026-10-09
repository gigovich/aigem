# Agent Tickets Part 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Local tickets per project with one level of subtickets, dependencies and statuses,
served over HTTP and shown as a tree table and a ticket page in the web UI.

**Architecture:** A `runner.Tickets` registry owns every rule and persists one
`tickets.json` per project. `cmd/aigem`'s `webBackend` adapts it to a new
`web.TicketsBackend` seam; `internal/web` serves the routes and the `ticket.updated` control
frame. The React UI keeps the selected project's tickets in the app store and draws two screens.

**Tech Stack:** Go 1.26 (`net/http` ServeMux patterns, `internal/store`), React 19 + TypeScript,
Tailwind v4, vitest + Testing Library.

**Spec:** `docs/superpowers/specs/2026-10-09-agent-tickets-design.md` (part 1)

## Global Constraints

- Work directly on `main` in `/Users/gigovich/work/gigovich/aigem`; commit after every task.
- Ticket ids are `TCK-<n>` per project and are never reused, also after delete and restart.
- One level only: a subticket cannot have subtickets.
- Statuses: `open`, `planning`, `review`, `ready`, `running`, `blocked`, `done`, `closed`.
- Person moves: `open<->ready`; `blocked->open|ready`; any except
  `running|planning|review` -> `closed`; `closed->open`; `ready|blocked->done`.
- A parent with subtickets has a derived status and no moves of its own.
- `runnable` = status `ready`, no subtickets, every dependency `done`.
- Title and body are not editable in part 1; `PATCH` accepts only `status` and `dependsOn`.
- Body up to 64 KiB, comment up to 16 KiB; empty title is 400.
- Errors: 404 unknown project/ticket, 409 broken rule (sentence for a person), 400 bad input.
- Storage: `<stateDir>/projects/<projectID>/tickets.json`; a failed write rolls the change back.
- UI sizes in rem / Tailwind spacing scale only (no new px); no `cursor-*` classes (global rule);
  icon-only buttons need `aria-label` and `title`. Do not run prettier.
- Go tests that touch state set `XDG_STATE_HOME` to `t.TempDir()`.
- Commands: `go test ./internal/runner/... ./internal/web/... ./cmd/aigem/...`;
  lint `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./...`
  (local v1 cannot read the repo; the old QF1003 in `internal/tui/model_add.go` is not ours);
  UI in `internal/web/_ui`: `npm run lint`, `npm run check`,
  `NODE_OPTIONS=--no-experimental-webstorage npx vitest run`.
- Known failures on macOS, also on `main`, not ours: `TestLoadRefusesAnUnresolvableWorkingDirectory`,
  `TestLoadErrorNamesTheDirectoryAndKeepsTheCause` (runner), `TestSetupSandboxIsPrivate` (testenv).

## Review Focus

- A dependency added to a ticket that some subticket of it already waits on, or an indirect
  cycle through three tickets: refused with 409 and the path, nothing written. (Task 1 tests.)
- A subticket closed or done changes the parent's derived status and the parent is announced
  too, so the tree row updates live. (Task 2 tests.)
- A project id like `../x` or `PRJ-1/..` in the URL never reaches the file system.
  (Task 2 tests.)
- Two tabs: one creates a ticket while the other has the Tickets screen open - the other tab
  shows it without reload, and an older list response does not overwrite a newer one.
  (Task 5 tests.)
- The person switches project while the Tickets screen is open: the list empties and reloads
  for the new project, never showing the old project's tickets. (Task 5 tests.)

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/runner/ticket_rules.go` (new) | types, statuses, pure rules: moves, derived status, deps and cycles, runnable, views |
| `internal/runner/ticket_rules_test.go` (new) | rule tests |
| `internal/runner/tickets.go` (new) | `Tickets` registry: per-project tables, persistence, rollback, notify |
| `internal/runner/tickets_test.go` (new) | registry tests |
| `internal/web/api_tickets.go` (new) | `TicketsBackend`, wire types, handlers |
| `internal/web/api_tickets_test.go` (new) | handler tests with a fake |
| `internal/web/server.go`, `meta.go`, `api_runs.go` | routes, `tickets` feature, `ErrNoTicket`, sized decode |
| `cmd/aigem/webtickets.go` (new) | `webBackend` tickets methods, conversion, errors, activity |
| `cmd/aigem/webtickets_test.go` (new) | backend tests |
| `cmd/aigem/webbackend.go`, `webcmd.go`, `webruns.go` | field, wiring, `publishTicket`, `Unavailable` |
| `internal/web/_ui/src/lib/wire.ts`, `api.ts` | ticket types, control kind, API calls |
| `internal/web/_ui/src/state/app.ts` | `tickets` in the store, refresh, control frame |
| `internal/web/_ui/src/state/tickets.ts` (new) | pure UI helpers: tree rows, filters, moves, status look |
| `internal/web/_ui/src/screens/Tickets.tsx` (new) | tree table, filter, new ticket dialog |
| `internal/web/_ui/src/screens/Task.tsx` (new) | ticket page |
| `internal/web/_ui/src/screens/Placeholders.tsx` | deleted |
| `internal/web/_ui/src/screens/tickets.test.tsx` (new) | UI tests |
| `docs/web.md`, `CHANGELOG.md` | docs |

---

### Task 1: Ticket rules (pure)

**Files:**
- Create: `internal/runner/ticket_rules.go`
- Test: `internal/runner/ticket_rules_test.go`

**Interfaces:**
- Produces: `Ticket`, `Comment`, `TicketTable`, `TicketView`, `TicketProgress`, `TicketRefusal`,
  status constants `TicketOpen..TicketClosed`, `ErrNoTicket`, and unexported
  `findTicket(rows []Ticket, id string) int`, `subtickets(rows []Ticket, id string) []Ticket`,
  `personMove(from, to string) error`, `derive(kids []Ticket) string`,
  `checkDeps(rows []Ticket, t Ticket, deps []string) ([]string, error)`,
  `ticketView(rows []Ticket, t Ticket) TicketView`, `validStatus(s string) bool`.

- [ ] **Step 1: Write the failing tests**

```go
package runner

import (
	"errors"
	"strings"
	"testing"
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/runner/ -run 'Move|Parent|Dependencies|Runnable' 2>&1 | head`
Expected: FAIL, `undefined: personMove` (and the other names).

- [ ] **Step 3: Write the implementation**

```go
package runner

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	TicketOpen     = "open"
	TicketPlanning = "planning"
	TicketReview   = "review"
	TicketReady    = "ready"
	TicketRunning  = "running"
	TicketBlocked  = "blocked"
	TicketDone     = "done"
	TicketClosed   = "closed"
)

const ticketIDPrefix = "TCK-"

var ErrNoTicket = errors.New("runner: no such ticket")

// TicketRefusal is a change the ticket rules refuse. Reason is written for a person.
type TicketRefusal struct{ Reason string }

func (e *TicketRefusal) Error() string { return e.Reason }

func refuse(format string, a ...any) error {
	return &TicketRefusal{Reason: fmt.Sprintf(format, a...)}
}

type Comment struct {
	At   time.Time `json:"at"`
	By   string    `json:"by"`
	Text string    `json:"text"`
}

type Ticket struct {
	ID        string    `json:"id"`
	Repo      string    `json:"repo"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Status    string    `json:"status"`
	Parent    string    `json:"parent,omitempty"`
	DependsOn []string  `json:"dependsOn,omitempty"`
	By        string    `json:"by"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
	Comments  []Comment `json:"comments,omitempty"`
	Runs      []string  `json:"runs,omitempty"`
}

// TicketTable is one project's saved tickets. Next survives a delete, so an id never comes back.
type TicketTable struct {
	Next    int      `json:"next"`
	Tickets []Ticket `json:"tickets"`
}

type TicketProgress struct{ Done, Total int }

type TicketView struct {
	Ticket
	Runnable bool
	Progress *TicketProgress
}

func validStatus(s string) bool {
	switch s {
	case TicketOpen, TicketPlanning, TicketReview, TicketReady, TicketRunning, TicketBlocked,
		TicketDone, TicketClosed:
		return true
	}
	return false
}

func findTicket(rows []Ticket, id string) int {
	return slices.IndexFunc(rows, func(t Ticket) bool { return t.ID == id })
}

func subtickets(rows []Ticket, id string) []Ticket {
	var out []Ticket
	for _, t := range rows {
		if t.Parent == id {
			out = append(out, t)
		}
	}
	return out
}

func personMove(from, to string) error {
	switch {
	case to == TicketClosed && from != TicketRunning && from != TicketPlanning && from != TicketReview,
		from == TicketOpen && to == TicketReady,
		from == TicketReady && to == TicketOpen,
		from == TicketBlocked && (to == TicketOpen || to == TicketReady),
		from == TicketClosed && to == TicketOpen,
		(from == TicketReady || from == TicketBlocked) && to == TicketDone:
		return nil
	}
	return refuse("a ticket cannot move from %s to %s", from, to)
}

func derive(kids []Ticket) string {
	count := map[string]int{}
	for _, k := range kids {
		count[k.Status]++
	}
	switch {
	case count[TicketDone] > 0 && count[TicketDone]+count[TicketClosed] == len(kids):
		return TicketDone
	case count[TicketClosed] == len(kids):
		return TicketClosed
	case count[TicketBlocked] > 0:
		return TicketBlocked
	case count[TicketRunning] > 0:
		return TicketRunning
	case count[TicketReady] > 0:
		return TicketReady
	}
	return TicketOpen
}

// checkDeps validates the tickets t would wait for and returns them without duplicates.
func checkDeps(rows []Ticket, t Ticket, deps []string) ([]string, error) {
	var out []string
	for _, d := range deps {
		switch i := findTicket(rows, d); {
		case d == t.ID:
			return nil, refuse("%s cannot wait for itself", d)
		case i < 0:
			return nil, refuse("%s does not exist", d)
		case d == t.Parent:
			return nil, refuse("%s cannot wait for its own parent %s", t.ID, d)
		case rows[i].Parent == t.ID:
			return nil, refuse("%s cannot wait for its own subticket %s", t.ID, d)
		}
		if slices.Contains(out, d) {
			continue
		}
		if path := waitPath(rows, d, t.ID, nil); path != nil {
			return nil, refuse("that makes a cycle: %s -> %s", t.ID, strings.Join(path, " -> "))
		}
		out = append(out, d)
	}
	return out, nil
}

// waitPath is the chain of waits from one ticket to another, or nil when there is none.
func waitPath(rows []Ticket, from, to string, seen []string) []string {
	if from == to {
		return []string{to}
	}
	if slices.Contains(seen, from) {
		return nil
	}
	seen = append(seen, from)
	i := findTicket(rows, from)
	if i < 0 {
		return nil
	}
	for _, next := range rows[i].DependsOn {
		if rest := waitPath(rows, next, to, seen); rest != nil {
			return append([]string{from}, rest...)
		}
	}
	return nil
}

func ticketView(rows []Ticket, t Ticket) TicketView {
	v := TicketView{Ticket: t}
	if kids := subtickets(rows, t.ID); len(kids) > 0 {
		done := 0
		for _, k := range kids {
			if k.Status == TicketDone {
				done++
			}
		}
		v.Progress = &TicketProgress{Done: done, Total: len(kids)}
		return v
	}
	if t.Status != TicketReady {
		return v
	}
	for _, d := range t.DependsOn {
		if i := findTicket(rows, d); i < 0 || rows[i].Status != TicketDone {
			return v
		}
	}
	v.Runnable = true
	return v
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/runner/ -run 'Move|Parent|Dependencies|Runnable' -v 2>&1 | tail -15`
Expected: PASS for all four tests.

- [ ] **Step 5: Lint and commit**

```bash
gofmt -l internal/runner
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./internal/runner/...
git add internal/runner/ticket_rules.go internal/runner/ticket_rules_test.go
git commit -m "feat(runner): ticket rules - moves, derived parent status, dependencies"
```

---

### Task 2: Tickets registry

**Files:**
- Create: `internal/runner/tickets.go`
- Test: `internal/runner/tickets_test.go`

**Interfaces:**
- Consumes (Task 1): `Ticket`, `TicketTable`, `TicketView`, `refuse`, `findTicket`, `subtickets`,
  `personMove`, `derive`, `checkDeps`, `ticketView`, `validStatus`, `ErrNoTicket`. From
  `projects.go`: `ErrNoProject`, `projectIDPrefix`, `projectNumber`.
- Produces:
  ```go
  type TicketsConfig struct {
      Dir    string // the state dir; tickets live in Dir/projects/<id>/tickets.json; "" = memory
      Notify func(project string, v TicketView)
      Now    func() time.Time
  }
  func NewTickets(cfg TicketsConfig) *Tickets
  type NewTicket struct { Repo, Title, Body, Parent string; DependsOn []string; By string }
  type TicketPatch struct { Status *string; DependsOn *[]string }
  func (t *Tickets) List(project string) ([]TicketView, error)
  func (t *Tickets) Get(project, id string) (TicketView, error)
  func (t *Tickets) Create(project string, n NewTicket) (TicketView, error)
  func (t *Tickets) Update(project, id string, p TicketPatch) (TicketView, error)
  func (t *Tickets) Comment(project, id, by, text string) (TicketView, error)
  func (t *Tickets) Delete(project, id string) error
  ```

- [ ] **Step 1: Write the failing tests**

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/runner/ -run 'Tickets|BadProjectId|Subtickets|DeleteRefusals|WriteThatFails|ParallelWrites' 2>&1 | head`
Expected: FAIL, `undefined: NewTickets`.

- [ ] **Step 3: Write the implementation**

```go
package runner

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/store"
)

type TicketsConfig struct {
	Dir    string
	Notify func(project string, v TicketView)
	Now    func() time.Time
}

type NewTicket struct {
	Repo, Title, Body, Parent string
	DependsOn                 []string
	By                        string
}

type TicketPatch struct {
	Status    *string
	DependsOn *[]string
}

type Tickets struct {
	dir    string
	notify func(string, TicketView)
	now    func() time.Time

	mu    sync.Mutex
	books map[string]*ticketBook
}

type ticketBook struct {
	file  *store.File[TicketTable]
	table TicketTable
}

func NewTickets(cfg TicketsConfig) *Tickets {
	t := &Tickets{dir: cfg.Dir, notify: cfg.Notify, now: cfg.Now, books: map[string]*ticketBook{}}
	if t.notify == nil {
		t.notify = func(string, TicketView) {}
	}
	if t.now == nil {
		t.now = time.Now
	}
	return t
}

func (t *Tickets) bookLocked(project string) (*ticketBook, error) {
	if b := t.books[project]; b != nil {
		return b, nil
	}
	if n := projectNumber(project); n == 0 || project != projectIDPrefix+strconv.Itoa(n) {
		return nil, ErrNoProject
	}
	b := &ticketBook{}
	if t.dir != "" {
		b.file = store.New[TicketTable](filepath.Join(t.dir, "projects", project, "tickets.json"))
		saved, err := b.file.Load()
		if err != nil {
			return nil, fmt.Errorf("runner: could not read the tickets of %s: %w", project, err)
		}
		b.table = saved
		for _, tk := range saved.Tickets {
			if n, err := strconv.Atoi(strings.TrimPrefix(tk.ID, ticketIDPrefix)); err == nil && n > b.table.Next {
				b.table.Next = n
			}
		}
	}
	t.books[project] = b
	return b, nil
}

func (t *Tickets) List(project string) ([]TicketView, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, err := t.bookLocked(project)
	if err != nil {
		return nil, err
	}
	out := make([]TicketView, 0, len(b.table.Tickets))
	for _, tk := range b.table.Tickets {
		out = append(out, ticketView(b.table.Tickets, tk))
	}
	return out, nil
}

func (t *Tickets) Get(project, id string) (TicketView, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, err := t.bookLocked(project)
	if err != nil {
		return TicketView{}, err
	}
	i := findTicket(b.table.Tickets, id)
	if i < 0 {
		return TicketView{}, ErrNoTicket
	}
	return ticketView(b.table.Tickets, b.table.Tickets[i]), nil
}

func (t *Tickets) Create(project string, n NewTicket) (TicketView, error) {
	title := strings.TrimSpace(n.Title)
	if title == "" {
		return TicketView{}, refuse("a ticket needs a title")
	}
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		if n.Parent != "" {
			i := findTicket(tab.Tickets, n.Parent)
			if i < 0 {
				return nil, refuse("parent %s does not exist", n.Parent)
			}
			if tab.Tickets[i].Parent != "" {
				return nil, refuse("%s is a subticket and cannot have subtickets", n.Parent)
			}
		}
		now := t.now()
		tab.Next++
		tk := Ticket{
			ID: ticketIDPrefix + strconv.Itoa(tab.Next), Repo: n.Repo, Title: title, Body: n.Body,
			Status: TicketOpen, Parent: n.Parent, By: n.By, Created: now, Updated: now,
		}
		deps, err := checkDeps(tab.Tickets, tk, n.DependsOn)
		if err != nil {
			return nil, err
		}
		tk.DependsOn = deps
		tab.Tickets = append(tab.Tickets, tk)
		return []string{tk.ID}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}

func (t *Tickets) Update(project, id string, p TicketPatch) (TicketView, error) {
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		tk := &tab.Tickets[i]
		if p.DependsOn != nil {
			deps, err := checkDeps(tab.Tickets, *tk, *p.DependsOn)
			if err != nil {
				return nil, err
			}
			tk.DependsOn = deps
		}
		if p.Status != nil && *p.Status != tk.Status {
			switch {
			case !validStatus(*p.Status):
				return nil, refuse("unknown status %q", *p.Status)
			case len(subtickets(tab.Tickets, id)) > 0:
				return nil, refuse("%s follows its subtickets; change them instead", id)
			}
			if err := personMove(tk.Status, *p.Status); err != nil {
				return nil, err
			}
			tk.Status = *p.Status
		}
		tk.Updated = t.now()
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}

func (t *Tickets) Comment(project, id, by, text string) (TicketView, error) {
	if strings.TrimSpace(text) == "" {
		return TicketView{}, refuse("a comment cannot be empty")
	}
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		now := t.now()
		tab.Tickets[i].Comments = append(tab.Tickets[i].Comments, Comment{At: now, By: by, Text: text})
		tab.Tickets[i].Updated = now
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}

func (t *Tickets) Delete(project, id string) error {
	_, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		if len(subtickets(tab.Tickets, id)) > 0 {
			return nil, refuse("%s has subtickets; close it instead", id)
		}
		for _, other := range tab.Tickets {
			if slices.Contains(other.DependsOn, id) {
				return nil, refuse("%s waits for %s; close it instead", other.ID, id)
			}
		}
		if s := tab.Tickets[i].Status; s == TicketRunning || s == TicketPlanning {
			return nil, refuse("%s is %s; it cannot be deleted now", id, s)
		}
		parent := tab.Tickets[i].Parent
		tab.Tickets = slices.Delete(tab.Tickets, i, i+1)
		if parent != "" {
			return []string{id, parent}, nil
		}
		return []string{id}, nil
	})
	return err
}

// change applies fn to a copy of the project's table, settles the parents it touched, writes
// the copy and keeps it only when the write succeeded. Views come back in the order touched.
func (t *Tickets) change(project string, fn func(*TicketTable) ([]string, error)) ([]TicketView, error) {
	t.mu.Lock()
	b, err := t.bookLocked(project)
	if err != nil {
		t.mu.Unlock()
		return nil, err
	}
	next := cloneTable(b.table)
	touched, err := fn(&next)
	if err != nil {
		t.mu.Unlock()
		return nil, err
	}
	touched = settleParents(&next, touched, t.now())
	if b.file != nil {
		if err := b.file.Save(next); err != nil {
			t.mu.Unlock()
			return nil, fmt.Errorf("runner: could not write the tickets of %s: %w", project, err)
		}
	}
	b.table = next
	views := make([]TicketView, 0, len(touched))
	for _, id := range touched {
		if i := findTicket(next.Tickets, id); i >= 0 {
			views = append(views, ticketView(next.Tickets, next.Tickets[i]))
		} else {
			views = append(views, TicketView{Ticket: Ticket{ID: id}})
		}
	}
	t.mu.Unlock()
	for _, v := range views {
		t.notify(project, v)
	}
	return views, nil
}

// settleParents re-derives the status of every parent of a touched ticket (or a touched parent)
// and returns touched with those parents appended.
func settleParents(tab *TicketTable, touched []string, now time.Time) []string {
	out := slices.Clone(touched)
	for _, id := range touched {
		parent := id
		if i := findTicket(tab.Tickets, id); i >= 0 && tab.Tickets[i].Parent != "" {
			parent = tab.Tickets[i].Parent
		}
		pi := findTicket(tab.Tickets, parent)
		if pi < 0 {
			continue
		}
		kids := subtickets(tab.Tickets, parent)
		if len(kids) == 0 {
			continue
		}
		if s := derive(kids); s != tab.Tickets[pi].Status {
			tab.Tickets[pi].Status = s
			tab.Tickets[pi].Updated = now
		}
		if !slices.Contains(out, parent) {
			out = append(out, parent)
		}
	}
	return out
}

func cloneTable(tab TicketTable) TicketTable {
	out := TicketTable{Next: tab.Next, Tickets: make([]Ticket, len(tab.Tickets))}
	for i, tk := range tab.Tickets {
		tk.DependsOn = slices.Clone(tk.DependsOn)
		tk.Comments = slices.Clone(tk.Comments)
		tk.Runs = slices.Clone(tk.Runs)
		out.Tickets[i] = tk
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/runner/ -run 'Tickets|BadProjectId|Subtickets|DeleteRefusals|WriteThatFails|ParallelWrites|Move|Parent|Dependencies|Runnable' -v 2>&1 | tail -20`
Expected: PASS. (`TestSubticketsDriveTheParentAndBothAreAnnounced` checks that the subticket
is announced first and its parent second.)

- [ ] **Step 5: Lint and commit**

```bash
gofmt -l internal/runner
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./internal/runner/...
git add internal/runner/tickets.go internal/runner/tickets_test.go
git commit -m "feat(runner): tickets registry with per-project storage and rollback"
```

---

### Task 3: HTTP routes in `internal/web`

**Files:**
- Create: `internal/web/api_tickets.go`
- Test: `internal/web/api_tickets_test.go`
- Modify: `internal/web/server.go` (routes, after the `/api/projects/{id}/repos` lines)
- Modify: `internal/web/meta.go` (`featuresFor`)
- Modify: `internal/web/api_runs.go` (`decodeJSON`, `writeRunError`)

**Interfaces:**
- Produces:
  ```go
  type TicketsBackend interface {
      Tickets(ctx context.Context, project string) ([]Ticket, error)
      Ticket(ctx context.Context, project, id string) (Ticket, error)
      CreateTicket(ctx context.Context, project string, req NewTicket) (Ticket, error)
      UpdateTicket(ctx context.Context, project, id string, req TicketPatch) (Ticket, error)
      CommentTicket(ctx context.Context, project, id, text string) (Ticket, error)
      DeleteTicket(ctx context.Context, project, id string) error
  }
  type Ticket struct{ ID, Repo, Title, Body, Status, Parent string; DependsOn []string; By string;
      Created, Updated time.Time; Comments []TicketComment; Runs []string; Runnable bool;
      Progress *TicketProgress }   // json: id, repo, title, body, status, parent, dependsOn, by,
                                    // created, updated, comments, runs, runnable, progress
  type TicketComment struct{ At time.Time; By, Text string }   // json: at, by, text
  type TicketProgress struct{ Done, Total int }               // json: done, total
  type NewTicket struct{ Repo, Title, Body, Parent string; DependsOn []string }
  type TicketPatch struct{ Status *string; DependsOn *[]string }
  var ErrNoTicket
  ```
  Feature key `tickets`. Rule refusals arrive from the backend as `Conflict(reason)` (409).

- [ ] **Step 1: Write the failing tests**

```go
package web

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// ticketsBackend is a fake that records what the handlers passed and answers from a slice.
type ticketsBackend struct {
	*projectsBackend
	tmu     sync.Mutex
	tickets []Ticket
	patched TicketPatch
}

func (b *ticketsBackend) Tickets(_ context.Context, project string) ([]Ticket, error) {
	if project != "PRJ-1" {
		return nil, ErrNoProject
	}
	b.tmu.Lock()
	defer b.tmu.Unlock()
	return b.tickets, nil
}

func (b *ticketsBackend) Ticket(_ context.Context, project, id string) (Ticket, error) {
	b.tmu.Lock()
	defer b.tmu.Unlock()
	for _, t := range b.tickets {
		if t.ID == id {
			return t, nil
		}
	}
	return Ticket{}, ErrNoTicket
}

func (b *ticketsBackend) CreateTicket(_ context.Context, project string, req NewTicket) (Ticket, error) {
	if req.Parent == "TCK-2" {
		return Ticket{}, Conflict("TCK-2 is a subticket and cannot have subtickets")
	}
	t := Ticket{ID: "TCK-9", Title: req.Title, Status: "open", By: "you", DependsOn: req.DependsOn}
	return t, nil
}

func (b *ticketsBackend) UpdateTicket(_ context.Context, project, id string, req TicketPatch) (Ticket, error) {
	b.tmu.Lock()
	b.patched = req
	b.tmu.Unlock()
	return Ticket{ID: id, Status: "ready"}, nil
}

func (b *ticketsBackend) CommentTicket(_ context.Context, project, id, text string) (Ticket, error) {
	return Ticket{ID: id, Comments: []TicketComment{{By: "you", Text: text}}}, nil
}

func (b *ticketsBackend) DeleteTicket(_ context.Context, project, id string) error {
	if id == "TCK-1" {
		return Conflict("TCK-1 has subtickets; close it instead")
	}
	return nil
}

func newTicketsServer(t *testing.T) (*Server, *ticketsBackend) {
	t.Helper()
	_, pb := newProjectsServer(t)
	b := &ticketsBackend{projectsBackend: pb, tickets: []Ticket{
		{ID: "TCK-1", Title: "goal", Status: "ready", Progress: &TicketProgress{Done: 0, Total: 1}},
		{ID: "TCK-2", Title: "kid", Status: "ready", Parent: "TCK-1", Runnable: true},
		{ID: "TCK-3", Title: "done", Status: "done"},
	}}
	return newTestServer(t, Config{Backend: b}), b
}

func TestTheTicketRoutesAnswerAndFilter(t *testing.T) {
	srv, b := newTicketsServer(t)
	_, meta := getMeta(t, srv)
	if !meta.Features["tickets"] {
		t.Error("the feature map does not name tickets")
	}

	all := decode[[]Ticket](t, api(t, srv, http.MethodGet, "/api/projects/PRJ-1/tickets", ""))
	if len(all) != 3 || !all[1].Runnable || all[0].Progress.Total != 1 {
		t.Fatalf("list = %+v", all)
	}
	ready := decode[[]Ticket](t, api(t, srv, http.MethodGet, "/api/projects/PRJ-1/tickets?status=ready", ""))
	if len(ready) != 2 {
		t.Errorf("?status=ready = %d tickets, want 2", len(ready))
	}
	kids := decode[[]Ticket](t, api(t, srv, http.MethodGet, "/api/projects/PRJ-1/tickets?parent=TCK-1", ""))
	if len(kids) != 1 || kids[0].ID != "TCK-2" {
		t.Errorf("?parent=TCK-1 = %+v", kids)
	}
	if res := api(t, srv, http.MethodGet, "/api/projects/PRJ-9/tickets", ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown project = %d, want 404", res.StatusCode)
	}
	if res := api(t, srv, http.MethodGet, "/api/projects/PRJ-1/tickets/TCK-7", ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown ticket = %d, want 404", res.StatusCode)
	}

	res := api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets", `{"title":"new","dependsOn":["TCK-3"]}`)
	if res.StatusCode != http.StatusCreated || decode[Ticket](t, res).ID != "TCK-9" {
		t.Errorf("create = %d", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets", `{"title":"  "}`)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(readBody(t, res), "title is required") {
		t.Errorf("empty title = %d, want 400", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets", `{"title":"x","parent":"TCK-2"}`)
	if res.StatusCode != http.StatusConflict || !strings.Contains(readBody(t, res), "cannot have subtickets") {
		t.Errorf("a refused rule = %d, want 409 with the sentence", res.StatusCode)
	}
	big := strings.Repeat("a", 70<<10)
	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets", `{"title":"x","body":"`+big+`"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("a 70 KiB body = %d, want 400", res.StatusCode)
	}

	res = api(t, srv, http.MethodPatch, "/api/projects/PRJ-1/tickets/TCK-2", `{"status":"ready","dependsOn":[]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d", res.StatusCode)
	}
	b.tmu.Lock()
	if b.patched.Status == nil || *b.patched.Status != "ready" || b.patched.DependsOn == nil {
		t.Errorf("patched = %+v, want status and an empty dependsOn passed through", b.patched)
	}
	b.tmu.Unlock()
	res = api(t, srv, http.MethodPatch, "/api/projects/PRJ-1/tickets/TCK-2", `{"title":"renamed"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("patching the title = %d, want 400 (not editable in part 1)", res.StatusCode)
	}

	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets/TCK-2/comments", `{"text":"hi"}`)
	if res.StatusCode != http.StatusCreated {
		t.Errorf("comment = %d, want 201", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets/TCK-2/comments",
		`{"text":"`+strings.Repeat("a", 17<<10)+`"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("a 17 KiB comment = %d, want 400", res.StatusCode)
	}

	if res := api(t, srv, http.MethodDelete, "/api/projects/PRJ-1/tickets/TCK-3", ""); res.StatusCode != http.StatusNoContent {
		t.Errorf("delete = %d, want 204", res.StatusCode)
	}
	if res := api(t, srv, http.MethodDelete, "/api/projects/PRJ-1/tickets/TCK-1", ""); res.StatusCode != http.StatusConflict {
		t.Errorf("delete refused = %d, want 409", res.StatusCode)
	}
}

func TestTheTicketRoutesRefuseOtherMethodsAndNeedTheFeature(t *testing.T) {
	srv, _ := newTicketsServer(t)
	for path, method := range map[string]string{
		"/api/projects/PRJ-1/tickets":                http.MethodPut,
		"/api/projects/PRJ-1/tickets/TCK-1":          http.MethodPost,
		"/api/projects/PRJ-1/tickets/TCK-1/comments": http.MethodGet,
	} {
		if res := api(t, srv, method, path, ""); res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", method, path, res.StatusCode)
		}
	}
	projectsOnly, _ := newProjectsServer(t)
	if res := api(t, projectsOnly, http.MethodGet, "/api/projects/PRJ-1/tickets", ""); res.StatusCode != http.StatusNotImplemented {
		t.Errorf("a backend without the seam = %d, want 501", res.StatusCode)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/web/ -run Ticket 2>&1 | head`
Expected: FAIL, `undefined: Ticket` / `TicketsBackend`.

- [ ] **Step 3: Write the implementation**

`internal/web/api_tickets.go`:

```go
package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// The tickets API: a project's tickets, their subtickets and dependencies. Every rule lives
// behind the seam; a refused rule comes back as a Conflict carrying the sentence to show.

type TicketsBackend interface {
	Tickets(ctx context.Context, project string) ([]Ticket, error)
	Ticket(ctx context.Context, project, id string) (Ticket, error)
	CreateTicket(ctx context.Context, project string, req NewTicket) (Ticket, error)
	UpdateTicket(ctx context.Context, project, id string, req TicketPatch) (Ticket, error)
	CommentTicket(ctx context.Context, project, id, text string) (Ticket, error)
	DeleteTicket(ctx context.Context, project, id string) error
}

type Ticket struct {
	ID        string          `json:"id"`
	Repo      string          `json:"repo"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Status    string          `json:"status"`
	Parent    string          `json:"parent,omitempty"`
	DependsOn []string        `json:"dependsOn"`
	By        string          `json:"by"`
	Created   time.Time       `json:"created,omitzero"`
	Updated   time.Time       `json:"updated,omitzero"`
	Comments  []TicketComment `json:"comments"`
	Runs      []string        `json:"runs"`
	Runnable  bool            `json:"runnable"`
	Progress  *TicketProgress `json:"progress,omitempty"`
}

type TicketComment struct {
	At   time.Time `json:"at"`
	By   string    `json:"by"`
	Text string    `json:"text"`
}

type TicketProgress struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

type NewTicket struct {
	Repo      string   `json:"repo,omitempty"`
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	Parent    string   `json:"parent,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

type TicketPatch struct {
	Status    *string   `json:"status,omitempty"`
	DependsOn *[]string `json:"dependsOn,omitempty"`
}

// ErrNoTicket is returned for a ticket id the project does not hold.
var ErrNoTicket = errors.New("web: no such ticket")

const (
	maxTicketBody   = 64 << 10
	maxCommentBytes = 16 << 10
)

func (s *Server) handleTickets(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	items, err := b.Tickets(r.Context(), r.PathValue("id"))
	if err != nil {
		writeRunError(w, "listing tickets", err)
		return
	}
	status, parent := r.URL.Query().Get("status"), r.URL.Query().Get("parent")
	out := []Ticket{}
	for _, t := range items {
		if (status == "" || t.Status == status) && (parent == "" || t.Parent == parent) {
			out = append(out, t)
		}
	}
	writeJSON(w, out)
}

func (s *Server) handleTicket(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	t, err := b.Ticket(r.Context(), r.PathValue("id"), r.PathValue("tid"))
	if err != nil {
		writeRunError(w, "reading a ticket", err)
		return
	}
	writeJSON(w, t)
}

func (s *Server) handleCreateTicket(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	var req NewTicket
	if err := decodeJSONLimit(w, r, &req, maxTicketBody); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	t, err := b.CreateTicket(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeRunError(w, "creating a ticket", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, t)
}

func (s *Server) handleUpdateTicket(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	var req TicketPatch
	if err := decodeJSON(w, r, &req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	t, err := b.UpdateTicket(r.Context(), r.PathValue("id"), r.PathValue("tid"), req)
	if err != nil {
		writeRunError(w, "changing a ticket", err)
		return
	}
	writeJSON(w, t)
}

func (s *Server) handleCommentTicket(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := decodeJSONLimit(w, r, &req, maxCommentBytes+1<<10); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Text) > maxCommentBytes {
		http.Error(w, "a comment is at most 16 KiB", http.StatusBadRequest)
		return
	}
	t, err := b.CommentTicket(r.Context(), r.PathValue("id"), r.PathValue("tid"), req.Text)
	if err != nil {
		writeRunError(w, "commenting on a ticket", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, t)
}

func (s *Server) handleDeleteTicket(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	if err := b.DeleteTicket(r.Context(), r.PathValue("id"), r.PathValue("tid")); err != nil {
		writeRunError(w, "deleting a ticket", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

`internal/web/api_runs.go` - make `decodeJSON` a call into a sized variant, and map
`ErrNoTicket` (insert the case right after the `ErrNoProject` case in `writeRunError`):

```go
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return decodeJSONLimit(w, r, v, maxRunBody)
}

func decodeJSONLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	// ...the rest of the existing decodeJSON body, unchanged...
}
```

```go
	case errors.Is(err, ErrNoTicket):
		http.Error(w, "no such ticket", http.StatusNotFound)
```

(If `maxRunBody` is an untyped constant this compiles as is; if it is typed `int`, pass
`int64(maxRunBody)`.)

`internal/web/meta.go` - in `featuresFor`, after the `ProjectsBackend` check:

```go
	if _, ok := b.(TicketsBackend); ok {
		out["tickets"] = true
	}
```

`internal/web/server.go` - after the `/api/projects/{id}/repos` lines in `routes()`:

```go
	s.api("GET /api/projects/{id}/tickets", s.handleTickets)
	s.api("POST /api/projects/{id}/tickets", s.handleCreateTicket)
	s.mux.HandleFunc("/api/projects/{id}/tickets", methodNotAllowed("GET, HEAD, POST"))
	s.api("GET /api/projects/{id}/tickets/{tid}", s.handleTicket)
	s.api("PATCH /api/projects/{id}/tickets/{tid}", s.handleUpdateTicket)
	s.api("DELETE /api/projects/{id}/tickets/{tid}", s.handleDeleteTicket)
	s.mux.HandleFunc("/api/projects/{id}/tickets/{tid}", methodNotAllowed("GET, HEAD, PATCH, DELETE"))
	s.api("POST /api/projects/{id}/tickets/{tid}/comments", s.handleCommentTicket)
	s.mux.HandleFunc("/api/projects/{id}/tickets/{tid}/comments", methodNotAllowed("POST"))
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/web/... 2>&1 | tail -5`
Expected: PASS, including the existing `TestTheFeatureMapNamesProjects` (its fake does not
implement `TicketsBackend`, so its exact list is unchanged).

- [ ] **Step 5: Lint and commit**

```bash
gofmt -l internal/web
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./internal/web/...
git add internal/web/api_tickets.go internal/web/api_tickets_test.go internal/web/server.go \
  internal/web/meta.go internal/web/api_runs.go
git commit -m "feat(web): ticket routes and the tickets feature"
```

---

### Task 4: Daemon wiring in `cmd/aigem`

**Files:**
- Create: `cmd/aigem/webtickets.go`
- Test: `cmd/aigem/webtickets_test.go`
- Modify: `cmd/aigem/webbackend.go` (field `tickets *runner.Tickets` in `webBackend` and in
  `webBackendConfig`, copied in `newWebBackend`; `Unavailable` adds `"tickets"` when nil)
- Modify: `cmd/aigem/webruns.go` (`publishTicket`)
- Modify: `cmd/aigem/webcmd.go` (create the registry next to `projects`, pass it on)

**Interfaces:**
- Consumes: `runner.Tickets` and friends (Task 2), `web.TicketsBackend` and wire types (Task 3),
  `b.projects.Get(id)` returning `runner.ErrNoProject`, `b.recordActivity(web.Activity)`,
  `web.Conflict`, `web.ErrNoProject`, `web.ErrNoTicket`.
- Produces: `(*webBackend)` implements `web.TicketsBackend`; control frame
  `ticket.updated` with data `{"projectId": "<id>", "id": "<tid>"}`; activity kinds
  `ticket.created`, `ticket.closed`.

- [ ] **Step 1: Write the failing tests**

```go
package main

import (
	"context"
	"errors"
	"testing"

	"github.com/gigovich/aigem/internal/runner"
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
```

Imports for this test file: `context`, `errors`, `path/filepath`, `testing`,
`internal/runner`, `internal/store`, `internal/web`. `b.Activity(ctx, since, limit)` is the
existing reader (`cmd/aigem/webphase1.go`).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/aigem/ -run 'Ticket' 2>&1 | head`
Expected: FAIL, unknown field `tickets` in `webBackendConfig`.

- [ ] **Step 3: Write the implementation**

`cmd/aigem/webtickets.go`:

```go
package main

import (
	"context"
	"errors"

	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/web"
)

func (b *webBackend) Tickets(_ context.Context, project string) ([]web.Ticket, error) {
	if err := b.ticketProject(project); err != nil {
		return nil, err
	}
	views, err := b.tickets.List(project)
	if err != nil {
		return nil, webTicketError(err)
	}
	out := make([]web.Ticket, 0, len(views))
	for _, v := range views {
		out = append(out, webTicket(v))
	}
	return out, nil
}

func (b *webBackend) Ticket(_ context.Context, project, id string) (web.Ticket, error) {
	if err := b.ticketProject(project); err != nil {
		return web.Ticket{}, err
	}
	v, err := b.tickets.Get(project, id)
	return webTicket(v), webTicketError(err)
}

func (b *webBackend) CreateTicket(_ context.Context, project string, req web.NewTicket) (web.Ticket, error) {
	if err := b.ticketProject(project); err != nil {
		return web.Ticket{}, err
	}
	v, err := b.tickets.Create(project, runner.NewTicket{
		Repo: req.Repo, Title: req.Title, Body: req.Body, Parent: req.Parent,
		DependsOn: req.DependsOn, By: "you",
	})
	if err != nil {
		return web.Ticket{}, webTicketError(err)
	}
	b.recordActivity(web.Activity{Kind: "ticket.created", Text: "Created ticket " + v.ID + ": " + v.Title})
	return webTicket(v), nil
}

func (b *webBackend) UpdateTicket(_ context.Context, project, id string, req web.TicketPatch) (web.Ticket, error) {
	if err := b.ticketProject(project); err != nil {
		return web.Ticket{}, err
	}
	v, err := b.tickets.Update(project, id, runner.TicketPatch{Status: req.Status, DependsOn: req.DependsOn})
	if err != nil {
		return web.Ticket{}, webTicketError(err)
	}
	if req.Status != nil && *req.Status == runner.TicketClosed {
		b.recordActivity(web.Activity{Kind: "ticket.closed", Text: "Closed ticket " + v.ID + ": " + v.Title})
	}
	return webTicket(v), nil
}

func (b *webBackend) CommentTicket(_ context.Context, project, id, text string) (web.Ticket, error) {
	if err := b.ticketProject(project); err != nil {
		return web.Ticket{}, err
	}
	v, err := b.tickets.Comment(project, id, "you", text)
	return webTicket(v), webTicketError(err)
}

func (b *webBackend) DeleteTicket(_ context.Context, project, id string) error {
	if err := b.ticketProject(project); err != nil {
		return err
	}
	return webTicketError(b.tickets.Delete(project, id))
}

// ticketProject answers for a project the registry no longer lists, before the tickets
// registry would happily open a file for it.
func (b *webBackend) ticketProject(project string) error {
	if b.tickets == nil || b.projects == nil {
		return web.ErrUnavailable
	}
	if _, err := b.projects.Get(project); err != nil {
		return webProjectError(err)
	}
	return nil
}

func webTicketError(err error) error {
	var refusal *runner.TicketRefusal
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runner.ErrNoTicket):
		return web.ErrNoTicket
	case errors.Is(err, runner.ErrNoProject):
		return web.ErrNoProject
	case errors.As(err, &refusal):
		return web.Conflict(refusal.Reason)
	default:
		return err
	}
}

func webTicket(v runner.TicketView) web.Ticket {
	t := web.Ticket{
		ID: v.ID, Repo: v.Repo, Title: v.Title, Body: v.Body, Status: v.Status, Parent: v.Parent,
		DependsOn: append([]string{}, v.DependsOn...), By: v.By, Created: v.Created, Updated: v.Updated,
		Comments: []web.TicketComment{}, Runs: append([]string{}, v.Runs...), Runnable: v.Runnable,
	}
	for _, c := range v.Comments {
		t.Comments = append(t.Comments, web.TicketComment{At: c.At, By: c.By, Text: c.Text})
	}
	if v.Progress != nil {
		t.Progress = &web.TicketProgress{Done: v.Progress.Done, Total: v.Progress.Total}
	}
	return t
}
```

`cmd/aigem/webruns.go`, next to `publishProject`:

```go
// publishTicket names the ticket that changed; a page re-reads its list rather than
// patching it, so the frame carries the address and not the record.
func (n *notifier) publishTicket(project string, v runner.TicketView) {
	n.publish("ticket.updated", map[string]string{"projectId": project, "id": v.ID})
}
```

`cmd/aigem/webbackend.go`: add `tickets *runner.Tickets` to `webBackend` and
`webBackendConfig`, copy it in `newWebBackend` the way `projects` is copied, and in
`Unavailable` add:

```go
	if b.tickets == nil || b.projects == nil {
		out = append(out, "tickets")
	}
```

`cmd/aigem/webcmd.go`, right after the `projects` registry block (inside `if stateDir != ""`
scope or guarded by the same condition):

```go
	var tickets *runner.Tickets
	if stateDir != "" {
		tickets = runner.NewTickets(runner.TicketsConfig{Dir: stateDir, Notify: announce.publishTicket})
	}
```

and pass `tickets: tickets` in the `newWebBackend(webBackendConfig{...})` call.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./cmd/aigem/ ./internal/web/... ./internal/runner/... 2>&1 | tail -8`
Expected: PASS (apart from the known macOS failures listed in Global Constraints).

- [ ] **Step 5: Lint and commit**

```bash
gofmt -l cmd/aigem
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./cmd/aigem/...
git add cmd/aigem/webtickets.go cmd/aigem/webtickets_test.go cmd/aigem/webbackend.go \
  cmd/aigem/webruns.go cmd/aigem/webcmd.go
git commit -m "feat(web): serve tickets from the daemon"
```

---

### Task 5: UI data layer

**Files:**
- Modify: `internal/web/_ui/src/lib/wire.ts`
- Modify: `internal/web/_ui/src/lib/api.ts`
- Modify: `internal/web/_ui/src/state/app.ts`
- Create: `internal/web/_ui/src/state/tickets.ts`
- Test: `internal/web/_ui/src/state/tickets.test.ts`, `internal/web/_ui/src/screens/tickets.test.tsx`
- Modify: `internal/web/_ui/src/test/harness.tsx` (`Daemon.tickets`, a default route)

**Interfaces:**
- Produces (wire.ts):
  ```ts
  export type TicketStatus = 'open' | 'planning' | 'review' | 'ready' | 'running' | 'blocked' | 'done' | 'closed'
  export type TicketComment = { at: string; by: string; text: string }
  export type Ticket = { id: string; repo: string; title: string; body: string; status: TicketStatus;
    parent?: string; dependsOn: string[]; by: string; created?: string; updated?: string;
    comments: TicketComment[]; runs: string[]; runnable: boolean; progress?: { done: number; total: number } }
  export type NewTicket = { repo?: string; title: string; body?: string; parent?: string; dependsOn?: string[] }
  export type TicketPatch = { status?: TicketStatus; dependsOn?: string[] }
  // Feature gains 'tickets'; ControlKind gains TicketUpdated: 'ticket.updated'
  ```
- Produces (api.ts): `api.tickets(project)`, `api.createTicket(project, req)`,
  `api.updateTicket(project, id, patch)`, `api.commentTicket(project, id, text)`,
  `api.deleteTicket(project, id)`.
- Produces (app.ts): `AppState.tickets: Ticket[]`, `refresh.tickets()`.
- Produces (state/tickets.ts):
  ```ts
  export type TicketRow = { ticket: Ticket; depth: 0 | 1; open?: boolean }
  export type TicketFilter = 'active' | 'ready' | 'blocked' | 'all'
  export function treeRows(tickets: Ticket[], filter: TicketFilter, needle: string, collapsed: Set<string>): TicketRow[]
  export function personMoves(status: TicketStatus): TicketStatus[]
  export function waitsFor(t: Ticket, all: Ticket[]): string[]   // ids not yet done
  export function blocks(t: Ticket, all: Ticket[]): Ticket[]
  export const TICKET_STATUS: Record<TicketStatus, { label: string; icon: string; color: string }>
  ```

- [ ] **Step 1: Write the failing tests**

`src/state/tickets.test.ts`:

```ts
import { describe, expect, test } from 'vitest'
import type { Ticket } from '@/lib/wire'
import { blocks, personMoves, treeRows, waitsFor } from './tickets'

const t = (id: string, status: Ticket['status'], extra: Partial<Ticket> = {}): Ticket => ({
  id, repo: '', title: id, body: '', status, dependsOn: [], by: 'you', comments: [], runs: [],
  runnable: false, ...extra,
})

const all = [
  t('TCK-1', 'running', { progress: { done: 1, total: 2 } }),
  t('TCK-2', 'done', { parent: 'TCK-1' }),
  t('TCK-3', 'ready', { parent: 'TCK-1', dependsOn: ['TCK-2', 'TCK-4'] }),
  t('TCK-4', 'running'),
  t('TCK-5', 'closed', { title: 'old thing' }),
]

describe('treeRows', () => {
  test('subtickets follow their parent, one level deeper', () => {
    expect(treeRows(all, 'all', '', new Set()).map((r) => [r.ticket.id, r.depth])).toEqual([
      ['TCK-1', 0], ['TCK-2', 1], ['TCK-3', 1], ['TCK-4', 0], ['TCK-5', 0],
    ])
  })
  test('active hides done and closed, but keeps a parent whose subticket is shown', () => {
    expect(treeRows(all, 'active', '', new Set()).map((r) => r.ticket.id)).toEqual(['TCK-1', 'TCK-3', 'TCK-4'])
  })
  test('a collapsed parent hides its subtickets', () => {
    expect(treeRows(all, 'all', '', new Set(['TCK-1'])).map((r) => r.ticket.id)).toEqual(['TCK-1', 'TCK-4', 'TCK-5'])
  })
  test('the filter matches id and title', () => {
    expect(treeRows(all, 'all', 'old', new Set()).map((r) => r.ticket.id)).toEqual(['TCK-5'])
    expect(treeRows(all, 'all', 'tck-4', new Set()).map((r) => r.ticket.id)).toEqual(['TCK-4'])
  })
})

test('waitsFor lists the dependencies not done yet, blocks lists who waits', () => {
  expect(waitsFor(all[2], all)).toEqual(['TCK-4'])
  expect(blocks(all[3], all).map((x) => x.id)).toEqual(['TCK-3'])
})

test('personMoves mirrors the daemon rules', () => {
  expect(personMoves('open')).toEqual(['ready', 'closed'])
  expect(personMoves('ready')).toEqual(['open', 'done', 'closed'])
  expect(personMoves('blocked')).toEqual(['open', 'ready', 'done', 'closed'])
  expect(personMoves('closed')).toEqual(['open'])
  expect(personMoves('running')).toEqual([])
})
```

`src/screens/tickets.test.tsx` (data-layer part; Tasks 6-7 add more cases to this file):

```tsx
import { act } from '@testing-library/react'
import { expect, test } from 'vitest'
import type { Ticket } from '@/lib/wire'
import { selectProject, store } from '@/state/app'
import { DAEMON_PROJECT, mountApp, waitFor } from '@/test/harness'

const PRJ = { id: 'PRJ-1', name: 'work', dir: '/home/dev/work' }
const META_TICKETS = {
  features: {
    controlSocket: true, runs: true, models: true, skills: true, commands: true, usage: true,
    activity: true, providerLogin: true, projects: true, tickets: true,
  },
}
const ticket = (id: string, extra: Partial<Ticket> = {}): Ticket => ({
  id, repo: '', title: `Title ${id}`, body: '', status: 'open', dependsOn: [], by: 'you',
  comments: [], runs: [], runnable: false, ...extra,
})
const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200 })

test('tickets load for the selected project and follow a switch', async () => {
  const other = { id: 'PRJ-2', name: 'other', dir: '/home/dev/other' }
  await mountApp({
    meta: META_TICKETS,
    projects: [DAEMON_PROJECT, PRJ, other],
    routes: {
      '/api/projects/PRJ-1/tickets': () => json([ticket('TCK-1')]),
      '/api/projects/PRJ-2/tickets': () => json([ticket('TCK-7')]),
    },
  })
  expect(store.get().tickets).toEqual([])
  act(() => selectProject('PRJ-1'))
  await waitFor(() => expect(store.get().tickets.map((t) => t.id)).toEqual(['TCK-1']))
  act(() => selectProject('PRJ-2'))
  expect(store.get().tickets).toEqual([])
  await waitFor(() => expect(store.get().tickets.map((t) => t.id)).toEqual(['TCK-7']))
})

test('ticket.updated rereads, and an older answer does not win over a newer one', async () => {
  let answer: (r: Response) => void = () => {}
  let calls = 0
  const h = await mountApp({
    meta: META_TICKETS,
    projects: [DAEMON_PROJECT, PRJ],
    routes: {
      '/api/projects/PRJ-1/tickets': () => {
        calls++
        if (calls === 2) return new Promise<Response>((r) => (answer = r))
        return json(calls === 1 ? [ticket('TCK-1')] : [ticket('TCK-1'), ticket('TCK-2')])
      },
    },
  })
  act(() => selectProject('PRJ-1'))
  await waitFor(() => expect(store.get().tickets).toHaveLength(1))
  h.publish('ticket.updated', 2, { projectId: 'PRJ-1', id: 'TCK-2' })
  h.publish('ticket.updated', 3, { projectId: 'PRJ-1', id: 'TCK-2' })
  await waitFor(() => expect(store.get().tickets).toHaveLength(2))
  await act(async () => answer(json([])))
  expect(store.get().tickets).toHaveLength(2)
})
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd internal/web/_ui && NODE_OPTIONS=--no-experimental-webstorage npx vitest run src/state/tickets.test.ts src/screens/tickets.test.tsx`
Expected: FAIL, cannot resolve `./tickets` / `store.get().tickets` is undefined.

- [ ] **Step 3: Write the implementation**

`src/lib/wire.ts`: add `| 'tickets'` to `Feature`; add `TicketUpdated: 'ticket.updated',` to
`ControlKind`; add the types from the Interfaces block above (with a one-line doc comment on
`Ticket`: "A project's ticket; `runnable` and `progress` are computed by the daemon.").

`src/lib/api.ts` (import the new types; add inside `api` after `projectRepos`):

```ts
  tickets: (project: string, signal?: AbortSignal) =>
    json<Ticket[]>(`/api/projects/${encodeURIComponent(project)}/tickets`, { signal }),
  createTicket: (project: string, req: NewTicket, signal?: AbortSignal) =>
    json<Ticket>(`/api/projects/${encodeURIComponent(project)}/tickets`, { ...body(req), signal }),
  updateTicket: (project: string, id: string, patch: TicketPatch, signal?: AbortSignal) =>
    json<Ticket>(`/api/projects/${encodeURIComponent(project)}/tickets/${encodeURIComponent(id)}`, {
      ...body(patch),
      method: 'PATCH',
      signal,
    }),
  commentTicket: (project: string, id: string, text: string, signal?: AbortSignal) =>
    json<Ticket>(`/api/projects/${encodeURIComponent(project)}/tickets/${encodeURIComponent(id)}/comments`, {
      ...body({ text }),
      signal,
    }),
  deleteTicket: async (project: string, id: string, signal?: AbortSignal) => {
    await send(`/api/projects/${encodeURIComponent(project)}/tickets/${encodeURIComponent(id)}`, {
      method: 'DELETE',
      signal,
    })
  },
```

`src/state/app.ts`:
- `AppState`: add `tickets: Ticket[]` after `projects`; `initialState()`: `tickets: [],`.
- `setProject`: add `tickets: []` to the patch.
- `selectProject`: after `void refresh.commands()` add `void refresh.tickets()`.
- `refreshAll`: add `refresh.tickets(),` to the `Promise.all` list.
- `refresh.projects`: in the branch that resets a stale selection, the caller already rereads
  skills and commands; in the `ProjectUpdated` control handler add `void refresh.tickets()`
  next to them.
- Control handler: add

```ts
        case ControlKind.TicketUpdated:
          void refresh.tickets()
          break
```

- `refresh` gains, next to `runs`:

```ts
  tickets: () => {
    const gen = ++ticketsGen
    const { project } = store.get()
    if (!project) {
      patch({ tickets: [] })
      return Promise.resolve(false)
    }
    return load('tickets', 'tickets', async () => {
      const tickets = await api.tickets(project)
      return gen === ticketsGen && project === store.get().project ? tickets : store.get().tickets
    })
  },
```

with `let ticketsGen = 0` declared next to `runsGen`.

`src/state/tickets.ts`:

```ts
import type { Ticket, TicketStatus } from '@/lib/wire'

export type TicketRow = { ticket: Ticket; depth: 0 | 1; open?: boolean }
export type TicketFilter = 'active' | 'ready' | 'blocked' | 'all'

export const TICKET_STATUS: Record<TicketStatus, { label: string; icon: string; color: string }> = {
  open: { label: 'open', icon: '○', color: 'var(--fg-muted)' },
  planning: { label: 'planning', icon: '◇', color: 'var(--agent)' },
  review: { label: 'review', icon: '◆', color: 'var(--warning)' },
  ready: { label: 'ready', icon: '○', color: 'var(--success)' },
  running: { label: 'running', icon: '●', color: 'var(--running)' },
  blocked: { label: 'blocked', icon: '!', color: 'var(--danger)' },
  done: { label: 'done', icon: '✓', color: 'var(--fg-subtle)' },
  closed: { label: 'closed', icon: '×', color: 'var(--fg-subtle)' },
}

const MOVES: Record<TicketStatus, TicketStatus[]> = {
  open: ['ready', 'closed'],
  ready: ['open', 'done', 'closed'],
  blocked: ['open', 'ready', 'done', 'closed'],
  done: ['closed'],
  closed: ['open'],
  running: [],
  planning: [],
  review: [],
}

/** The moves the daemon lets a person make from a status. Keep in step with runner.personMove. */
export function personMoves(status: TicketStatus): TicketStatus[] {
  return MOVES[status]
}

function shown(t: Ticket, filter: TicketFilter, needle: string): boolean {
  if (needle && !`${t.id} ${t.title}`.toLowerCase().includes(needle)) return false
  if (filter === 'active') return t.status !== 'done' && t.status !== 'closed'
  if (filter === 'ready') return t.status === 'ready'
  if (filter === 'blocked') return t.status === 'blocked'
  return true
}

/** Top-level tickets in order, each followed by its shown subtickets unless collapsed. */
export function treeRows(tickets: Ticket[], filter: TicketFilter, needle: string, collapsed: Set<string>): TicketRow[] {
  const q = needle.trim().toLowerCase()
  const rows: TicketRow[] = []
  for (const top of tickets.filter((t) => !t.parent)) {
    const kids = tickets.filter((t) => t.parent === top.id && shown(t, filter, q))
    if (!shown(top, filter, q) && kids.length === 0) continue
    const open = !collapsed.has(top.id)
    rows.push({ ticket: top, depth: 0, open: kids.length > 0 ? open : undefined })
    if (open) for (const k of kids) rows.push({ ticket: k, depth: 1 })
  }
  return rows
}

export function waitsFor(t: Ticket, all: Ticket[]): string[] {
  return t.dependsOn.filter((id) => all.find((x) => x.id === id)?.status !== 'done')
}

export function blocks(t: Ticket, all: Ticket[]): Ticket[] {
  return all.filter((x) => x.dependsOn.includes(t.id))
}
```

Note: `MOVES.done` is `['closed']` (the daemon allows done -> closed); the
`personMoves` test above does not list `done`, so it stays valid.

`src/test/harness.tsx`: in the fetch stub, before the final "not stubbed" line, add a default
`if (/^\/api\/projects\/[^/]+\/tickets$/.test(path)) return Promise.resolve(ok([]))` so screens
that read tickets do not raise a banner in unrelated tests.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd internal/web/_ui && npm run check && NODE_OPTIONS=--no-experimental-webstorage npx vitest run`
Expected: typecheck clean; all tests pass, the new ones included.

- [ ] **Step 5: Lint and commit**

```bash
cd internal/web/_ui && npm run lint && cd -
git add internal/web/_ui/src/lib/wire.ts internal/web/_ui/src/lib/api.ts \
  internal/web/_ui/src/state/app.ts internal/web/_ui/src/state/tickets.ts \
  internal/web/_ui/src/state/tickets.test.ts internal/web/_ui/src/screens/tickets.test.tsx \
  internal/web/_ui/src/test/harness.tsx
git commit -m "feat(web): tickets in the UI store"
```

---

### Task 6: Tickets screen

**Files:**
- Create: `internal/web/_ui/src/screens/Tickets.tsx`
- Modify: `internal/web/_ui/src/App.tsx` (import `Tickets` from `@/screens/Tickets` instead of
  `@/screens/Placeholders`)
- Modify: `internal/web/_ui/src/screens/Placeholders.tsx` (remove `Tickets`; keep `Task` until
  Task 7)
- Modify: `internal/web/_ui/src/shell/commands.ts` (palette "New ticket", only with the
  `tickets` feature and a selected project; it navigates to `/tickets` and sets the store flag
  `newTicketOpen`, which the screen reads to open the dialog)
- Modify: `internal/web/_ui/src/state/app.ts` (`newTicketOpen`, `patchNewTicket`)
- Test: `internal/web/_ui/src/screens/tickets.test.tsx` (add cases)

**Interfaces:**
- Consumes: `treeRows`, `waitsFor`, `TICKET_STATUS`, `TicketFilter` (Task 5);
  `DataGrid`, `Column`, `SegmentedControl`, `FilterInput`, `EmptyState`, `Modal` (existing);
  `api.createTicket`, `api.projectRepos`, `refresh.tickets`, `currentProject`, `useApp`.
- Produces: `export function Tickets()`; `AppState.newTicketOpen: boolean`.

- [ ] **Step 1: Write the failing tests** (append to `src/screens/tickets.test.tsx`; move any new
  `import` lines to the top of the file, merged with the existing imports)

```tsx
import { fireEvent, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

async function openTickets(tickets: Ticket[], routes: Record<string, () => Response | Promise<Response>> = {}) {
  const h = await mountApp({
    meta: META_TICKETS,
    projects: [DAEMON_PROJECT, PRJ],
    path: '/tickets',
    routes: { '/api/projects/PRJ-1/tickets': () => json(tickets), ...routes },
  })
  act(() => selectProject('PRJ-1'))
  await screen.findByRole('grid', { name: 'Tickets' })
  return h
}

const PLAN = [
  ticket('TCK-1', { title: 'Delete sessions', status: 'running', progress: { done: 1, total: 2 } }),
  ticket('TCK-2', { title: 'Journal helper', status: 'done', parent: 'TCK-1' }),
  ticket('TCK-3', { title: 'HTTP endpoint', status: 'ready', parent: 'TCK-1', dependsOn: ['TCK-4'] }),
  ticket('TCK-4', { title: 'Runner change', status: 'running' }),
]

test('the tree shows subtickets under their parent with progress and waits', async () => {
  await openTickets(PLAN)
  expect(await screen.findByText('Delete sessions')).toBeInTheDocument()
  expect(screen.getByText('1/2 done')).toBeInTheDocument()
  expect(screen.getByText(/waits for TCK-4/)).toBeInTheDocument()
  // Active is the default and hides the done subticket.
  expect(screen.queryByText('Journal helper')).not.toBeInTheDocument()
  await userEvent.click(screen.getByRole('radio', { name: 'All' }))
  expect(screen.getByText('Journal helper')).toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'Collapse TCK-1' }))
  expect(screen.queryByText('HTTP endpoint')).not.toBeInTheDocument()
})

test('a row opens the ticket page', async () => {
  await openTickets(PLAN)
  await userEvent.click(await screen.findByText('Runner change'))
  expect(window.location.pathname).toBe('/task/TCK-4')
})

test('the new ticket dialog sends the form and shows a refusal', async () => {
  let created = 0
  const h = await openTickets(PLAN, {
    'POST /api/projects/PRJ-1/tickets': () => {
      created++
      return created === 1
        ? new Response('TCK-2 is a subticket and cannot have subtickets', { status: 409 })
        : new Response(JSON.stringify(ticket('TCK-5', { title: 'New one' })), { status: 201 })
    },
  })
  await userEvent.click(screen.getByRole('button', { name: 'New ticket' }))
  await userEvent.type(screen.getByLabelText('Title'), 'New one')
  fireEvent.change(screen.getByLabelText('Parent'), { target: { value: 'TCK-1' } })
  await userEvent.click(screen.getByRole('button', { name: 'Create ticket' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('cannot have subtickets')
  await userEvent.click(screen.getByRole('button', { name: 'Create ticket' }))
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  const post = h.sent.filter((s) => s.method === 'POST').pop()
  expect(JSON.parse(post!.body)).toMatchObject({ title: 'New one', parent: 'TCK-1' })
})

test('without a project the screen says to choose one', async () => {
  await mountApp({ meta: META_TICKETS, projects: [DAEMON_PROJECT, PRJ], path: '/tickets' })
  expect(await screen.findByText('Tickets need a project.')).toBeInTheDocument()
})
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd internal/web/_ui && NODE_OPTIONS=--no-experimental-webstorage npx vitest run src/screens/tickets.test.tsx`
Expected: FAIL, no grid named "Tickets".

- [ ] **Step 3: Write the implementation**

Check `SegmentedControl`'s props (`src/ui/SegmentedControl.tsx`) and `FilterInput`'s props
(`src/ui/FilterInput.tsx`) before using them; the code below assumes
`SegmentedControl({ label, value, onChange, segments: {value, label}[] })` (as used in
`Run.tsx`) and `FilterInput({ value, onChange, label })` (as declared in `FilterInput.tsx`).
`SegmentedControl` renders `role="radio"` items; if it does not, change the test's
`getByRole('radio', …)` to the role it does render.

`src/screens/Tickets.tsx`:

```tsx
import { useEffect, useState } from 'react'
import { api } from '@/lib/api'
import { navigate } from '@/lib/route'
import type { Repository, Ticket } from '@/lib/wire'
import { currentProject, explain, flash, patchNewTicket, refresh, useApp } from '@/state/app'
import { TICKET_STATUS, treeRows, waitsFor } from '@/state/tickets'
import type { TicketFilter, TicketRow } from '@/state/tickets'
import { DataGrid } from '@/ui/DataGrid'
import type { Column } from '@/ui/DataGrid'
import { EmptyState } from '@/ui/EmptyState'
import { FilterInput } from '@/ui/FilterInput'
import { Modal } from '@/ui/Modal'
import { SegmentedControl } from '@/ui/SegmentedControl'

export function Tickets() {
  const { project, tickets, name, newOpen } = useApp((s) => ({
    project: s.project,
    tickets: s.tickets,
    name: currentProject(s)?.name ?? '',
    newOpen: s.newTicketOpen,
  }))
  const [filter, setFilter] = useState<TicketFilter>('active')
  const [needle, setNeedle] = useState('')
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())

  const toggle = (id: string) =>
    setCollapsed((s) => {
      const next = new Set(s)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })

  const columns: Column<TicketRow>[] = [
    {
      key: 'id',
      header: 'Id',
      width: '5.5rem',
      cell: (r) => (
        <span className="flex items-center gap-1 font-mono text-[0.75rem] text-fg-subtle">
          {r.open !== undefined && (
            <button
              type="button"
              aria-label={`${r.open ? 'Collapse' : 'Expand'} ${r.ticket.id}`}
              title={r.open ? 'Collapse' : 'Expand'}
              onClick={(e) => {
                e.stopPropagation()
                toggle(r.ticket.id)
              }}
              className="text-fg-subtle hover:text-fg"
            >
              {r.open ? '▾' : '▸'}
            </button>
          )}
          <span className={r.depth ? 'pl-4' : ''}>{r.ticket.id}</span>
        </span>
      ),
    },
    {
      key: 'title',
      header: 'Title',
      width: 'minmax(12rem, 1fr)',
      cell: (r) => <TitleCell row={r} all={tickets} />,
    },
    { key: 'repo', header: 'Repo', width: '7rem', cell: (r) => r.ticket.repo || name },
    { key: 'status', header: 'Status', width: '6.5rem', cell: (r) => <TicketStatusLabel ticket={r.ticket} /> },
  ]

  return (
    <>
      <div className="flex flex-none flex-wrap items-center gap-2.5 border-b border-line px-4.5 pt-3.5 pb-3">
        <h1 className="m-0 text-[1.0625rem] font-semibold tracking-[-0.015em]">Tickets</h1>
        {project && (
          <>
            <SegmentedControl
              label="Which tickets"
              value={filter}
              onChange={setFilter}
              segments={[
                { value: 'active', label: 'Active' },
                { value: 'ready', label: 'Ready' },
                { value: 'blocked', label: 'Blocked' },
                { value: 'all', label: 'All' },
              ]}
            />
            <FilterInput value={needle} onChange={setNeedle} label="Filter tickets…" />
            <button
              type="button"
              onClick={() => patchNewTicket(true)}
              className="ml-auto h-6.5 rounded-md border border-primary bg-primary px-2.5 text-[0.78125rem] font-medium text-bg hover:brightness-110"
            >
              New ticket
            </button>
          </>
        )}
      </div>
      {!project ? (
        <EmptyState
          title="Tickets need a project."
          detail="Choose or add a project in the sidebar. A ticket belongs to a repository inside it."
        />
      ) : (
        <DataGrid
          label="Tickets"
          columns={columns}
          rows={treeRows(tickets, filter, needle, collapsed)}
          rowKey={(r) => r.ticket.id}
          onSelect={(r) => navigate({ screen: 'task', id: r.ticket.id })}
          minWidth={560}
          empty={<EmptyState title="No tickets here." detail="Create one, or choose another filter." />}
        />
      )}
      {newOpen && project && <NewTicketDialog project={project} tickets={tickets} onClose={() => patchNewTicket(false)} />}
    </>
  )
}

function TitleCell({ row, all }: { row: TicketRow; all: Ticket[] }) {
  const t = row.ticket
  const waits = t.status === 'ready' && !t.runnable ? waitsFor(t, all) : []
  return (
    <span className={`flex min-w-0 items-baseline gap-2 ${row.depth ? 'pl-4' : ''}`}>
      <span className={`truncate ${row.depth ? '' : 'font-medium'}`}>{t.title}</span>
      {t.progress && (
        <span className="flex-none text-[0.75rem] text-fg-subtle">
          {t.progress.done}/{t.progress.total} done
        </span>
      )}
      {waits.length > 0 && (
        <span className="flex-none text-[0.75rem] text-attention">⧗ waits for {waits.join(', ')}</span>
      )}
    </span>
  )
}

export function TicketStatusLabel({ ticket }: { ticket: Ticket }) {
  const s = TICKET_STATUS[ticket.status]
  return (
    <span className="text-[0.78125rem] whitespace-nowrap" style={{ color: s.color }}>
      <span aria-hidden="true">{s.icon}</span> {s.label}
    </span>
  )
}

const INPUT =
  'rounded-md border border-line bg-bg px-2 py-1 text-[0.8125rem] text-fg outline-none focus:border-primary'

export function NewTicketDialog({
  project,
  tickets,
  parent: fixedParent,
  onClose,
}: {
  project: string
  tickets: Ticket[]
  parent?: string
  onClose: () => void
}) {
  const [repos, setRepos] = useState<Repository[]>([])
  const [repo, setRepo] = useState('')
  const [title, setTitle] = useState('')
  const [body, setBody] = useState('')
  const [parent, setParent] = useState(fixedParent ?? '')
  const [deps, setDeps] = useState<string[]>([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const ready = title.trim() !== '' && !busy

  useEffect(() => {
    const abort = new AbortController()
    void api.projectRepos(project, abort.signal).then(setRepos, () => setRepos([]))
    return () => abort.abort()
  }, [project])

  const create = async () => {
    if (!ready) return
    setBusy(true)
    setError('')
    try {
      const t = await api.createTicket(project, {
        title: title.trim(),
        ...(repo && { repo }),
        ...(body.trim() && { body }),
        ...(parent && { parent }),
        ...(deps.length > 0 && { dependsOn: deps }),
      })
      await refresh.tickets()
      flash(`Created ${t.id}`)
      onClose()
    } catch (err) {
      setError(explain(err))
      setBusy(false)
    }
  }

  return (
    <Modal
      title="New ticket"
      onClose={onClose}
      width={560}
      confirm={{ label: busy ? 'Creating…' : 'Create ticket', onClick: () => void create(), disabled: !ready }}
    >
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          void create()
        }}
      >
        <label className="flex flex-col gap-1 text-[0.78125rem] text-fg-subtle">
          Title
          <input value={title} onChange={(e) => setTitle(e.target.value)} className={INPUT} />
        </label>
        <label className="flex flex-col gap-1 text-[0.78125rem] text-fg-subtle">
          Repository
          <select value={repo} onChange={(e) => setRepo(e.target.value)} className={INPUT}>
            <option value="">the project itself</option>
            {repos.filter((r) => r.name).map((r) => (
              <option key={r.name} value={r.name}>
                {r.name}
              </option>
            ))}
          </select>
        </label>
        {!fixedParent && (
          <label className="flex flex-col gap-1 text-[0.78125rem] text-fg-subtle">
            Parent
            <select value={parent} onChange={(e) => setParent(e.target.value)} className={INPUT}>
              <option value="">none (top-level)</option>
              {tickets.filter((t) => !t.parent).map((t) => (
                <option key={t.id} value={t.id}>
                  {t.id} {t.title}
                </option>
              ))}
            </select>
          </label>
        )}
        <label className="flex flex-col gap-1 text-[0.78125rem] text-fg-subtle">
          Waits for
          <select
            multiple
            value={deps}
            onChange={(e) => setDeps(Array.from(e.target.selectedOptions, (o) => o.value))}
            className={`${INPUT} h-24`}
          >
            {tickets.map((t) => (
              <option key={t.id} value={t.id}>
                {t.id} {t.title}
              </option>
            ))}
          </select>
        </label>
        <label className="flex flex-col gap-1 text-[0.78125rem] text-fg-subtle">
          Description (markdown)
          <textarea value={body} onChange={(e) => setBody(e.target.value)} rows={6} className={INPUT} />
        </label>
        {error && (
          <p role="alert" className="m-0 text-[0.78125rem] text-attention">
            {error}
          </p>
        )}
      </form>
    </Modal>
  )
}
```

`src/state/app.ts`: add `newTicketOpen: boolean` to `AppState` (initial `false`) and

```ts
export function patchNewTicket(open: boolean) {
  patch({ newTicketOpen: open })
}
```

`src/shell/commands.ts`: add a palette command next to "New session", shown when
`has('tickets') && store.get().project`:

```ts
    {
      id: 'new-ticket',
      label: 'New ticket',
      hint: 'in the selected project',
      icon: '+',
      group: 'Create',
      run: () => {
        setPalette(false)
        navigate({ screen: 'tickets' })
        patchNewTicket(true)
      },
    },
```

(Follow the shape of the existing commands in that file exactly; if its items carry a
`when`/`available` field instead of being filtered at the call site, use that.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd internal/web/_ui && npm run check && NODE_OPTIONS=--no-experimental-webstorage npx vitest run`
Expected: all pass.

- [ ] **Step 5: Lint and commit**

```bash
cd internal/web/_ui && npm run lint && cd -
git add internal/web/_ui/src/screens/Tickets.tsx internal/web/_ui/src/App.tsx \
  internal/web/_ui/src/screens/Placeholders.tsx internal/web/_ui/src/shell/commands.ts \
  internal/web/_ui/src/state/app.ts internal/web/_ui/src/screens/tickets.test.tsx
git commit -m "feat(web): tickets screen with the tree table and a new ticket dialog"
```

---

### Task 7: Ticket page

**Files:**
- Create: `internal/web/_ui/src/screens/Task.tsx`
- Modify: `internal/web/_ui/src/App.tsx` (import `Task` from `@/screens/Task`)
- Delete: `internal/web/_ui/src/screens/Placeholders.tsx` (nothing imports it any more; move any
  test that imported it, e.g. in `screens.test.tsx` "every placeholder screen says what it is
  waiting for", to assert the new empty states instead)
- Test: `internal/web/_ui/src/screens/tickets.test.tsx` (add cases)

**Interfaces:**
- Consumes: `TicketStatusLabel`, `NewTicketDialog` (Task 6); `personMoves`, `waitsFor`,
  `blocks`, `treeRows` (Task 5); `Back`, `Markdown`, `SegmentedControl`, `EmptyState`;
  `api.updateTicket`, `api.commentTicket`, `refresh.tickets`.
- Produces: `export function Task()`.

- [ ] **Step 1: Write the failing tests** (append to `src/screens/tickets.test.tsx`; move any new
  `import` lines to the top of the file, merged with the existing imports)

```tsx
async function openTask(id: string, tickets: Ticket[], routes: Record<string, () => Response | Promise<Response>> = {}) {
  const h = await mountApp({
    meta: META_TICKETS,
    projects: [DAEMON_PROJECT, PRJ],
    path: `/task/${id}`,
    routes: { '/api/projects/PRJ-1/tickets': () => json(tickets), ...routes },
  })
  act(() => selectProject('PRJ-1'))
  await screen.findByRole('heading', { level: 1, name: tickets.find((t) => t.id === id)!.title })
  return h
}

test('a subticket page shows its parent, waits, blocks and only allowed moves', async () => {
  const tickets = [
    ...PLAN,
    ticket('TCK-5', { title: 'Trash button', status: 'ready', parent: 'TCK-1', dependsOn: ['TCK-3'] }),
  ]
  const h = await openTask('TCK-3', tickets, {
    'PATCH /api/projects/PRJ-1/tickets/TCK-3': () => json(ticket('TCK-3', { status: 'open' })),
  })
  expect(screen.getByRole('button', { name: '‹ TCK-1' })).toBeInTheDocument()
  expect(screen.getByText(/waits for TCK-4/)).toBeInTheDocument()
  expect(screen.getByRole('link', { name: 'TCK-5' })).toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Back to open' })).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'Reopen' })).not.toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'Back to open' }))
  const patchReq = h.sent.find((s) => s.method === 'PATCH')
  expect(JSON.parse(patchReq!.body)).toEqual({ status: 'open' })
})

test('adding a dependency that makes a cycle shows the daemon sentence', async () => {
  await openTask('TCK-4', PLAN, {
    'PATCH /api/projects/PRJ-1/tickets/TCK-4': () =>
      new Response('that makes a cycle: TCK-4 -> TCK-3 -> TCK-4', { status: 409 }),
  })
  fireEvent.change(screen.getByLabelText('Add dependency'), { target: { value: 'TCK-3' } })
  expect(await screen.findByRole('alert')).toHaveTextContent('makes a cycle')
})

test('a parent page lists its subtickets and has no status buttons', async () => {
  await openTask('TCK-1', PLAN)
  expect(screen.getByText('1/2 done')).toBeInTheDocument()
  expect(screen.getByText('HTTP endpoint')).toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Add subticket' })).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'Close' })).not.toBeInTheDocument()
})

test('a comment is sent and the discussion shows who wrote what', async () => {
  const withComments = PLAN.map((t) =>
    t.id === 'TCK-4' ? { ...t, by: 'run RUN-12', comments: [{ at: '2026-10-09T10:00:00Z', by: 'run RUN-12', text: 'Split from TCK-1.' }] } : t,
  )
  const h = await openTask('TCK-4', withComments, {
    'POST /api/projects/PRJ-1/tickets/TCK-4/comments': () =>
      new Response(JSON.stringify(withComments[3]), { status: 201 }),
  })
  expect(screen.getByText(/created by run RUN-12/)).toBeInTheDocument()
  await userEvent.click(screen.getByRole('radio', { name: /Discussion/ }))
  expect(screen.getByText('Split from TCK-1.')).toBeInTheDocument()
  await userEvent.type(screen.getByLabelText('Comment'), 'Keep the 404')
  await userEvent.click(screen.getByRole('button', { name: 'Send comment' }))
  await waitFor(() => expect(h.sent.some((s) => s.path.endsWith('/comments'))).toBe(true))
})

test('an unknown ticket says so', async () => {
  await mountApp({ meta: META_TICKETS, projects: [DAEMON_PROJECT, PRJ], path: '/task/TCK-99' })
  act(() => selectProject('PRJ-1'))
  expect(await screen.findByText('No such ticket.')).toBeInTheDocument()
})
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd internal/web/_ui && NODE_OPTIONS=--no-experimental-webstorage npx vitest run src/screens/tickets.test.tsx`
Expected: FAIL on the new cases (the placeholder renders).

- [ ] **Step 3: Write the implementation**

`src/screens/Task.tsx`:

```tsx
import { useState } from 'react'
import { api } from '@/lib/api'
import { format, getRoute, navigate } from '@/lib/route'
import type { Ticket, TicketStatus } from '@/lib/wire'
import { currentProject, explain, refresh, setBanner, useApp } from '@/state/app'
import { blocks, personMoves, waitsFor } from '@/state/tickets'
import { Back } from '@/ui/Back'
import { EmptyState } from '@/ui/EmptyState'
import { Markdown } from '@/ui/Markdown'
import { SegmentedControl } from '@/ui/SegmentedControl'
import { NewTicketDialog, TicketStatusLabel } from './Tickets'

const MOVE_LABEL: Record<TicketStatus, string> = {
  open: 'Back to open',
  ready: 'Mark ready',
  done: 'Mark done',
  closed: 'Close',
  planning: '',
  review: '',
  running: '',
  blocked: '',
}

const BUTTON =
  'h-6.5 rounded-md border border-line px-2.5 text-[0.78125rem] text-fg-muted hover:border-line-strong hover:text-fg'

export function Task() {
  const { project, tickets, name } = useApp((s) => ({
    project: s.project,
    tickets: s.tickets,
    name: currentProject(s)?.name ?? '',
  }))
  const id = getRoute().id ?? ''
  const t = tickets.find((x) => x.id === id)
  const [tab, setTab] = useState<'overview' | 'discussion' | 'runs'>('overview')
  const [error, setError] = useState('')
  const [adding, setAdding] = useState(false)

  if (!project) return <EmptyState title="Tasks need a project." detail="Choose a project in the sidebar." />
  if (!t) {
    return (
      <EmptyState
        title="No such ticket."
        detail={`${id} is not a ticket of ${name}.`}
        action={{ label: 'Back to tickets', onClick: () => navigate({ screen: 'tickets' }) }}
      />
    )
  }

  const kids = tickets.filter((x) => x.parent === t.id)
  const isParent = kids.length > 0
  const waits = waitsFor(t, tickets)

  const change = async (patch: { status?: TicketStatus; dependsOn?: string[] }) => {
    setError('')
    try {
      await api.updateTicket(project, t.id, patch)
      await refresh.tickets()
    } catch (err) {
      setError(explain(err))
    }
  }

  return (
    <>
      <div className="flex-none border-b border-line px-4.5 pt-3.5 pb-3">
        <div className="flex flex-wrap items-center gap-2.5">
          <Back label={t.parent || 'Tickets'} to={t.parent ? { screen: 'task', id: t.parent } : { screen: 'tickets' }} />
          <span className="font-mono text-[0.8125rem] text-fg-subtle">{t.id}</span>
          <h1 className="m-0 text-[1rem] font-semibold">{t.title}</h1>
          <TicketStatusLabel ticket={t} />
          {waits.length > 0 && <span className="text-[0.75rem] text-attention">⧗ waits for {waits.join(', ')}</span>}
          {!isParent && (
            <div className="ml-auto flex gap-1.5">
              {personMoves(t.status).map((to) => (
                <button key={to} type="button" onClick={() => void change({ status: to })} className={BUTTON}>
                  {MOVE_LABEL[to]}
                </button>
              ))}
            </div>
          )}
        </div>
        <div className="mt-1.5 font-mono text-[0.71875rem] text-fg-subtle">
          {t.repo || name} · created by {t.by || 'you'}
        </div>
        {error && (
          <p role="alert" className="m-0 mt-1.5 text-[0.78125rem] text-attention">
            {error}
          </p>
        )}
      </div>

      <div className="flex min-h-0 flex-1">
        <div className="flex min-w-0 flex-1 flex-col overflow-y-auto px-4.5 py-2.5">
          <SegmentedControl
            label="What to show"
            value={tab}
            onChange={setTab}
            segments={[
              { value: 'overview', label: 'Overview' },
              { value: 'discussion', label: `Discussion ${t.comments.length}` },
              { value: 'runs', label: `Runs ${t.runs.length}` },
            ]}
          />
          {tab === 'overview' && (
            <div className="mt-3">
              {t.body ? <Markdown source={t.body} /> : <p className="text-fg-subtle">No description.</p>}
              {isParent && (
                <div className="mt-4">
                  <div className="mb-1.5 flex items-center gap-2">
                    <span className="text-[0.6875rem] tracking-[.07em] text-fg-subtle uppercase">Subtickets</span>
                    <span className="text-[0.75rem] text-fg-subtle">
                      {t.progress?.done ?? 0}/{t.progress?.total ?? kids.length} done
                    </span>
                    <button type="button" onClick={() => setAdding(true)} className={`ml-auto ${BUTTON}`}>
                      Add subticket
                    </button>
                  </div>
                  {kids.map((k) => (
                    <button
                      key={k.id}
                      type="button"
                      onClick={() => navigate({ screen: 'task', id: k.id })}
                      className="flex w-full items-center gap-2.5 border-b border-line py-1.5 text-left hover:bg-s0"
                    >
                      <span className="font-mono text-[0.75rem] text-fg-subtle">{k.id}</span>
                      <span className="flex-1 truncate">{k.title}</span>
                      <TicketStatusLabel ticket={k} />
                    </button>
                  ))}
                </div>
              )}
            </div>
          )}
          {tab === 'discussion' && <Discussion project={project} ticket={t} />}
          {tab === 'runs' && <p className="mt-3 text-fg-subtle">Runs on tickets arrive in the next part.</p>}
        </div>

        <aside className="flex-none overflow-y-auto border-l border-line bg-shell px-3.5 py-3 text-[0.75rem]" style={{ width: 'var(--panel)' }}>
          {t.parent && (
            <Section title="Parent">
              <TicketLink id={t.parent} all={tickets} />
            </Section>
          )}
          {!isParent && (
            <>
              <Section title="Waits for">
                {t.dependsOn.map((d) => (
                  <div key={d} className="flex items-center gap-2 py-0.5">
                    <TicketLink id={d} all={tickets} />
                    <button
                      type="button"
                      aria-label={`Stop waiting for ${d}`}
                      title="Remove dependency"
                      onClick={() => void change({ dependsOn: t.dependsOn.filter((x) => x !== d) })}
                      className="ml-auto text-fg-subtle hover:text-danger"
                    >
                      ×
                    </button>
                  </div>
                ))}
                <label className="mt-1 flex flex-col gap-1 text-fg-subtle">
                  Add dependency
                  <select
                    value=""
                    onChange={(e) => e.target.value && void change({ dependsOn: [...t.dependsOn, e.target.value] })}
                    className="rounded-md border border-line bg-bg px-1.5 py-0.5 text-fg"
                  >
                    <option value="">choose a ticket…</option>
                    {tickets
                      .filter((x) => x.id !== t.id && !t.dependsOn.includes(x.id))
                      .map((x) => (
                        <option key={x.id} value={x.id}>
                          {x.id} {x.title}
                        </option>
                      ))}
                  </select>
                </label>
              </Section>
              <Section title="Blocks">
                {blocks(t, tickets).map((b) => (
                  <div key={b.id} className="py-0.5">
                    <TicketLink id={b.id} all={tickets} />
                  </div>
                ))}
              </Section>
            </>
          )}
          <Section title="Repository">{t.repo || name}</Section>
        </aside>
      </div>
      {adding && <NewTicketDialog project={project} tickets={tickets} parent={t.id} onClose={() => setAdding(false)} />}
    </>
  )
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="mb-3">
      <div className="mb-1 text-[0.65625rem] tracking-[.06em] text-fg-subtle uppercase">{title}</div>
      {children}
    </div>
  )
}

function TicketLink({ id, all }: { id: string; all: Ticket[] }) {
  const t = all.find((x) => x.id === id)
  return (
    <span className="inline-flex items-center gap-1.5">
      <a
        href={format({ screen: 'task', id })}
        onClick={(e) => {
          e.preventDefault()
          navigate({ screen: 'task', id })
        }}
        className="rounded-[0.1875rem] border border-line-strong px-1 font-mono text-[0.6875rem] text-fg-muted"
      >
        {id}
      </a>
      {t && <TicketStatusLabel ticket={t} />}
    </span>
  )
}

function Discussion({ project, ticket }: { project: string; ticket: Ticket }) {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const send = async () => {
    if (!text.trim() || busy) return
    setBusy(true)
    try {
      await api.commentTicket(project, ticket.id, text)
      setText('')
      await refresh.tickets()
    } catch (err) {
      setBanner(explain(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="mt-3">
      {ticket.comments.map((c, i) => (
        <div key={i} className="border-b border-line py-2">
          <div className="font-mono text-[0.6875rem]" style={{ color: c.by === 'you' ? 'var(--primary)' : 'var(--agent)' }}>
            {c.by}
          </div>
          <Markdown source={c.text} />
        </div>
      ))}
      <div className="mt-2 flex gap-2">
        <textarea
          aria-label="Comment"
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) void send()
          }}
          rows={3}
          placeholder="Write a comment… ⌘↵ to send"
          className="flex-1 rounded-md border border-line bg-bg px-2 py-1 text-[0.8125rem] outline-none focus:border-primary"
        />
        <button type="button" onClick={() => void send()} disabled={!text.trim() || busy} className={BUTTON}>
          Send comment
        </button>
      </div>
    </div>
  )
}
```

Check before use: `EmptyState`'s `action` prop shape (`src/ui/EmptyState.tsx`; `Run.tsx` uses
`action={{ label, onClick }}`), `Markdown`'s props (`{ source, className? }`), and `Back`'s
props (`{ label, to }`; it renders `‹ {label}`, which the test's `'‹ TCK-1'` relies on). If
`getRoute()` is not reactive inside the screen, read the id the way `Run.tsx` reads its run id
(it is passed from `App.tsx`); follow that instead.

`src/App.tsx`: import `Task` from `@/screens/Task` and `Tickets` from `@/screens/Tickets`;
remove the `Placeholders` import and delete `src/screens/Placeholders.tsx`. Update the old test
"every placeholder screen says what it is waiting for" in `src/screens/screens.test.tsx`: for
`tickets` and `task` with no project selected, it should still find the `h1`/empty state
("Tickets need a project." and "Tasks need a project.").

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd internal/web/_ui && npm run lint && npm run check && NODE_OPTIONS=--no-experimental-webstorage npx vitest run`
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add internal/web/_ui/src/screens/Task.tsx internal/web/_ui/src/App.tsx \
  internal/web/_ui/src/screens/Placeholders.tsx internal/web/_ui/src/screens/screens.test.tsx \
  internal/web/_ui/src/screens/tickets.test.tsx
git commit -m "feat(web): ticket page with dependencies, subtickets and discussion"
```

---

### Task 8: Docs, full check, push

**Files:**
- Modify: `docs/web.md` (routes table, a "Tickets" section, `ticket.updated`, feature
  `tickets`, activity kinds)
- Modify: `CHANGELOG.md` (one line under Unreleased/Added for `aigem web`)

- [ ] **Step 1: Document**

In `docs/web.md`, add the six routes from the spec's API table to the routes table, a short
"Tickets" section (statuses, person moves, one level, derived parent status, runnable, 404/409/400
rules, 64 KiB body and 16 KiB comment caps), `ticket.updated {projectId, id}` to the control
frames list, `tickets` to the feature map list, and `ticket.created` / `ticket.closed` to the
activity kinds. Max 100 characters per line, hyphens not em dashes. In `CHANGELOG.md` add
under Unreleased/Added: "Web UI: tickets per project with subtickets and dependencies (part 1
of agent tickets)."

- [ ] **Step 2: Full check**

```bash
go build ./... && go test ./... 2>&1 | grep -v '^ok' | head -20
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./...
cd internal/web/_ui && npm run lint && npm run check && \
  NODE_OPTIONS=--no-experimental-webstorage npx vitest run && cd -
make web && make build
```

Expected: only the known macOS failures and the old QF1003.

- [ ] **Step 3: Manual check in the browser**

Start `bin/aigem web --addr 127.0.0.1:7799` (stop an older one on that port first), select a
project, create a parent and two subtickets with a dependency, move statuses, add a dependency
that makes a cycle, comment, open the same page in a second tab and confirm live updates.

- [ ] **Step 4: Commit and push**

```bash
git add docs/web.md CHANGELOG.md
git commit -m "docs: tickets API and UI"
git push origin main
```
