# Agent Tickets Part 4 Implementation Plan: Dispatcher

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Each project gets a number of slots; while fewer of its tickets are `running` than
there are slots, the daemon starts the oldest runnable ticket by itself, and a person can pause
the queue.

**Architecture:** `runner.Project` gains `Slots` and `Paused`, set by `Projects.SetDispatch`.
`runner.Tickets` gains `Block` for a ready ticket that cannot start. A new
`runner.Dispatcher` runs passes on one goroutine: each pass reads every project with free slots
and calls part 2's `TicketRuns.Start` on the oldest runnable tickets. `Wake` is a non-blocking
send on a channel of size 1, so the registries' notify callbacks can call it while a pass is
running (a pass itself causes notifications) without a deadlock. `cmd/aigem` builds it after
`Recover`, wakes it from the ticket and project notifications, records `ticket.started`, and
serves `PATCH /api/projects/{id}`; the Tickets screen gets the Slots select, Pause / Resume, the
count and the badge.

**Tech Stack:** Go 1.26 (`net/http` ServeMux patterns), git >= 2.28 for the test repositories,
React 19 + TypeScript, Tailwind v4, vitest + Testing Library.

**Spec:** `docs/superpowers/specs/2026-10-10-agent-tickets-part4-dispatcher-design.md` (part 4;
context: `docs/superpowers/specs/2026-10-09-agent-tickets-design.md` (part 1),
`docs/superpowers/specs/2026-10-09-agent-tickets-part2-runs-design.md` (part 2) and
`docs/superpowers/specs/2026-10-10-agent-tickets-part3-planner-design.md` (part 3))

## Global Constraints

- Work directly on `main` in `/Users/gigovich/work/gigovich/aigem`; commit after every task.
  Do not push.
- Simplicity is the top criterion: minimal code, no new abstractions, layers or dependencies.
  Follow the idioms already in the file you touch.
- No code comments unless they explain hidden logic; keep them short.
- Go lines <= 120 characters, `gofmt -w` on touched Go files. Markdown lines <= 100 characters
  with hard breaks (table rows in `docs/web.md` may be longer, as the existing ones are). No em
  dashes anywhere; use `-`.
- Slots are 0 to 8 per project, default 0 (off). Out of range, exactly:
  `slots must be between 0 and 8` (400 over HTTP).
- A slot is a leaf ticket with status `running`, whoever started it. Blocked tickets, planner
  runs (`planning`) and parents (derived status) take no slot. The daemon's limit of 32 live
  runs still applies.
- The dispatcher never plans. It never stops a run: Pause and lower slots only stop new starts.
- A failed start, exactly: a `*TicketRefusal` (the ticket's own fault: a path in the way, not a
  checkout, the worktree could not be added, a bad repository) moves the ticket `ready` ->
  `blocked` with the comment `the dispatcher could not start it: <error>` by `aigem`. Any other
  error, `ErrTooManyRuns` included, blocks nothing: it is logged and the dispatcher holds (the
  pass ends, wakes are ignored until the next 30 s tick). `ErrRunsClosed` and `Close` stop the
  dispatcher, and a start that `Close` cancelled never blocks its ticket. A ticket that is no
  longer runnable or that another Start holds is left alone.
- `Tickets.Block` refusals, exactly: `<id> is <status>, not ready`, `<id> is not runnable`.
- Route: `PATCH /api/projects/{id}` `{slots?, paused?}`; 200 with the project, 400 out of range
  or unknown field, 404 unknown project, 405 other methods. `GET /api/projects` returns `slots`
  and `paused` with every project.
- Activity: `ticket.started` with text `Started TCK-n: <title>`, only for the dispatcher's
  starts; a failed start is the existing `ticket.blocked`.
- UI sizes in rem / Tailwind spacing only (no new px); no `cursor-*` classes; every control is
  keyboard-operable and labelled. Do not run prettier.
- Go tests that touch state set `XDG_STATE_HOME` to `t.TempDir()` (the part 2 `newFixture` does).
  Git tests use the part 2 `gitRepo` helper (real repositories in `t.TempDir()`).
- Commands: `gofmt -w <touched files>`; `go test ./...` and
  `go test -race ./internal/runner/...`; lint
  `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./...` (the old
  QF1003 in `internal/tui/model_add.go` is not ours); UI in `internal/web/_ui`: `npm run lint`,
  `npm run check` (typecheck), `NODE_OPTIONS=--no-experimental-webstorage npx vitest run`.
- Known failures on macOS, also on `main`, not ours:
  `TestLoadRefusesAnUnresolvableWorkingDirectory`,
  `TestLoadErrorNamesTheDirectoryAndKeepsTheCause` (runner), `TestSetupSandboxIsPrivate`
  (testenv). Flaky under `-race`: `TestSpecEvictionSettingsReachTheAgent`,
  `TestSpecAutoCompactionReachesTheAgent`, `TestCompactionInheritsTheSessionsContextWindow`
  (runner), `TestSkillsBrowserAndDispatch` (tui).

### Decisions and deviations

- Decision: `SetDispatch(id string, slots *int, paused *bool)`; nil keeps a setting. The PATCH
  merge happens under the registry lock, so two tabs never lose each other's field, and the
  adapter is one call.
- Decision: the running count skips parents (`Progress != nil`): a parent's `running` is
  derived from a running subticket that is already counted.
- Deviation from spec: `Block` also refuses a ready ticket that is not runnable
  (`<id> is not runnable`). This is how "a refusal because the ticket is no longer runnable is
  ignored" is decided atomically, inside the registry lock, with no extra read.
- Decision: `TicketRuns.Start`'s `<id> is already starting` refusal becomes the unexported type
  `startingRefusal` (it unwraps to the same `*TicketRefusal`, so HTTP is unchanged). The
  dispatcher leaves such a ticket alone: a person's "Run" in flight is not blocked.
- Decision: only a `*TicketRefusal` blocks. Other errors (a project env that does not load, a
  model or login error, `Runs.Create` errors, `ErrTooManyRuns`) are not the ticket's fault, so
  the dispatcher logs them and holds until its next tick. Without the hold this is a hot loop:
  an env load failure calls `Projects.notify`, which wakes the dispatcher, and every start runs
  git `prepare` before `ErrTooManyRuns`. The hold is a `held bool` field touched only on the
  dispatcher goroutine: `pass` sets it and returns at once while it is set, the tick clears it.
- Decision: no live-run check in `TicketRuns.Start` before `prepare`: it needs a new `Runs`
  method, and the hold already limits it to one `prepare` (undone) per 30 s while full.
- Deviation from spec: no "starting" set in the dispatcher. Passes run one at a time on its one
  goroutine and `Start` is synchronous, so it cannot start a ticket twice; `TicketRuns.starting`
  already guards against a person's Run.
- Deviation from spec: `TicketRunsConfig.Finished` does not call `Wake` itself. Every outcome
  goes through `Tickets.Finish`, whose ticket notification already wakes the dispatcher.
- Decision: after `Block` the dispatcher calls `TicketRuns`' own `finished` callback, which
  `cmd/aigem` already wires to `ticketFinished` (writes `ticket.blocked`). No extra config.
- Decision: `DispatcherConfig.Started func(project string, v TicketView, run string)` gets the
  list view and the run id from `Start`; no extra `tickets.Get`. It is required (no nil
  default).
- Decision: `Start` runs with the dispatcher's context, which `Close` cancels; `Close` then
  waits for the pass. A start that fails while stopped ends the pass and never blocks its
  ticket. A person's "Run" during a pass can briefly put one ticket over the slots (the pass
  counted before it); documented, not prevented.
- Decision: the 30 s safety tick is a constant, not injectable. Tests build the dispatcher
  without its goroutine (`newDispatcher`), call `pass` synchronously and clear `held` where the
  tick would. One test runs the goroutine for `Wake` and `Close`.
- Decision: "oldest first by ticket number" is the list order: tickets are appended in creation
  order and ids only grow.
- Decision: UI errors go to the page banner (`setBanner`, the app's `role="alert"`); "n of N
  running" shows only while slots > 0; Pause / Resume shows always.

## Review Focus

- A person presses "Run" on the very ticket the dispatcher takes next: one run, the ticket is
  not blocked. (Task 3 `TestADispatcherLeavesATicketAPersonIsStarting`.)
- The daemon holds 32 live runs: nothing is blocked, a wake does not retry, the tick does.
  (Task 3 `TestTheDispatcherHoldsWhileTheDaemonIsFull`.)
- An error that is not the ticket's fault (env load, model, `Runs.Create`): nothing is blocked
  and there is no hot loop through `Projects.notify` -> `Wake`. (Task 3
  `TestAFailureNotOfTheTicketHoldsUntilTheTick`.)
- A ticket finishes: its slot is filled right away through the ticket notification, without
  the 30 s tick, and the notify path does not deadlock with a pass. (Task 2
  `TestAFinishedTicketFreesItsSlotForTheNext`.)
- `Close` during a start: the cancelled start never blocks its ticket. (Task 3
  `TestAStoppedDispatcherNeverBlocksATicket`.)
- After a restart `Recover` leaves the old running tickets `blocked`: the dispatcher must not
  start them again, only the next ready ones. (Task 2
  `TestTheDispatcherFillsFreeSlotsOldestFirst`: a blocked ticket keeps its status.)
- A broken ticket (path in the way, not a checkout, worktree add) is blocked once with the
  reason and is not retried; the next ticket still starts in the same pass. (Task 3
  `TestADispatcherBlocksABrokenTicketOnceAndGoesOn`.)
- A person moves a blocked ticket back to ready while its run is still live: the dispatcher
  blocks it again with the live-run reason (`<id> has a live run RUN-n; stop it first`).
  Acceptable and documented in `docs/web.md`.

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/runner/projects.go` | `Project.Slots`, `Paused`; `SetDispatch` |
| `internal/runner/tickets.go` | `Block` |
| `internal/runner/ticketruns.go` | `startingRefusal` for "already starting" |
| `internal/runner/dispatcher.go` (new) | `Dispatcher`: passes, `Wake`, `Close` |
| `internal/web/api_projects.go`, `server.go` | `ProjectPatch`, `UpdateProject`, the route |
| `cmd/aigem/webprojects.go`, `webtickets.go` | adapter, `ticketStarted` |
| `cmd/aigem/webruns.go`, `webcmd.go` | notifier wakes the dispatcher; wiring |
| `internal/web/_ui/src/lib/wire.ts`, `api.ts` | `slots`, `paused`, `updateProject` |
| `internal/web/_ui/src/screens/Tickets.tsx`, `Task.tsx` | dispatcher controls; export `BUTTON` |
| `docs/web.md`, `CHANGELOG.md` | docs |

---

### Task 1: Slots and Pause on a project

**Files:**
- Modify: `internal/runner/projects.go` (`Project`, new `maxSlots`, `SetDispatch`)
- Test: `internal/runner/projects_test.go` (append)

**Interfaces:**
- Produces:
  ```go
  type Project struct {
  	// ...existing fields...
  	Slots  int  `json:"slots,omitempty"`
  	Paused bool `json:"paused,omitempty"`
  }
  const maxSlots = 8
  func (p *Projects) SetDispatch(id string, slots *int, paused *bool) (ProjectView, error)
  ```
  `ProjectView` embeds `Project`, so `v.Slots` and `v.Paused` reach every view and the notify
  callback. Errors: `runner: slots must be between 0 and 8` (a plain error; the web adapter
  strips the prefix), `ErrNoProject`, or the save error (the change is rolled back).

- [ ] **Step 1: Write the failing test**

Append to `internal/runner/projects_test.go` (package `runner_test`; `newProjects`,
`addProject`, `errors`, `filepath` are already there):

```go
func TestDispatchSettingsAreCheckedSavedAndAnnounced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	var seen []runner.ProjectView
	p := newProjects(t, path, func(v runner.ProjectView) { seen = append(seen, v) })
	addProject(t, p, t.TempDir(), "")

	slots, paused := 3, true
	v, err := p.SetDispatch("PRJ-1", &slots, &paused)
	if err != nil || v.Slots != 3 || !v.Paused {
		t.Fatalf("SetDispatch = %+v, %v", v, err)
	}
	if last := seen[len(seen)-1]; last.Slots != 3 || !last.Paused {
		t.Errorf("announced %+v, want the new settings", last)
	}
	resume := false
	if v, err = p.SetDispatch("PRJ-1", nil, &resume); err != nil || v.Slots != 3 || v.Paused {
		t.Errorf("resume = %+v, %v, want the slots kept", v, err)
	}
	for _, bad := range []int{-1, 9} {
		_, err := p.SetDispatch("PRJ-1", &bad, nil)
		if err == nil || err.Error() != "runner: slots must be between 0 and 8" {
			t.Errorf("slots %d = %v", bad, err)
		}
	}
	if _, err := p.SetDispatch("PRJ-9", &slots, nil); !errors.Is(err, runner.ErrNoProject) {
		t.Errorf("unknown project = %v, want ErrNoProject", err)
	}

	again := newProjects(t, path, nil)
	if got, err := again.Get("PRJ-1"); err != nil || got.Slots != 3 || got.Paused {
		t.Errorf("after a restart = %+v, %v", got, err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/runner/ -run TestDispatchSettingsAreCheckedSavedAndAnnounced`
Expected: FAIL to compile: `p.SetDispatch undefined`.

- [ ] **Step 3: Write the implementation**

In `internal/runner/projects.go` add the two fields at the end of `type Project struct`:

```go
	Slots   int       `json:"slots,omitempty"`
	Paused  bool      `json:"paused,omitempty"`
```

After `func (p *Projects) Remove`, add:

```go
const maxSlots = 8

// SetDispatch sets how many tickets the dispatcher keeps running in the project and whether it
// is paused; nil keeps a setting.
func (p *Projects) SetDispatch(id string, slots *int, paused *bool) (ProjectView, error) {
	if slots != nil && (*slots < 0 || *slots > maxSlots) {
		return ProjectView{}, fmt.Errorf("runner: slots must be between 0 and %d", maxSlots)
	}
	p.mu.Lock()
	pr := p.byID[id]
	if pr == nil {
		p.mu.Unlock()
		return ProjectView{}, ErrNoProject
	}
	old := pr.rec
	if slots != nil {
		pr.rec.Slots = *slots
	}
	if paused != nil {
		pr.rec.Paused = *paused
	}
	if err := p.saveLocked(); err != nil {
		pr.rec = old
		p.mu.Unlock()
		return ProjectView{}, err
	}
	v := p.viewLocked(pr)
	p.mu.Unlock()
	p.notify(v)
	return v, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -w internal/runner/projects.go internal/runner/projects_test.go &&
go test ./internal/runner/ -run 'Project|Dispatch'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/runner/projects.go internal/runner/projects_test.go
git commit -m "feat(runner): slots and pause on a project"
```

---

### Task 2: The dispatcher fills free slots

**Files:**
- Create: `internal/runner/dispatcher.go`
- Modify: `internal/runner/ticketruns_test.go` (the fixture keeps its `*Projects`)
- Test: `internal/runner/dispatcher_test.go` (new, package `runner`)

**Interfaces:**
- Consumes: `Projects.SetDispatch`, `ProjectView.Slots`, `ProjectView.Paused` (Task 1);
  part 2's `TicketRuns.Start(ctx, project, id string) (RunView, error)`, `ErrRunsClosed`;
  `Tickets.List`, `TicketView.Runnable`, `TicketView.Progress`.
- Produces:
  ```go
  type DispatcherConfig struct {
  	Projects   *Projects
  	Tickets    *Tickets
  	TicketRuns *TicketRuns
  	Started    func(project string, v TicketView, run string) // required; v is the listed view
  }
  func NewDispatcher(cfg DispatcherConfig) *Dispatcher // starts its goroutine; first pass now
  func (d *Dispatcher) Wake()                          // never blocks; coalesces
  func (d *Dispatcher) Close()                         // cancels a start, waits; safe twice
  ```
  Unexported, used by Task 3: `newDispatcher(cfg) *Dispatcher` (no goroutine), `loop()`,
  `pass() bool` (false: the dispatcher stops), the fields `ctx`, `cancel`, `wake`, `done`, the
  constant `dispatchEvery = 30 * time.Second`. Test helpers produced in `dispatcher_test.go`
  and used by Task 3: `f.setDispatch(slots int, paused bool)`, `f.dispatcher() *Dispatcher`
  (no goroutine), `f.dispatcherConfig()`, `f.pass(d)`, `f.tell(s)`, `f.said() []string` (a
  locked copy of `f.told`), `f.count(prefix) int`, `f.ticket(id) TicketView`.

- [ ] **Step 1: Keep the projects in the fixture**

In `internal/runner/ticketruns_test.go`, in `type fixture struct`, add a field after
`tickets *Tickets`:

```go
	projects *Projects
```

and in `newFixture`, right after `t.Cleanup(projects.Close)`:

```go
	f.projects = projects
```

- [ ] **Step 2: Write the failing tests**

Create `internal/runner/dispatcher_test.go`. Every test but one builds the dispatcher without
its goroutine and runs its passes itself; `TestAFinishedTicketFreesItsSlotForTheNext` runs the
goroutine for `Wake` and `Close`.

```go
package runner

import (
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

// dispatcher builds a dispatcher without its goroutine: the test runs its passes.
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
```

`f.tickets.notify` is set before the first ticket exists and before `go d.loop()`, so no
wake is lost. After `d.Close()` returns the goroutine is gone, so the check for `late`
needs no wait.

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/runner/ -run 'Dispatcher|Pause|AFinishedTicketFrees|WakeUps'`
Expected: FAIL to compile: `undefined: Dispatcher`, `undefined: newDispatcher`.

- [ ] **Step 4: Write the implementation**

Create `internal/runner/dispatcher.go`:

```go
package runner

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const dispatchEvery = 30 * time.Second

type DispatcherConfig struct {
	Projects   *Projects
	Tickets    *Tickets
	TicketRuns *TicketRuns
	// Started is told about every ticket the dispatcher started, as listed before the start,
	// with its new run.
	Started func(project string, v TicketView, run string)
}

// Dispatcher starts the oldest runnable tickets of every project with free slots. Its passes
// run one at a time on its own goroutine, so Wake is safe from inside the registries' notify.
type Dispatcher struct {
	projects *Projects
	tickets  *Tickets
	runs     *TicketRuns
	started  func(string, TicketView, string)

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	done   chan struct{}
}

func NewDispatcher(cfg DispatcherConfig) *Dispatcher {
	d := newDispatcher(cfg)
	go d.loop()
	return d
}

func newDispatcher(cfg DispatcherConfig) *Dispatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{
		projects: cfg.Projects, tickets: cfg.Tickets, runs: cfg.TicketRuns, started: cfg.Started,
		ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}),
	}
}

// Wake asks for a pass. Wake-ups that arrive during a pass cause one more.
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Close cancels the start in flight and waits for the dispatcher to stop.
func (d *Dispatcher) Close() {
	d.cancel()
	<-d.done
}

func (d *Dispatcher) loop() {
	defer close(d.done)
	tick := time.NewTicker(dispatchEvery)
	defer tick.Stop()
	for d.pass() {
		select {
		case <-d.ctx.Done():
			return
		case <-d.wake:
		case <-tick.C:
		}
	}
}

// pass fills the free slots of every project and reports whether the dispatcher goes on.
func (d *Dispatcher) pass() bool {
	for _, p := range d.projects.List() {
		if p.Slots == 0 || p.Paused {
			continue
		}
		views, err := d.tickets.List(p.ID)
		if err != nil {
			slog.Warn("the dispatcher could not read a project's tickets", "project", p.ID, "err", err)
			continue
		}
		running := 0
		for _, v := range views {
			if v.Status == TicketRunning && v.Progress == nil {
				running++
			}
		}
		for _, v := range views {
			if running >= p.Slots {
				break
			}
			if !v.Runnable {
				continue
			}
			run, err := d.runs.Start(d.ctx, p.ID, v.ID)
			switch {
			case err == nil:
				running++
				d.started(p.ID, v, run.ID)
			case errors.Is(err, ErrRunsClosed), d.ctx.Err() != nil:
				return false
			}
		}
	}
	return true
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `gofmt -w internal/runner/dispatcher.go internal/runner/dispatcher_test.go
internal/runner/ticketruns_test.go && go test -race ./internal/runner/`
Expected: PASS (only the known macOS failures and flakes listed in Global Constraints).

- [ ] **Step 6: Commit**

```bash
git add internal/runner/dispatcher.go internal/runner/dispatcher_test.go \
  internal/runner/ticketruns_test.go
git commit -m "feat(runner): the dispatcher fills free slots"
```

---

### Task 3: A failed start blocks the ticket once, or holds the dispatcher

**Files:**
- Modify: `internal/runner/tickets.go` (`Block`)
- Modify: `internal/runner/ticketruns.go` (`startingRefusal`, the "already starting" case)
- Modify: `internal/runner/dispatcher.go` (`held`, `loop`, `pass`)
- Test: `internal/runner/tickets_test.go`, `internal/runner/dispatcher_test.go` (append)

**Interfaces:**
- Consumes: `Dispatcher`, `newDispatcher`, `loop`, `pass`, the test helpers (Task 2);
  `TicketRuns.finished` (part 2's `Finished` callback, never nil); `ErrTooManyRuns` (wrapped
  with `%w` by `Runs.Create`).
- Produces:
  ```go
  func (t *Tickets) Block(project, id, reason string) (TicketView, error)
  type startingRefusal struct{ *TicketRefusal } // Unwrap() error returns the *TicketRefusal
  // Dispatcher gains the field held bool: set by pass, cleared by the tick in loop.
  ```
  `Block`: only a `ready`, runnable ticket; status `blocked`, the reason as a comment by
  `aigem` (cut like a run comment); refusals `<id> is <status>, not ready`,
  `<id> is not runnable`; `ErrNoTicket`. The reason the dispatcher writes is
  `the dispatcher could not start it: <error>`; it then calls `d.runs.finished(project, v,
  reason)`, which `cmd/aigem` wires to `ticketFinished` (`ticket.blocked`). The fixture's
  `Finished` records it as `blocked: <reason>`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/runner/tickets_test.go`:

```go
func TestBlockMovesOnlyARunnableReadyTicket(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ts, told := newTestTickets(t, t.TempDir())
	first := mustCreate(t, ts, NewTicket{Title: "first"})
	waits := mustCreate(t, ts, NewTicket{Title: "waits", DependsOn: []string{first.ID}})
	_, err := ts.Block("PRJ-1", first.ID, "x")
	refusal(t, err, "TCK-1 is open, not ready")
	for _, id := range []string{first.ID, waits.ID} {
		if _, err := ts.Update("PRJ-1", id, TicketPatch{Status: ptr(TicketReady)}); err != nil {
			t.Fatal(err)
		}
	}
	_, err = ts.Block("PRJ-1", waits.ID, "x")
	refusal(t, err, "TCK-2 is not runnable")

	n := len(*told)
	v, err := ts.Block("PRJ-1", first.ID, "the dispatcher could not start it: boom")
	if err != nil || v.Status != TicketBlocked || len(v.Comments) != 1 ||
		v.Comments[0].By != "aigem" || v.Comments[0].Text != "the dispatcher could not start it: boom" {
		t.Fatalf("Block = %+v, %v", v, err)
	}
	if len(*told) != n+1 {
		t.Errorf("announced %v, want the blocked ticket once more", *told)
	}
	_, err = ts.Block("PRJ-1", first.ID, "again")
	refusal(t, err, "TCK-1 is blocked, not ready")
	if _, err := ts.Block("PRJ-1", "TCK-9", "x"); !errors.Is(err, ErrNoTicket) {
		t.Errorf("unknown ticket = %v, want ErrNoTicket", err)
	}
}
```

Append to `internal/runner/dispatcher_test.go` (add `"context"` and `"os"` to its imports):

```go
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
```

In `TestAFailureNotOfTheTicketHoldsUntilTheTick` a file at `<repo>/.aigem` makes `prepare`'s
`os.MkdirAll` fail with a plain error, not a `*TicketRefusal`. In
`TestAStoppedDispatcherNeverBlocksATicket` the cancelled context makes git fail inside
`Start`, which refuses with `<repo> has no main or master branch`, a `*TicketRefusal`: the
dispatcher must see that it is stopped and not block.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/runner/ -run 'Block|Broken|Full|NotOfTheTicket|IsStarting|Stopped'`
Expected: FAIL to compile: `ts.Block undefined`, `d.held undefined`.

- [ ] **Step 3: `Tickets.Block`**

In `internal/runner/tickets.go`, after `Finish`, add:

```go
// Block moves a ready ticket that could not start to blocked, with the reason as a comment.
func (t *Tickets) Block(project, id, reason string) (TicketView, error) {
	reason = clipRunComment(reason)
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		tk := &tab.Tickets[i]
		switch {
		case tk.Status != TicketReady:
			return nil, refuse("%s is %s, not ready", id, tk.Status)
		case !ticketView(tab.Tickets, *tk).Runnable:
			return nil, refuse("%s is not runnable", id)
		}
		now := t.now()
		tk.Status, tk.Updated = TicketBlocked, now
		tk.Comments = append(tk.Comments, Comment{At: now, By: "aigem", Text: reason})
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}
```

- [ ] **Step 4: Tell "already starting" apart**

In `internal/runner/ticketruns.go`, after `var ErrNoWorktree = ...`, add:

```go
// startingRefusal refuses a ticket another Start holds, so the dispatcher can leave a person's
// Run in flight alone instead of blocking the ticket.
type startingRefusal struct{ *TicketRefusal }

func (e startingRefusal) Unwrap() error { return e.TicketRefusal }
```

In `Start`, replace the `case t.starting[key]:` branch:

```go
	case t.starting[key]:
		t.mu.Unlock()
		return RunView{}, startingRefusal{&TicketRefusal{Reason: id + " is already starting"}}
```

- [ ] **Step 5: The dispatcher blocks or holds**

In `internal/runner/dispatcher.go`, add the field after `done` in `type Dispatcher struct`:

```go
	// held ignores wake-ups until the next tick; only the dispatcher's goroutine touches it.
	held bool
```

In `loop`, the tick clears it:

```go
		case <-tick.C:
			d.held = false
```

In `pass`, return at once while held (first line of the function):

```go
	if d.held {
		return true
	}
```

and replace the `switch` after `d.runs.Start` with:

```go
			switch {
			case err == nil:
				running++
				d.started(p.ID, v, run.ID)
			case errors.Is(err, ErrRunsClosed), d.ctx.Err() != nil:
				return false
			case errors.As(err, new(startingRefusal)):
			case errors.As(err, new(*TicketRefusal)):
				reason := "the dispatcher could not start it: " + err.Error()
				if b, berr := d.tickets.Block(p.ID, v.ID, reason); berr == nil {
					d.runs.finished(p.ID, b, reason)
				}
			default:
				slog.Warn("the dispatcher waits for its next tick", "project", p.ID, "ticket", v.ID,
					"err", err)
				d.held = true
				return true
			}
```

The order matters: the stopped check comes before the refusal (a cancelled start refuses),
and `startingRefusal` before `*TicketRefusal` (it unwraps to one). `Block` refuses a ticket
that is no longer ready or runnable (it changed between the list and the start), so that case
needs no code here.

- [ ] **Step 6: Run the tests and the linter**

Run: `gofmt -w internal/runner/*.go && go test -race ./internal/runner/ &&
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./internal/runner/...`
Expected: PASS (only the known failures and flakes); `TestRunRefusesATicketThatCannotStart`
and the part 2 "already starting" refusals still pass, since `refusal` uses `errors.As`.

- [ ] **Step 7: Commit**

```bash
git add internal/runner/tickets.go internal/runner/tickets_test.go \
  internal/runner/ticketruns.go internal/runner/dispatcher.go internal/runner/dispatcher_test.go
git commit -m "feat(runner): a failed start blocks the ticket once or holds the dispatcher"
```

---

### Task 4: PATCH /api/projects/{id}, the daemon wiring and ticket.started

**Files:**
- Modify: `internal/web/api_projects.go` (`Project` fields, `ProjectPatch`, seam method,
  handler)
- Modify: `internal/web/server.go` (route)
- Modify: `cmd/aigem/webprojects.go` (`UpdateProject`, `webProject`)
- Modify: `cmd/aigem/webtickets.go` (`ticketStarted`)
- Modify: `cmd/aigem/webruns.go` (`notifier` wakes)
- Modify: `cmd/aigem/webcmd.go` (build, wire and close the dispatcher)
- Test: `internal/web/api_projects_test.go`, `cmd/aigem/webprojects_test.go`,
  `cmd/aigem/webtickets_test.go` (append)

**Interfaces:**
- Consumes: `Projects.SetDispatch` (Task 1); `NewDispatcher`, `DispatcherConfig{Projects,
  Tickets, TicketRuns, Started}`, `Wake`, `Close` (Tasks 2, 3). A ticket the dispatcher blocks
  already reaches the existing `ticketFinished` through `TicketRunsConfig.Finished`.
- Produces:
  ```go
  // web.Project gains:
  Slots  int  `json:"slots"`
  Paused bool `json:"paused"`
  type ProjectPatch struct {
  	Slots  *int  `json:"slots,omitempty"`
  	Paused *bool `json:"paused,omitempty"`
  }
  // web.ProjectsBackend gains:
  UpdateProject(ctx context.Context, id string, req ProjectPatch) (Project, error)
  // cmd/aigem:
  func (b *webBackend) ticketStarted(project string, v runner.TicketView, run string)
  func (n *notifier) wakes(f func())
  ```
  Route `PATCH /api/projects/{id}`: 200 with the project; 400 for a `*Refusal` or a bad body;
  404 `ErrNoProject`; 405 for other methods (`Allow: PATCH, DELETE`).

- [ ] **Step 1: Write the failing tests**

Append to `internal/web/api_projects_test.go`:

```go
func (b *projectsBackend) UpdateProject(_ context.Context, id string, req ProjectPatch) (
	Project, error,
) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, p := range b.projects {
		if p.ID != id {
			continue
		}
		if req.Slots != nil {
			if *req.Slots < 0 || *req.Slots > 8 {
				return Project{}, Refuse(errors.New("slots must be between 0 and 8"))
			}
			b.projects[i].Slots = *req.Slots
		}
		if req.Paused != nil {
			b.projects[i].Paused = *req.Paused
		}
		return b.projects[i], nil
	}
	return Project{}, ErrNoProject
}

func TestUpdatingAProjectAnswersForEachState(t *testing.T) {
	srv, _ := newProjectsServer(t)
	api(t, srv, http.MethodPost, "/api/projects", `{"dir":"/home/dev/thing"}`)

	res := api(t, srv, http.MethodPatch, "/api/projects/PRJ-1", `{"slots":2}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", res.StatusCode, readBody(t, res))
	}
	if got := decode[Project](t, res); got.Slots != 2 || got.Paused {
		t.Errorf("after slots = %+v", got)
	}
	res = api(t, srv, http.MethodPatch, "/api/projects/PRJ-1", `{"paused":true}`)
	if got := decode[Project](t, res); got.Slots != 2 || !got.Paused {
		t.Errorf("after pause = %+v, want the slots kept", got)
	}
	list := decode[[]Project](t, api(t, srv, http.MethodGet, "/api/projects", ""))
	if last := list[len(list)-1]; last.Slots != 2 || !last.Paused {
		t.Errorf("listed = %+v, want slots and paused", last)
	}

	res = api(t, srv, http.MethodPatch, "/api/projects/PRJ-1", `{"slots":9}`)
	if res.StatusCode != http.StatusBadRequest ||
		!strings.Contains(readBody(t, res), "slots must be between 0 and 8") {
		t.Errorf("slots 9 = %d, want 400 with the sentence", res.StatusCode)
	}
	if res := api(t, srv, http.MethodPatch, "/api/projects/PRJ-1", `{"name":"x"}`); res.StatusCode !=
		http.StatusBadRequest {
		t.Errorf("an unknown field = %d, want 400", res.StatusCode)
	}
	if res := api(t, srv, http.MethodPatch, "/api/projects/PRJ-9", `{"slots":1}`); res.StatusCode !=
		http.StatusNotFound {
		t.Errorf("an unknown project = %d, want 404", res.StatusCode)
	}
}
```

The existing `TestTheProjectRoutesRefuseOtherMethodsAndNeedTheFeature` already checks
`GET /api/projects/PRJ-1` is 405; it must keep passing.

Append to `cmd/aigem/webprojects_test.go`:

```go
func TestUpdatingAProjectSetsItsDispatcher(t *testing.T) {
	ctx := context.Background()
	b := newWebBackend(webBackendConfig{projects: testProjects(t, nil)})
	added, err := b.AddProject(ctx, web.NewProject{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	slots, paused := 2, true
	got, err := b.UpdateProject(ctx, added.ID, web.ProjectPatch{Slots: &slots})
	if err != nil || got.Slots != 2 || got.Paused {
		t.Fatalf("slots = %+v, %v", got, err)
	}
	if got, err = b.UpdateProject(ctx, added.ID, web.ProjectPatch{Paused: &paused}); err != nil ||
		got.Slots != 2 || !got.Paused {
		t.Fatalf("pause = %+v, %v", got, err)
	}
	if list, _ := b.Projects(ctx); list[len(list)-1].Slots != 2 || !list[len(list)-1].Paused {
		t.Errorf("listed = %+v", list)
	}
	bad := 9
	_, err = b.UpdateProject(ctx, added.ID, web.ProjectPatch{Slots: &bad})
	var refusal *web.Refusal
	if !errors.As(err, &refusal) || err.Error() != "slots must be between 0 and 8" {
		t.Errorf("slots 9 = %v, want a refusal without the package prefix", err)
	}
	if _, err := b.UpdateProject(ctx, "PRJ-9", web.ProjectPatch{Slots: &slots}); !errors.Is(err,
		web.ErrNoProject) {
		t.Errorf("unknown project = %v, want web.ErrNoProject", err)
	}
}
```

Append to `cmd/aigem/webtickets_test.go`:

```go
func TestADispatchedTicketIsInTheActivityFeed(t *testing.T) {
	b, project, _ := ticketsBackend(t)
	v := runner.TicketView{Ticket: runner.Ticket{ID: "TCK-1", Title: "notes"}}
	b.ticketStarted(project, v, "RUN-2")
	feed, _ := b.Activity(context.Background(), 0, 0)
	if len(feed) != 1 || feed[0].Kind != "ticket.started" || feed[0].Text != "Started TCK-1: notes" ||
		feed[0].RunRef != "RUN-2" {
		t.Errorf("feed = %+v", feed)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/web/ ./cmd/aigem/ -run 'UpdatingAProject|Dispatched'`
Expected: FAIL to compile: `undefined: ProjectPatch`, `b.UpdateProject undefined`,
`b.ticketStarted undefined`.

- [ ] **Step 3: The route**

In `internal/web/api_projects.go`:

Add to `ProjectsBackend` after `RemoveProject`:

```go
	// UpdateProject sets the dispatcher's slots and pause. ErrNoProject for an unknown id, a
	// *Refusal for slots out of range.
	UpdateProject(ctx context.Context, id string, req ProjectPatch) (Project, error)
```

Add at the end of `type Project struct`:

```go
	// Slots is how many tickets the dispatcher keeps running; 0 is off.
	Slots  int  `json:"slots"`
	Paused bool `json:"paused"`
```

After `type NewProject struct`, add:

```go
type ProjectPatch struct {
	Slots  *int  `json:"slots,omitempty"`
	Paused *bool `json:"paused,omitempty"`
}
```

After `handleRemoveProject`, add:

```go
func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[ProjectsBackend](s, w, "projects")
	if !ok {
		return
	}
	var req ProjectPatch
	if err := decodeJSON(w, r, &req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	p, err := b.UpdateProject(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeRunError(w, "changing a project", err)
		return
	}
	writeJSON(w, p)
}
```

In `internal/web/server.go` replace the two `/api/projects/{id}` lines with:

```go
	s.api("PATCH /api/projects/{id}", s.handleUpdateProject)
	s.api("DELETE /api/projects/{id}", s.handleRemoveProject)
	s.mux.HandleFunc("/api/projects/{id}", methodNotAllowed("PATCH, DELETE"))
```

- [ ] **Step 4: The adapter and the activity**

In `cmd/aigem/webprojects.go`, after `RemoveProject`, add:

```go
func (b *webBackend) UpdateProject(_ context.Context, id string, req web.ProjectPatch) (
	web.Project, error,
) {
	if b.projects == nil {
		return web.Project{}, web.ErrUnavailable
	}
	v, err := b.projects.SetDispatch(id, req.Slots, req.Paused)
	if err != nil {
		return web.Project{}, webProjectError(err)
	}
	return webProject(v), nil
}
```

and make `webProject` carry the two fields:

```go
func webProject(v runner.ProjectView) web.Project {
	return web.Project{
		ID: v.ID, Name: v.Name, Dir: v.Dir, Created: v.Created, LoadError: v.LoadError,
		Slots: v.Slots, Paused: v.Paused,
	}
}
```

In `cmd/aigem/webtickets.go`, after `ticketFinished`, add:

```go
// ticketStarted records a ticket the dispatcher started in the activity feed.
func (b *webBackend) ticketStarted(_ string, v runner.TicketView, run string) {
	b.recordActivity(web.Activity{Kind: "ticket.started", Text: "Started " + v.ID + ": " + v.Title,
		RunRef: run})
}
```

- [ ] **Step 5: The notifier wakes the dispatcher**

In `cmd/aigem/webruns.go` give `notifier` a field and two methods, and call `dispatch` from
the ticket and project callbacks:

```go
type notifier struct {
	mu   sync.Mutex
	srv  *web.Server
	wake func()
}

// wakes sets what a ticket or project change wakes: the dispatcher, once it exists.
func (n *notifier) wakes(f func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.wake = f
}

func (n *notifier) dispatch() {
	n.mu.Lock()
	wake := n.wake
	n.mu.Unlock()
	if wake != nil {
		wake()
	}
}
```

```go
func (n *notifier) publishProject(v runner.ProjectView) {
	n.publish("project.updated", webProject(v))
	n.dispatch()
}
```

```go
func (n *notifier) publishTicket(project string, v runner.TicketView) {
	n.publish("ticket.updated", map[string]string{"projectId": project, "id": v.ID})
	n.dispatch()
}
```

Keep the existing doc comments above `publishProject` and `publishTicket`.

- [ ] **Step 6: Wire the dispatcher**

In `cmd/aigem/webcmd.go`, replace the `if tickets != nil && projects != nil { ... }` block that
builds `backend.ticketRuns` with:

```go
	var dispatcher *runner.Dispatcher
	if tickets != nil && projects != nil {
		backend.ticketRuns = runner.NewTicketRuns(runner.TicketRunsConfig{
			Runs: runs, Tickets: tickets, Projects: projects,
			Finished: backend.ticketFinished, Planned: backend.ticketPlanned,
		})
		backend.ticketRuns.Recover()
		// Deferred after runs.Close, so it runs first: deliveries stop before the sessions do.
		defer backend.ticketRuns.Close()
		dispatcher = runner.NewDispatcher(runner.DispatcherConfig{
			Projects: projects, Tickets: tickets, TicketRuns: backend.ticketRuns,
			Started: backend.ticketStarted,
		})
		announce.wakes(dispatcher.Wake)
		// Deferred after ticketRuns.Close, so it runs before it: no start races their shutdown.
		defer dispatcher.Close()
	}
```

In the signal branch, before `if backend.ticketRuns != nil {`, add:

```go
		if dispatcher != nil {
			dispatcher.Close()
		}
```

- [ ] **Step 7: Run the tests and the linter**

Run: `gofmt -w internal/web/*.go cmd/aigem/*.go && go build ./... &&
go test ./internal/web/ ./cmd/aigem/ &&
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./...`
Expected: PASS; lint shows only the old QF1003.

- [ ] **Step 8: Commit**

```bash
git add internal/web/api_projects.go internal/web/api_projects_test.go internal/web/server.go \
  cmd/aigem/webprojects.go cmd/aigem/webprojects_test.go cmd/aigem/webtickets.go \
  cmd/aigem/webtickets_test.go cmd/aigem/webruns.go cmd/aigem/webcmd.go
git commit -m "feat(web): project slots and pause route; the daemon runs the dispatcher"
```

---

### Task 5: UI - Slots, Pause / Resume and the running count

**Files:**
- Modify: `internal/web/_ui/src/lib/wire.ts` (`Project`, `ProjectPatch`)
- Modify: `internal/web/_ui/src/lib/api.ts` (`updateProject`)
- Modify: `internal/web/_ui/src/screens/Tickets.tsx` (header controls)
- Modify: `internal/web/_ui/src/screens/Task.tsx` (export `BUTTON`)
- Test: `internal/web/_ui/src/screens/tickets.test.tsx` (append)

**Interfaces:**
- Consumes: `PATCH /api/projects/{id}` `{slots?, paused?}` and `slots`, `paused` on
  `GET /api/projects` (Task 4).
- Produces:
  ```ts
  export type Project = { /* ...existing... */ slots?: number; paused?: boolean }
  export type ProjectPatch = { slots?: number; paused?: boolean }
  api.updateProject(id: string, patch: ProjectPatch, signal?: AbortSignal): Promise<Project>
  ```
  On the Tickets screen with a project selected: a "Slots" select (Off, 1-8), a Pause / Resume
  button, "n of N running" while slots > 0 (n = leaf tickets `running`), a "Paused" badge while
  paused. A failed change goes to the page banner. The daemon's own directory shows none.

- [ ] **Step 1: Write the failing tests**

Append to `internal/web/_ui/src/screens/tickets.test.tsx`:

```ts
test('the dispatcher controls set slots, pause and resume, and count running tickets', async () => {
  let prj = { ...PRJ, slots: 2, paused: false }
  const h = await openTickets(PLAN, {
    '/api/projects': () => json([DAEMON_PROJECT, prj]),
    'PATCH /api/projects/PRJ-1': () => {
      prj = { ...prj, ...(JSON.parse(h.sent[h.sent.length - 1]?.body ?? '{}') as object) }
      return json(prj)
    },
  })
  expect(await screen.findByText('1 of 2 running')).toBeInTheDocument()
  expect(screen.getByLabelText('Slots')).toHaveValue('2')
  expect(screen.queryByText('Paused')).not.toBeInTheDocument()

  await userEvent.selectOptions(screen.getByLabelText('Slots'), '3')
  expect(await screen.findByText('1 of 3 running')).toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'Pause' }))
  expect(await screen.findByText('Paused')).toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'Resume' }))
  await waitFor(() => expect(screen.queryByText('Paused')).not.toBeInTheDocument())

  const patches = h.sent
    .filter((s) => s.method === 'PATCH')
    .map((s) => JSON.parse(s.body) as unknown)
  expect(patches).toEqual([{ slots: 3 }, { paused: true }, { paused: false }])
})

test('a refused slot count shows the sentence; the daemon directory has no controls', async () => {
  await openTickets(PLAN, {
    'PATCH /api/projects/PRJ-1': () =>
      new Response('slots must be between 0 and 8', { status: 400 }),
  })
  await userEvent.selectOptions(screen.getByLabelText('Slots'), '5')
  expect(await screen.findByRole('alert')).toHaveTextContent('slots must be between 0 and 8')
  expect(screen.queryByText(/ of \d+ running$/)).not.toBeInTheDocument()
  act(() => selectProject(''))
  expect(await screen.findByText('Tickets need a project.')).toBeInTheDocument()
  expect(screen.queryByLabelText('Slots')).not.toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'Pause' })).not.toBeInTheDocument()
})
```

`PLAN` has one running leaf (TCK-4) and one running parent (TCK-1, with `progress`), so the
count is 1. In the second test `PRJ` has no `slots`, so the select shows Off and no count.

The Slots select adds `option` roles to the page, so in the existing test
`the palette opens a ticket found by its title` scope the options to the palette. Replace:

```ts
  await screen.findByRole('dialog', { name: 'Command palette' })
  await userEvent.keyboard('Runner change')
  expect(screen.getAllByRole('option')[0]).toHaveTextContent('TCK-4 Runner change')
```

with:

```ts
  const palette = await screen.findByRole('dialog', { name: 'Command palette' })
  await userEvent.keyboard('Runner change')
  expect(within(palette).getAllByRole('option')[0]).toHaveTextContent('TCK-4 Runner change')
```

- [ ] **Step 2: Run the tests to verify they fail**

Run in `internal/web/_ui`:

```bash
NODE_OPTIONS=--no-experimental-webstorage npx vitest run src/screens/tickets.test.tsx
```

Expected: FAIL: no text "1 of 2 running", no label "Slots".

- [ ] **Step 3: Wire types and the call**

In `internal/web/_ui/src/lib/wire.ts` replace the `Project` line and add `ProjectPatch` after
it:

```ts
export type Project = {
  id: string
  name: string
  dir: string
  created?: string
  loadError?: string
  slots?: number
  paused?: boolean
}

export type ProjectPatch = { slots?: number; paused?: boolean }
```

In `internal/web/_ui/src/lib/api.ts` add `ProjectPatch,` to the type import (after `Project,`)
and, after `addProject`, add:

```ts
  updateProject: (id: string, patch: ProjectPatch, signal?: AbortSignal) =>
    json<Project>(`/api/projects/${encodeURIComponent(id)}`, {
      ...body(patch),
      method: 'PATCH',
      signal,
    }),
```

- [ ] **Step 4: The header controls**

In `internal/web/_ui/src/screens/Tickets.tsx`:

Change the imports:

```tsx
import type { ProjectPatch, Repository, Ticket } from '@/lib/wire'
import {
  currentProject,
  explain,
  flash,
  patchNewTicket,
  refresh,
  setBanner,
  useApp,
} from '@/state/app'
```

In `internal/web/_ui/src/screens/Task.tsx` change `const BUTTON =` to `export const BUTTON =`
(the value stays). In `Tickets.tsx` add after the `@/ui/SegmentedControl` import:

```tsx
import { BUTTON } from './Task'
```

`Task.tsx` already imports from `./Tickets`; the cycle is safe because both constants are read
only while rendering, never while the modules load.

Replace the `useApp` call at the top of `Tickets` and add the count and the change:

```tsx
  const { project, tickets, name, newOpen, slots, paused } = useApp((s) => ({
    project: s.project,
    tickets: s.tickets,
    name: currentProject(s)?.name ?? '',
    newOpen: s.newTicketOpen,
    slots: currentProject(s)?.slots ?? 0,
    paused: currentProject(s)?.paused ?? false,
  }))
  const running = tickets.filter((t) => t.status === 'running' && !t.progress).length
```

```tsx
  const dispatch = async (change: ProjectPatch) => {
    try {
      await api.updateProject(project, change)
      await refresh.projects()
    } catch (err) {
      setBanner(explain(err))
    }
  }
```

Replace the "New ticket" button in the header (inside `{project && ( <> ... </> )}`) with:

```tsx
            <div className="ml-auto flex items-center gap-2 text-[0.78125rem] text-fg-subtle">
              {paused && (
                <span
                  className="rounded-[0.1875rem] border border-line-strong px-1 text-[0.6875rem]"
                >
                  Paused
                </span>
              )}
              {slots > 0 && <span>{`${running} of ${slots} running`}</span>}
              <label className="flex items-center gap-1">
                Slots
                <select
                  value={slots}
                  onChange={(e) => void dispatch({ slots: Number(e.target.value) })}
                  className={INPUT}
                >
                  <option value={0}>Off</option>
                  {Array.from({ length: 8 }, (_, i) => i + 1).map((n) => (
                    <option key={n} value={n}>
                      {n}
                    </option>
                  ))}
                </select>
              </label>
              <button
                type="button"
                onClick={() => void dispatch({ paused: !paused })}
                className={BUTTON}
              >
                {paused ? 'Resume' : 'Pause'}
              </button>
            </div>
            <button type="button" onClick={() => patchNewTicket(true)} className={PRIMARY}>
              New ticket
            </button>
```

`INPUT` is the constant already exported further down this file.

- [ ] **Step 5: Run the tests and checks**

Run in `internal/web/_ui`:

```bash
npm run lint && npm run check && NODE_OPTIONS=--no-experimental-webstorage npx vitest run
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/web/_ui/src/lib/wire.ts internal/web/_ui/src/lib/api.ts \
  internal/web/_ui/src/screens/Tickets.tsx internal/web/_ui/src/screens/Task.tsx \
  internal/web/_ui/src/screens/tickets.test.tsx
git commit -m "feat(web): slots, pause and the running count on the Tickets screen"
```

---

### Task 6: Docs, full check, manual check

**Files:**
- Modify: `docs/web.md` (route row, `GET /api/projects` note, a "Dispatcher" section)
- Modify: `CHANGELOG.md` (one entry under Unreleased/Added)

- [ ] **Step 1: Document**

In `docs/web.md`, in the routes table after the `DELETE /api/projects/{id}` row add:

```markdown
| `PATCH /api/projects/{id}` | `{slots?, paused?}`; 200 with the project, 400 out of range, 404 |
```

and in the `GET /api/projects` row, after "first with an empty id", add
"; each with `slots` and `paused`" (same cell).

After the "Planner" section (before `## Runs`) add:

```markdown
## Dispatcher

The dispatcher presses "Run" by itself. Each project has `slots` (0 to 8, default 0: off) and
`paused`. While fewer of its tickets are `running` than there are slots, the daemon starts the
oldest runnable ticket. A slot is a `running` ticket, whoever started it; blocked tickets and
planner runs take none. A person's "Run" during a pass can briefly put one ticket over the
slots. The daemon's limit of 32 live runs still applies. The dispatcher never plans.

It looks again after every ticket or project change and every 30 seconds. A start the ticket
itself cannot make (a path in the way of the worktree, not a git checkout) moves it to
`blocked` with the comment "the dispatcher could not start it: <error>". Any other error (the
run limit, a project env that does not load, the model) blocks nothing: the dispatcher logs it
and waits for the next 30 second tick. A ticket moved back to `ready` while its run is still
live is blocked again with the live-run reason. Pause stops new starts; lowering the slots
stops nothing.
```

In `CHANGELOG.md` under `## [Unreleased]` / `### Added`, above the part 3 entry, add:

```markdown
- Web UI: the dispatcher (part 4 of agent tickets). Each project has 0 to 8 slots; while
  fewer tickets are running, the daemon starts the oldest runnable one by itself. Pause and
  Resume on the Tickets screen; a ticket that cannot start is blocked with the reason.
```

Max 100 characters per prose line; table rows may be longer, as the existing rows are.

- [ ] **Step 2: Full check**

```bash
go build ./... && go test ./... 2>&1 | grep -v '^ok' | head -20
go test -race ./internal/runner/... 2>&1 | grep -v '^ok' | head -20
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./...
cd internal/web/_ui && npm run lint && npm run check && \
  NODE_OPTIONS=--no-experimental-webstorage npx vitest run && cd -
make web && make build
```

Expected: only the known macOS failures and flakes, and the old QF1003.

- [ ] **Step 3: Manual check with a real model**

Start `bin/aigem web --addr 127.0.0.1:7799` (stop an older one on that port first). Add a
throwaway git repository with a `main` branch as a project, with `.aigem/project.json`
`{"check": "true"}` committed. Create three small tickets and move them to `ready`. Set
Slots to 2: two tickets go `running` by themselves, the header says "2 of 2 running", and the
activity feed shows two `ticket.started`. Press Pause: the "Paused" badge shows, the running
two go on, and when one finishes the third does not start. Press Resume: the third starts.
Put a file at `<project>/.aigem/worktrees/TCK-n` for a new ready ticket: it is blocked once
with "the dispatcher could not start it: ..." and a `ticket.blocked` line.

- [ ] **Step 4: Commit**

```bash
git add docs/web.md CHANGELOG.md
git commit -m "docs: the dispatcher"
```
