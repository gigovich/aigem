# Agent Tickets Part 3 Implementation Plan: Planner

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** "Plan" on an open top-level ticket opens a read-only planner run that splits it into
draft subtickets; the ticket waits in `review` until a person approves, rejects or revises the
plan.

**Architecture:** `runner.Tickets` learns the plan moves (`StartPlan`, `PlanReview`, `Approve`,
`Reject`, `Revise`), stops deriving a parent while it is `planning` or `review`, and guards the
draft; the planner's writes go through unexported `create`, `update` and `remove` that take the
run id. `runner.Runs` opens an autonomous run with a capability profile and a root that is not a
worktree. Part 2's coordinator `runner.TicketRuns` gains `Plan`, `Approve`, `Reject`, `Revise`
and the planner's ticket tools; its turn end, `Stop`, `Remove` and `Recover` send a planner's
ticket to `review`. `cmd/aigem` adapts it to four new routes; the React ticket page gets the
buttons, the reason and feedback boxes and the draft badge, and the Tickets list a "Needs you"
filter.

**Tech Stack:** Go 1.26 (`net/http` ServeMux patterns), git >= 2.28 for the test repositories,
React 19 + TypeScript, Tailwind v4, vitest + Testing Library.

**Spec:** `docs/superpowers/specs/2026-10-10-agent-tickets-part3-planner-design.md` (part 3;
context: `docs/superpowers/specs/2026-10-09-agent-tickets-design.md` (part 1) and
`docs/superpowers/specs/2026-10-09-agent-tickets-part2-runs-design.md` (part 2))

## Global Constraints

- Work directly on `main` in `/Users/gigovich/work/gigovich/aigem`; commit after every task.
  Nothing pushes - not the daemon, not this plan.
- Who can be planned: a top-level ticket with status `open` and no subtickets. Refusals (409),
  exactly: `<id> is a subticket; only a top-level ticket is planned`, `<id> is <status>, not
  open`, `<id> already has subtickets`, `<id> has a live run <run>; stop it first`.
- Draft guards, exactly: `<id> is part of a plan in review` (a status move of a draft),
  `<id> is a draft of <parent>'s plan` (a dependency from outside the plan),
  `<parent> is being planned` (a person adds, deletes or relinks a subticket in `planning`).
- Approve refusal: `<id>'s plan has no subtickets; revise or reject it`. Reject comment:
  `Plan rejected: <reason>`. Revise feedback is a comment by `you`.
- Review reasons, exactly: `stopped by a person`, `the run was deleted`,
  `the daemon restarted`, `interrupted`, `the turn ended with an error: ...`.
- Planner first-message rule, verbatim: "Split this ticket into subtickets that each fit one
  autonomous run of a coding agent. Link them with dependencies. Read the code as much as you
  need; you cannot change it. When the plan is complete, call plan_done with a short summary."
- The planner run: `ModeAutonomous`, capability profile `read-only`, rooted at
  `<project dir>/<repo>` (the project directory when `repo` is empty), `TicketID` set, no
  worktree. Draft subtickets are `open` with `by: run RUN-n`.
- Routes on `/api/projects/{id}/tickets/{tid}/`: `POST plan` (201 with the run),
  `POST approve` (200 with the ticket), `POST reject` `{reason}` (200), `POST revise` `{text}`
  (200). Activity kinds: `ticket.planned`, `ticket.approved`, `ticket.rejected`.
- Minimal code, few and concise comments, follow the idioms already in the file you touch.
- Go lines <= 120 characters; Markdown lines <= 100 characters; no em dashes anywhere, use `-`.
- UI sizes in rem / Tailwind spacing only (no new px); no `cursor-*` classes (global rule);
  icon-only buttons need `aria-label` and `title`; every control is keyboard-operable. Do not
  run prettier.
- Go tests that touch state set `XDG_STATE_HOME` to `t.TempDir()`. Git tests use real
  repositories in `t.TempDir()` with `user.name`, `user.email` and `commit.gpgsign=false` set
  locally (the part 2 `gitRepo` helper does this).
- Commands: `gofmt -w <touched files>`; `go test -race ./internal/... ./cmd/aigem/...`;
  lint `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./...`
  (the old QF1003 in `internal/tui/model_add.go` is not ours); UI in `internal/web/_ui`:
  `npm run lint`, `npm run check`, `NODE_OPTIONS=--no-experimental-webstorage npx vitest run`.
- Known failures on macOS, also on `main`, not ours:
  `TestLoadRefusesAnUnresolvableWorkingDirectory`,
  `TestLoadErrorNamesTheDirectoryAndKeepsTheCause` (runner), `TestSetupSandboxIsPrivate`
  (testenv). Flaky under `-race`: `TestSpecEvictionSettingsReachTheAgent`,
  `TestSpecAutoCompactionReachesTheAgent`, `TestCompactionInheritsTheSessionsContextWindow`
  (runner), `TestSkillsBrowserAndDispatch` (tui).

### Decisions and deviations

- Decision: the planner's writes use unexported `Tickets.create`, `update` and `remove`, which
  take the run id; the exported `Create`, `Update` and `Delete` pass `""` (a person). One rule,
  `planLock`, decides both sides, so a person cannot pose as a planner over HTTP.
- Deviation from spec: refusals the spec does not list, for the planner's tools:
  `<run> does not plan <parent>`, `<parent> is <status>, not planning`,
  `<run> only changes the subtickets of the ticket it plans`; and for people:
  `<id> is <status>, not in review`, `a rejection needs a reason`, `feedback cannot be empty`,
  `<dir> does not exist` (the ticket's repository directory is missing); and for a planner run
  ending its plan: `<id> is <status>; no plan is in progress` (`PlanReview`).
- Decision: a person typing into a live planner run while the ticket is in `review` moves it
  back to `planning` (as part 2 takes `blocked` back to `running`); that turn's end sends it to
  `review` again.
- Decision: `PlanReview` also accepts a ticket already in `review`: stopping or deleting a live
  planner run in `review` adds the reason as a comment and leaves the ticket in `review`.
- Decision: no "starting" set for planners. `StartPlan` accepts only `open` and `Tickets.Revise`
  only `review`, inside the registry lock, so a second click's run is refused and deleted.
- Decision: `Tickets.Revise` takes the run that will get the feedback and appends it to `Runs`
  when it is new, so a revise with a dead run is one registry change.
- Decision: `RunRequest` gains `Dir` and `Profile`. `Create` refuses (`ErrRunMode`) an unknown
  profile, a profile on an interactive run, and an autonomous run without a worktree unless its
  profile is `read-only` and `Dir` is set. `Mode.CapabilitySubset` takes the profile name.
- Decision: the planner prompt always lists `""` (the project directory) first, then each named
  checkout; a new planner run after Revise lists the draft under "## Your draft", and the
  feedback is the last comment of the discussion it carries.
- Decision: the message sent to a live planner on Revise is "A person reviewed the plan and
  asks for changes:", the feedback, then "Revise the subtickets, then call plan_done again."
- Deviation from spec: `list_tickets` and `get_ticket` read any ticket of the project (the spec
  says the tools act only on that ticket's subtickets, but also "read the project's tickets");
  the writes touch only the planned ticket's subtickets.
- Decision: the planner's subagents and skills build their tools from the run's registry, so
  they reach the ticket tools too. Accepted: the same `Tickets` guards hold for them.
- Decision: a `delete_subticket` of a draft another draft waits for keeps part 1's refusal
  (`<x> waits for <id>; close it instead`); the planner unlinks first with `set_dependencies`.
- Deviation from spec: deleting a ticket in `review` is refused too (part 1 refuses only
  `running`, `planning` or a ticket with subtickets), so no live planner run is left without
  its ticket: reject the plan first, then delete.
- Decision: the planner run's title is `Plan <TCK-n>: <title>`.
- Decision: activity texts: `Plan of <id> in review: <first line>`,
  `Approved the plan of <id>: <title>`, `Rejected the plan of <id>: <first line of reason>`.
- Decision: on the ticket page "Stop" moves out of the "no subtickets" block, so a parent whose
  planner is live offers it; Approve/Reject/Revise show in `review` with or without subtickets.
- Decision: the list filter value `blocked` becomes `needs` with the label "Needs you".

## Review Focus

- Revise after the planner run is gone (stopped, deleted, daemon restarted): a new planner run
  opens, `Runs` has both, and its first message carries the draft and the feedback; nothing is
  lost. (Task 6 `TestReviseOpensANewPlannerWhenTheOldOneIsGone`.)
- A person edits the draft: in `review` they add, delete and relink subtickets with the
  existing UI; in `planning` they are refused; no outside ticket can wait for a draft, and no
  draft can be moved to `ready`/`closed` before approval. (Task 1
  `TestADraftIsHeldWhileItsParentIsPlannedOrReviewed`.)
- Two quick clicks on "Plan": exactly one planner starts, the other is refused with a sentence
  and leaves no run behind (the daemon's run limit is shared). (Task 5
  `TestTwoPlanClicksOpenOnePlanner`.)
- A late turn end after Approve or Reject writes no comment and moves nothing, and the planner
  run no longer holds a live slot. (Task 6 `TestApproveMakesTheDraftReadyAndStopsThePlanner`,
  `TestRejectDeletesTheDraftAndOpensTheTicket`.)
- The planner tries to change code or other tickets: no file is written, and its ticket tools
  refuse anything but its own ticket's subtickets, a stale run, or a ticket not `planning`.
  (Task 3 `TestAReadOnlyRunIsRootedAtItsDirAndCannotWrite`; Task 4
  `TestThePlannerToolsActOnlyWhileTheirRunPlans`; Task 5
  `TestAPlannerRunSplitsTheTicketAndHandsItToAPerson`.)

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/runner/ticket_rules.go` | `isDraft`, `planLock`, `plannable`; draft case in `checkDeps` |
| `internal/runner/tickets.go` | derive skip; `create`/`update`/`remove`; the five plan moves |
| `internal/runner/session.go` | `Spec.Profile`; `Mode.CapabilitySubset(profile)` |
| `internal/runner/runs.go` | `RunRequest.Dir`, `Profile`, `allowed`; `readOnlyProfile` |
| `cmd/aigem/webruns.go` | `openRun` passes the profile |
| `internal/runner/plantools.go` (new) | the planner's ticket tools |
| `internal/runner/ticketplan.go` (new) | `Plan`, `Approve`, `Reject`, `Revise`, prompt, review |
| `internal/runner/ticketruns.go` | `ticketRun.plan`, `Planned`, turn end, detach, recover |
| `internal/web/api_tickets.go`, `server.go` | seam methods, handlers, routes |
| `cmd/aigem/webtickets.go`, `webcmd.go` | adapter, activity, wiring |
| `internal/web/_ui/src/lib/api.ts` | four calls |
| `internal/web/_ui/src/screens/Task.tsx` | buttons, reason/feedback box, draft badge |
| `internal/web/_ui/src/state/tickets.ts`, `screens/Tickets.tsx` | "Needs you" filter |
| `docs/web.md`, `CHANGELOG.md` | docs |

---

### Task 1: Plan statuses and draft guards in the tickets registry

**Files:**
- Modify: `internal/runner/ticket_rules.go` (`isDraft`, `planLock`, `plannable`, `checkDeps`)
- Modify: `internal/runner/tickets.go` (`create`, `update`, `remove`, `clipRunComment`,
  `StartPlan`, `PlanReview`, `settleParents`)
- Test: `internal/runner/ticket_rules_test.go`, `internal/runner/tickets_test.go` (append)

**Interfaces:**
- Produces:
  ```go
  func isDraft(rows []Ticket, t Ticket) bool                   // parent is planning or review
  func planLock(rows []Ticket, parent, run string) error       // run "" is a person
  func plannable(t Ticket, hasKids bool) error                 // the three spec refusals
  func (t *Tickets) create(project string, n NewTicket, run string) (TicketView, error)
  func (t *Tickets) update(project, id string, p TicketPatch, run string) (TicketView, error)
  func (t *Tickets) remove(project, id, run string) error
  func (t *Tickets) StartPlan(project, id, run string) (TicketView, error)
  func (t *Tickets) PlanReview(project, id, run, comment string) (TicketView, error)
  func clipRunComment(s string) string
  ```
  `StartPlan`: a new run needs `plannable` and is appended, the ticket goes to `planning`; the
  last run takes `review` back to `planning` (no-op in `planning`). `PlanReview`: only the last
  run, only from `planning` or `review`; status `review`, comment by `aigem` (cut to 16 KiB).
  `Create`/`Update`/`Delete` keep their signatures and call the unexported ones with `""`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/runner/ticket_rules_test.go`:

```go
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
```

Append to `internal/runner/tickets_test.go`:

```go
func TestAPlanStartsOnlyOnAnOpenTopLevelTicketAndEndsInReview(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ts, _ := newTestTickets(t, t.TempDir())
	goal := mustCreate(t, ts, NewTicket{Title: "goal"})
	split := mustCreate(t, ts, NewTicket{Title: "split"})
	kid := mustCreate(t, ts, NewTicket{Title: "kid", Parent: split.ID})
	ready := mustCreate(t, ts, NewTicket{Title: "ready"})
	if _, err := ts.Update("PRJ-1", ready.ID, TicketPatch{Status: ptr(TicketReady)}); err != nil {
		t.Fatal(err)
	}

	_, err := ts.StartPlan("PRJ-1", kid.ID, "RUN-1")
	refusal(t, err, "TCK-3 is a subticket; only a top-level ticket is planned")
	_, err = ts.StartPlan("PRJ-1", split.ID, "RUN-1")
	refusal(t, err, "TCK-2 already has subtickets")
	_, err = ts.StartPlan("PRJ-1", ready.ID, "RUN-1")
	refusal(t, err, "TCK-4 is ready, not open")

	v, err := ts.StartPlan("PRJ-1", goal.ID, "RUN-1")
	if err != nil || v.Status != TicketPlanning || !slices.Equal(v.Runs, []string{"RUN-1"}) {
		t.Fatalf("plan = %+v, %v", v, err)
	}
	_, err = ts.StartPlan("PRJ-1", goal.ID, "RUN-2")
	refusal(t, err, "TCK-1 is planning, not open")
	_, err = ts.PlanReview("PRJ-1", goal.ID, "RUN-2", "x")
	refusal(t, err, "TCK-1 is driven by another run")

	v, err = ts.PlanReview("PRJ-1", goal.ID, "RUN-1", "Three steps.")
	if err != nil || v.Status != TicketReview || lastComment(v) != "Three steps." || v.Comments[0].By != "aigem" {
		t.Fatalf("review = %+v, %v", v, err)
	}
	refusal(t, ts.Delete("PRJ-1", goal.ID), "TCK-1 is review; it cannot be deleted now")
	v, err = ts.StartPlan("PRJ-1", goal.ID, "RUN-1")
	if err != nil || v.Status != TicketPlanning || len(v.Runs) != 1 {
		t.Fatalf("a person's turn in the planner = %+v, %v, want planning again with the same run", v, err)
	}
	_, err = ts.PlanReview("PRJ-1", ready.ID, "RUN-1", "x")
	refusal(t, err, "TCK-4 is driven by another run")
}

func TestADraftIsHeldWhileItsParentIsPlannedOrReviewed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ts, _ := newTestTickets(t, t.TempDir())
	goal := mustCreate(t, ts, NewTicket{Title: "goal"})
	other := mustCreate(t, ts, NewTicket{Title: "other"})
	if _, err := ts.StartPlan("PRJ-1", goal.ID, "RUN-1"); err != nil {
		t.Fatal(err)
	}
	a, err := ts.create("PRJ-1", NewTicket{Title: "a", Parent: goal.ID, By: "run RUN-1"}, "RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ts.create("PRJ-1", NewTicket{Title: "b", Parent: goal.ID, DependsOn: []string{a.ID}}, "RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := ts.Get("PRJ-1", goal.ID); g.Status != TicketPlanning || g.Progress == nil || g.Progress.Total != 2 {
		t.Fatalf("parent = %s %+v, want planning with two subtickets", g.Status, g.Progress)
	}

	_, err = ts.Create("PRJ-1", NewTicket{Title: "mine", Parent: goal.ID})
	refusal(t, err, "TCK-1 is being planned")
	refusal(t, ts.Delete("PRJ-1", b.ID), "TCK-1 is being planned")
	_, err = ts.Update("PRJ-1", b.ID, TicketPatch{DependsOn: &[]string{}})
	refusal(t, err, "TCK-1 is being planned")
	_, err = ts.Update("PRJ-1", a.ID, TicketPatch{Status: ptr(TicketReady)})
	refusal(t, err, "TCK-3 is part of a plan in review")
	_, err = ts.Update("PRJ-1", other.ID, TicketPatch{DependsOn: &[]string{a.ID}})
	refusal(t, err, "TCK-3 is a draft of TCK-1's plan")
	_, err = ts.create("PRJ-1", NewTicket{Title: "x", Parent: other.ID}, "RUN-1")
	refusal(t, err, "RUN-1 does not plan TCK-2")
	_, err = ts.create("PRJ-1", NewTicket{Title: "x"}, "RUN-1")
	refusal(t, err, "RUN-1 only changes the subtickets of the ticket it plans")
	_, err = ts.create("PRJ-1", NewTicket{Title: "x", Parent: goal.ID}, "RUN-2")
	refusal(t, err, "RUN-2 does not plan TCK-1")
	if _, err := ts.update("PRJ-1", b.ID, TicketPatch{DependsOn: &[]string{}}, "RUN-1"); err != nil {
		t.Fatalf("the planner relinks = %v", err)
	}
	if err := ts.remove("PRJ-1", b.ID, "RUN-1"); err != nil {
		t.Fatalf("the planner deletes = %v", err)
	}

	if _, err := ts.PlanReview("PRJ-1", goal.ID, "RUN-1", "One step."); err != nil {
		t.Fatal(err)
	}
	_, err = ts.create("PRJ-1", NewTicket{Title: "late", Parent: goal.ID}, "RUN-1")
	refusal(t, err, "TCK-1 is review, not planning")
	mine, err := ts.Create("PRJ-1", NewTicket{Title: "mine", Parent: goal.ID, By: "you"})
	if err != nil {
		t.Fatalf("a person adds a subticket in review = %v", err)
	}
	if _, err := ts.Update("PRJ-1", mine.ID, TicketPatch{DependsOn: &[]string{a.ID}}); err != nil {
		t.Fatalf("a person relinks inside the plan = %v", err)
	}
	_, err = ts.Update("PRJ-1", mine.ID, TicketPatch{Status: ptr(TicketClosed)})
	refusal(t, err, mine.ID+" is part of a plan in review")
	if _, err := ts.Update("PRJ-1", mine.ID, TicketPatch{DependsOn: &[]string{}}); err != nil {
		t.Fatal(err)
	}
	if err := ts.Delete("PRJ-1", mine.ID); err != nil {
		t.Fatalf("a person deletes a draft in review = %v", err)
	}
	if g, _ := ts.Get("PRJ-1", goal.ID); g.Status != TicketReview {
		t.Errorf("parent = %s, want review: it is not derived while reviewed", g.Status)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```bash
go test ./internal/runner/ -run 'Planner|Plannable|APlanStarts|ADraftIsHeld' -count=1
```

Expected: FAIL to build: `undefined: planLock`, `undefined: plannable`, `ts.StartPlan undefined`.

- [ ] **Step 3: Write the rules**

In `internal/runner/ticket_rules.go`, in `checkDeps` add one case to the `switch` right after
the `case rows[i].Parent == t.ID:` case:

```go
		case isDraft(rows, rows[i]) && rows[i].Parent != t.Parent:
			return nil, refuse("%s is a draft of %s's plan", d, rows[i].Parent)
```

Append to `internal/runner/ticket_rules.go`:

```go
// isDraft says whether t is a subticket of a plan in progress or in review.
func isDraft(rows []Ticket, t Ticket) bool {
	i := findTicket(rows, t.Parent)
	return i >= 0 && (rows[i].Status == TicketPlanning || rows[i].Status == TicketReview)
}

// planLock says whether run ("" for a person) may add, delete or relink a subticket of parent.
// While parent is planning only its planner run may; a planner run touches nothing else.
func planLock(rows []Ticket, parent, run string) error {
	i := findTicket(rows, parent)
	planning := i >= 0 && rows[i].Status == TicketPlanning
	switch {
	case run == "" && planning:
		return refuse("%s is being planned", parent)
	case run == "":
		return nil
	case i < 0:
		return refuse("%s only changes the subtickets of the ticket it plans", run)
	case !lastRun(rows[i], run):
		return refuse("%s does not plan %s", run, parent)
	case !planning:
		return refuse("%s is %s, not planning", parent, rows[i].Status)
	}
	return nil
}

// plannable refuses a ticket a planner cannot take: only an open top-level ticket without
// subtickets is planned.
func plannable(t Ticket, hasKids bool) error {
	switch {
	case t.Parent != "":
		return refuse("%s is a subticket; only a top-level ticket is planned", t.ID)
	case t.Status != TicketOpen:
		return refuse("%s is %s, not open", t.ID, t.Status)
	case hasKids:
		return refuse("%s already has subtickets", t.ID)
	}
	return nil
}
```

- [ ] **Step 4: Route the registry's writes through the rules**

In `internal/runner/tickets.go`:

Replace the first line of `Create` and its signature so that `Create` delegates:

```go
func (t *Tickets) Create(project string, n NewTicket) (TicketView, error) { return t.create(project, n, "") }

// create adds a ticket for a person (run "") or for the planner run that plans its parent.
func (t *Tickets) create(project string, n NewTicket, run string) (TicketView, error) {
```

and keep the body of the old `Create` below it, with one addition right after the closing `}`
of the `if n.Parent != "" { ... }` block, before `now := t.now()`:

```go
		if err := planLock(tab.Tickets, n.Parent, run); err != nil {
			return nil, err
		}
```

Replace the signature of `Update` the same way:

```go
func (t *Tickets) Update(project, id string, p TicketPatch) (TicketView, error) {
	return t.update(project, id, p, "")
}

// update changes a ticket for a person (run "") or relinks a draft for its planner run.
func (t *Tickets) update(project, id string, p TicketPatch, run string) (TicketView, error) {
```

and in the body change the `p.DependsOn` block and the status `switch` to:

```go
		if p.DependsOn != nil {
			if err := planLock(tab.Tickets, tk.Parent, run); err != nil {
				return nil, err
			}
			deps, err := checkDeps(tab.Tickets, *tk, *p.DependsOn)
			if err != nil {
				return nil, err
			}
			changed = !slices.Equal(deps, tk.DependsOn)
			tk.DependsOn = deps
		}
		if p.Status != nil && *p.Status != tk.Status {
			switch {
			case !validStatus(*p.Status):
				return nil, refuse("unknown status %q", *p.Status)
			case isDraft(tab.Tickets, *tk):
				return nil, refuse("%s is part of a plan in review", id)
			case len(subtickets(tab.Tickets, id)) > 0:
				return nil, refuse("%s follows its subtickets; change them instead", id)
			}
```

(the rest of `update` stays as it was).

Replace `Delete`'s signature and its first lines:

```go
func (t *Tickets) Delete(project, id string) error { return t.remove(project, id, "") }

// remove deletes a ticket for a person (run "") or a draft for the planner run that plans it.
func (t *Tickets) remove(project, id, run string) error {
	_, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		if err := planLock(tab.Tickets, tab.Tickets[i].Parent, run); err != nil {
			return nil, err
		}
```

(the rest of the old `Delete` body follows unchanged, except that its status check becomes):

```go
		if s := tab.Tickets[i].Status; s == TicketRunning || s == TicketPlanning || s == TicketReview {
			return nil, refuse("%s is %s; it cannot be deleted now", id, s)
		}
```

In `Finish`, replace

```go
	if len(comment) > maxRunComment {
		comment = strings.ToValidUTF8(comment[:maxRunComment], "") + "…"
	}
```

with

```go
	comment = clipRunComment(comment)
```

and add after `lastRun`:

```go
func clipRunComment(s string) string {
	if len(s) > maxRunComment {
		return strings.ToValidUTF8(s[:maxRunComment], "") + "…"
	}
	return s
}
```

In `settleParents`, replace

```go
		if s := derive(kids); s != tab.Tickets[pi].Status {
```

with

```go
		// A plan in progress or in review does not follow its draft.
		held := tab.Tickets[pi].Status == TicketPlanning || tab.Tickets[pi].Status == TicketReview
		if s := derive(kids); !held && s != tab.Tickets[pi].Status {
```

- [ ] **Step 5: Add `StartPlan` and `PlanReview`**

Add to `internal/runner/tickets.go` after `Finish`:

```go
// StartPlan hands an open top-level ticket without subtickets to a new planner run. The run
// that already plans it (its last) takes it back from review when a person typed into it.
func (t *Tickets) StartPlan(project, id, run string) (TicketView, error) {
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		tk := &tab.Tickets[i]
		owns := lastRun(*tk, run)
		switch {
		case owns && tk.Status == TicketPlanning:
			return nil, nil
		case owns && tk.Status != TicketReview:
			return nil, refuse("%s is %s, not in review", id, tk.Status)
		case !owns && slices.Contains(tk.Runs, run):
			return nil, refuse("%s is driven by another run", id)
		case !owns:
			if err := plannable(*tk, len(subtickets(tab.Tickets, id)) > 0); err != nil {
				return nil, err
			}
			tk.Runs = append(tk.Runs, run)
		}
		tk.Status, tk.Updated = TicketPlanning, t.now()
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	if len(views) == 0 {
		return t.Get(project, id)
	}
	return views[0], nil
}

// PlanReview hands a plan to a person: the ticket goes to review with the planner's comment.
// Only the ticket's last run may do it.
func (t *Tickets) PlanReview(project, id, run, comment string) (TicketView, error) {
	comment = clipRunComment(comment)
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		tk := &tab.Tickets[i]
		switch {
		case !lastRun(*tk, run):
			return nil, refuse("%s is driven by another run", id)
		case tk.Status != TicketPlanning && tk.Status != TicketReview:
			return nil, refuse("%s is %s; no plan is in progress", id, tk.Status)
		}
		now := t.now()
		tk.Status, tk.Updated = TicketReview, now
		if comment != "" {
			tk.Comments = append(tk.Comments, Comment{At: now, By: "aigem", Text: comment})
		}
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run:

```bash
gofmt -w internal/runner/ticket_rules.go internal/runner/tickets.go internal/runner/*_test.go
go test -race ./internal/runner/ -run 'Ticket|Plan|Draft|Subticket|Delete|Patch|Cycle' -count=1
```

Expected: PASS, including every part 1 and part 2 registry test.

- [ ] **Step 7: Commit**

```bash
git add internal/runner/ticket_rules.go internal/runner/tickets.go \
  internal/runner/ticket_rules_test.go internal/runner/tickets_test.go
git commit -m "feat(runner): plan statuses and draft guards in the tickets registry"
```

---

### Task 2: Approve, Reject and Revise in the tickets registry

**Files:**
- Modify: `internal/runner/tickets.go` (`inReview`, `Approve`, `Reject`, `Revise`)
- Test: `internal/runner/tickets_test.go` (append)

**Interfaces:**
- Consumes: `StartPlan`, `PlanReview`, `create` (Task 1).
- Produces:
  ```go
  func (t *Tickets) Approve(project, id string) (TicketView, error)          // the parent's view
  func (t *Tickets) Reject(project, id, reason string) (TicketView, error)
  func (t *Tickets) Revise(project, id, run, feedback string) (TicketView, error)
  ```
  All three need `review` (`<id> is <status>, not in review`). `Approve`: every subticket
  `ready`, the parent derived (so `ready`); refused with none. `Reject`: a non-blank reason
  (`a rejection needs a reason`), the subtickets deleted, the ticket `open`, comment
  `Plan rejected: <reason>` by `you`. `Revise`: non-blank feedback (`feedback cannot be
  empty`), comment by `you`, `run` appended when it is not the last run, status `planning`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/runner/tickets_test.go`:

```go
func TestAPersonApprovesARevisedPlan(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ts, _ := newTestTickets(t, t.TempDir())
	goal := mustCreate(t, ts, NewTicket{Title: "goal"})
	_, err := ts.Approve("PRJ-1", goal.ID)
	refusal(t, err, "TCK-1 is open, not in review")
	if _, err := ts.StartPlan("PRJ-1", goal.ID, "RUN-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.PlanReview("PRJ-1", goal.ID, "RUN-1", "Nothing to split."); err != nil {
		t.Fatal(err)
	}
	_, err = ts.Approve("PRJ-1", goal.ID)
	refusal(t, err, "TCK-1's plan has no subtickets; revise or reject it")
	_, err = ts.Revise("PRJ-1", goal.ID, "RUN-1", "  ")
	refusal(t, err, "feedback cannot be empty")

	v, err := ts.Revise("PRJ-1", goal.ID, "RUN-1", "Split it in two.")
	if err != nil || v.Status != TicketPlanning || len(v.Runs) != 1 || lastComment(v) != "Split it in two." ||
		v.Comments[len(v.Comments)-1].By != "you" {
		t.Fatalf("revise = %+v, %v", v, err)
	}
	a, err := ts.create("PRJ-1", NewTicket{Title: "a", Parent: goal.ID}, "RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ts.create("PRJ-1", NewTicket{Title: "b", Parent: goal.ID, DependsOn: []string{a.ID}}, "RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.PlanReview("PRJ-1", goal.ID, "RUN-1", "Two steps."); err != nil {
		t.Fatal(err)
	}
	v, err = ts.Approve("PRJ-1", goal.ID)
	if err != nil || v.ID != goal.ID || v.Status != TicketReady {
		t.Fatalf("approve = %s %s, %v, want the parent ready", v.ID, v.Status, err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if k, _ := ts.Get("PRJ-1", id); k.Status != TicketReady {
			t.Errorf("%s = %s, want ready", id, k.Status)
		}
	}
	if k, _ := ts.Get("PRJ-1", a.ID); !k.Runnable {
		t.Error("the first step of an approved plan is not runnable")
	}
	_, err = ts.Reject("PRJ-1", goal.ID, "x")
	refusal(t, err, "TCK-1 is ready, not in review")
}

func TestARejectedPlanLosesItsDraftAndAReviseCanTakeANewRun(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ts, _ := newTestTickets(t, t.TempDir())
	goal := mustCreate(t, ts, NewTicket{Title: "goal"})
	if _, err := ts.StartPlan("PRJ-1", goal.ID, "RUN-1"); err != nil {
		t.Fatal(err)
	}
	a, _ := ts.create("PRJ-1", NewTicket{Title: "a", Parent: goal.ID}, "RUN-1")
	if _, err := ts.create("PRJ-1", NewTicket{Title: "b", Parent: goal.ID, DependsOn: []string{a.ID}},
		"RUN-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.PlanReview("PRJ-1", goal.ID, "RUN-1", "Two steps."); err != nil {
		t.Fatal(err)
	}
	_, err := ts.Reject("PRJ-1", goal.ID, " ")
	refusal(t, err, "a rejection needs a reason")

	v, err := ts.Revise("PRJ-1", goal.ID, "RUN-4", "Try again.")
	if err != nil || !slices.Equal(v.Runs, []string{"RUN-1", "RUN-4"}) || v.Status != TicketPlanning {
		t.Fatalf("revise with a new run = %+v, %v", v, err)
	}
	if _, err := ts.PlanReview("PRJ-1", goal.ID, "RUN-4", "Same plan."); err != nil {
		t.Fatal(err)
	}
	v, err = ts.Reject("PRJ-1", goal.ID, "too big")
	if err != nil || v.Status != TicketOpen || v.Progress != nil || lastComment(v) != "Plan rejected: too big" {
		t.Fatalf("reject = %+v, %v", v, err)
	}
	if list, _ := ts.List("PRJ-1"); len(list) != 1 {
		t.Errorf("tickets after reject = %d, want only the parent", len(list))
	}
	if _, err := ts.StartPlan("PRJ-1", goal.ID, "RUN-5"); err != nil {
		t.Errorf("a rejected ticket cannot be planned again: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```bash
go test ./internal/runner/ -run 'ApprovesARevisedPlan|ARejectedPlan' -count=1
```

Expected: FAIL to build: `ts.Approve undefined`.

- [ ] **Step 3: Write the moves**

Add to `internal/runner/tickets.go` after `PlanReview`:

```go
func inReview(rows []Ticket, id string) (int, error) {
	i := findTicket(rows, id)
	switch {
	case i < 0:
		return -1, ErrNoTicket
	case rows[i].Status != TicketReview:
		return -1, refuse("%s is %s, not in review", id, rows[i].Status)
	}
	return i, nil
}

// Approve makes a reviewed plan's subtickets ready and hands its parent back to them.
func (t *Tickets) Approve(project, id string) (TicketView, error) {
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i, err := inReview(tab.Tickets, id)
		if err != nil {
			return nil, err
		}
		now := t.now()
		touched := []string{id}
		for k := range tab.Tickets {
			if tab.Tickets[k].Parent == id {
				tab.Tickets[k].Status, tab.Tickets[k].Updated = TicketReady, now
				touched = append(touched, tab.Tickets[k].ID)
			}
		}
		if len(touched) == 1 {
			return nil, refuse("%s's plan has no subtickets; revise or reject it", id)
		}
		tab.Tickets[i].Status, tab.Tickets[i].Updated = derive(subtickets(tab.Tickets, id)), now
		return touched, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}

// Reject deletes a reviewed plan's subtickets and opens its ticket again with the reason.
func (t *Tickets) Reject(project, id, reason string) (TicketView, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return TicketView{}, refuse("a rejection needs a reason")
	}
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		if _, err := inReview(tab.Tickets, id); err != nil {
			return nil, err
		}
		touched := []string{id}
		tab.Tickets = slices.DeleteFunc(tab.Tickets, func(k Ticket) bool {
			if k.Parent == id {
				touched = append(touched, k.ID)
			}
			return k.Parent == id
		})
		now := t.now()
		tk := &tab.Tickets[findTicket(tab.Tickets, id)]
		tk.Status, tk.Updated = TicketOpen, now
		tk.Comments = append(tk.Comments, Comment{At: now, By: "you", Text: "Plan rejected: " + reason})
		return touched, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}

// Revise sends a reviewed plan back to planning under run, with the person's feedback.
func (t *Tickets) Revise(project, id, run, feedback string) (TicketView, error) {
	if strings.TrimSpace(feedback) == "" {
		return TicketView{}, refuse("feedback cannot be empty")
	}
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i, err := inReview(tab.Tickets, id)
		if err != nil {
			return nil, err
		}
		tk := &tab.Tickets[i]
		if !lastRun(*tk, run) {
			tk.Runs = append(tk.Runs, run)
		}
		now := t.now()
		tk.Status, tk.Updated = TicketPlanning, now
		tk.Comments = append(tk.Comments, Comment{At: now, By: "you", Text: feedback})
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:

```bash
gofmt -w internal/runner/tickets.go internal/runner/tickets_test.go
go test -race ./internal/runner/ -run 'Ticket|Plan|Draft|Subticket|Delete|Patch' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/runner/tickets.go internal/runner/tickets_test.go
git commit -m "feat(runner): approve, reject and revise a plan in the tickets registry"
```

---

### Task 3: A run with a capability profile and a root that is not a worktree

**Files:**
- Modify: `internal/runner/session.go` (`Spec.Profile`, `Mode.CapabilitySubset`)
- Modify: `internal/runner/runs.go` (`RunRequest.Dir`, `Profile`, `Root`, `allowed`,
  `readOnlyProfile`, `Create`)
- Modify: `cmd/aigem/webruns.go` (`openRun` passes `Profile`)
- Test: `internal/runner/session_test.go`, `internal/runner/runs_ticket_test.go`

**Interfaces:**
- Produces:
  ```go
  const readOnlyProfile = "read-only"                       // package runner
  type RunRequest struct { ...; Dir, Profile string }       // Dir roots a run without a worktree
  func (req RunRequest) Root(dir string) string             // Worktree, else Dir, else dir
  func (m Mode) CapabilitySubset(profile string) []string   // "" is the default profile
  type Spec struct { ...; Profile string }
  ```
  `Create` returns `ErrRunMode` for an unknown profile, a profile on an interactive run, and an
  autonomous run that has neither a worktree nor (`Profile == "read-only"` and `Dir`).

- [ ] **Step 1: Write the failing tests**

In `internal/runner/session_test.go` add `"slices"` to the imports and change
`TestModeDerivesAutonomousCapabilityAndBudget`'s two `CapabilitySubset()` calls to
`CapabilitySubset("")`; then add before its turn-budget check:

```go
	ro := runner.ModeAutonomous.CapabilitySubset("read-only")
	if slices.Contains(ro, "write_file") || slices.Contains(ro, "bash") || !slices.Contains(ro, "read_file") {
		t.Fatalf("read-only subset = %v", ro)
	}
```

In `internal/runner/runs_ticket_test.go` add `"path/filepath"` to the imports, change the
`NewSession` line of `scriptedOpen` to

```go
		sess := NewSession(Spec{Mode: req.Mode, Profile: req.Profile, Tools: reg, Backend: llm.NewRef(s),
			Title: req.Title})
```

add to `TestARunIsRootedAtItsWorktreeWhenItHasOne`:

```go
	if got := (RunRequest{Dir: "/p/api"}).Root("/p"); got != "/p/api" {
		t.Errorf("a run with a dir = %q, want its dir", got)
	}
```

replace `TestAnAutonomousRunNeedsATicketAndAWorktree` with:

```go
func TestAnAutonomousRunNeedsATicketAndAWorktreeOrOnlyReads(t *testing.T) {
	runs, err := NewRuns(RunsConfig{Open: scriptedOpen(&scripted{}, t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)
	for _, req := range []RunRequest{
		{Mode: ModeAutonomous, TicketID: "TCK-1"},
		{Mode: ModeAutonomous, Worktree: t.TempDir()},
		{Mode: ModeAutonomous, TicketID: "TCK-1", Dir: t.TempDir()},
		{Mode: ModeAutonomous, TicketID: "TCK-1", Dir: t.TempDir(), Profile: "shell"},
		{Mode: ModeAutonomous, TicketID: "TCK-1", Profile: readOnlyProfile},
		{Mode: ModeAutonomous, TicketID: "TCK-1", Worktree: t.TempDir(), Profile: "nope"},
		{Profile: readOnlyProfile},
	} {
		if _, err := runs.Create(context.Background(), req); !errors.Is(err, ErrRunMode) {
			t.Errorf("Create(%+v) = %v, want ErrRunMode", req, err)
		}
	}
}

func TestAReadOnlyRunIsRootedAtItsDirAndCannotWrite(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := &scripted{}
	s.then(call("write_file", `{"path":"x.txt","content":"x"}`), call("probe", `{}`), say("read it"))
	runs, err := NewRuns(RunsConfig{Open: scriptedOpen(s, t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)

	dir := t.TempDir()
	p := &probe{}
	turns := make(chan uisession.Event, 8)
	v, err := runs.Create(context.Background(), RunRequest{
		Mode: ModeAutonomous, Profile: readOnlyProfile, TicketID: "TCK-1", Dir: dir,
		Tools: []tools.Tool{p}, OnTurn: func(ev uisession.Event) { turns <- ev },
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Root != dir || v.Worktree != "" || v.TicketID != "TCK-1" {
		t.Fatalf("run = %+v", v)
	}
	if err := runs.Apply(v.ID, RunOp{Op: OpSubmit, Text: "plan"}); err != nil {
		t.Fatal(err)
	}
	nextTurn(t, turns)
	if ev := nextTurn(t, turns); ev.Kind != uisession.KindTurnEnd {
		t.Fatalf("second event = %s, want turn_end", ev.Kind)
	}
	if pathExists(filepath.Join(dir, "x.txt")) {
		t.Error("a read-only run wrote a file")
	}
	if !p.ran.Load() {
		t.Error("the extra tool was not offered past the read-only subset")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```bash
go test ./internal/runner/ -run 'Mode|Rooted|OnlyReads|ReadOnlyRun' -count=1
```

Expected: FAIL to build: `unknown field Profile in struct literal of type Spec`.

- [ ] **Step 3: The profile on the session**

In `internal/runner/session.go` replace `CapabilitySubset` with:

```go
// CapabilitySubset returns the tool names exposed to an autonomous session under the named
// capability profile ("" is the default). Interactive and unknown modes retain the complete
// registry. The returned slice is independent so callers cannot mutate the profile shared by
// other sessions.
func (m Mode) CapabilitySubset(profile string) []string {
	if m != ModeAutonomous {
		return nil
	}
	p, err := tools.ResolveCapabilityProfile(profile)
	if err != nil {
		// Runs.Create refuses an unknown name and the default is validated by tools' tests; a
		// failure here is a programming error, not user input.
		panic("runner: capability profile unavailable: " + err.Error())
	}
	return append([]string(nil), p.Allow...)
}
```

In `Spec`, right after `Mode Mode`, add:

```go
	// Profile names the capability profile of an autonomous session; empty is the default.
	Profile string
```

In `NewSession` change `spec.Mode.CapabilitySubset()` to `spec.Mode.CapabilitySubset(spec.Profile)`.

- [ ] **Step 4: The profile and the root on the request**

In `internal/runner/runs.go`, in `RunRequest` after the `TicketID, Worktree, Branch string`
field add:

```go
	// Dir roots a run that has no worktree, such as a planner at a repository's main checkout.
	Dir string
	// Profile names the capability profile of an autonomous run; empty is the default.
	Profile string
```

Replace `Root` with:

```go
// Root is where the session's tools are rooted: the ticket's worktree, else Dir, else dir.
func (req RunRequest) Root(dir string) string {
	switch {
	case req.Worktree != "":
		return req.Worktree
	case req.Dir != "":
		return req.Dir
	}
	return dir
}

// readOnlyProfile is the capability profile a run without a worktree must have.
const readOnlyProfile = "read-only"

// allowed says whether the request's mode, profile and root go together. The autonomous policy
// approves edits on the assumption that a ticket's worktree is all the session can reach; a run
// without one may only read.
func (req RunRequest) allowed() bool {
	if _, err := tools.ResolveCapabilityProfile(req.Profile); err != nil {
		return false
	}
	switch req.Mode {
	case ModeInteractive:
		return req.Profile == ""
	case ModeAutonomous:
		return req.TicketID != "" && (req.Worktree != "" || req.Profile == readOnlyProfile && req.Dir != "")
	}
	return false
}
```

In `Create` replace

```go
	if req.Mode != ModeInteractive && (req.Mode != ModeAutonomous || req.TicketID == "" || req.Worktree == "") {
		// The autonomous policy approves edits on the assumption that a ticket's worktree is
		// all the session can reach; without one there is nothing that assumption holds for.
		return RunView{}, fmt.Errorf("%w: %q", ErrRunMode, req.Mode)
	}
```

with

```go
	if !req.allowed() {
		return RunView{}, fmt.Errorf("%w: %q", ErrRunMode, req.Mode)
	}
```

In `cmd/aigem/webruns.go`, in the `runner.Spec{...}` literal of `openRun`, add after
`Mode:    req.Mode,`:

```go
		Profile: req.Profile,
```

- [ ] **Step 5: Run the tests to verify they pass**

Run:

```bash
gofmt -w internal/runner/session.go internal/runner/runs.go cmd/aigem/webruns.go internal/runner/*_test.go
go build ./... && go test -race ./internal/runner/ ./cmd/aigem/ -count=1
```

Expected: PASS apart from the known macOS failures.

- [ ] **Step 6: Commit**

```bash
git add internal/runner/session.go internal/runner/runs.go cmd/aigem/webruns.go \
  internal/runner/session_test.go internal/runner/runs_ticket_test.go
git commit -m "feat(runner): a read-only autonomous run rooted outside a worktree"
```

---

### Task 4: The planner's ticket tools

**Files:**
- Modify: `internal/runner/ticketruns.go` (`ticketRun.plan`)
- Create: `internal/runner/plantools.go`
- Test: `internal/runner/plantools_test.go` (new)

**Interfaces:**
- Consumes: `create`, `update`, `remove`, `planLock`, `StartPlan`, `PlanReview` (Task 1);
  `Projects.Repositories` (part 2).
- Produces:
  ```go
  type ticketRun struct { ...; plan bool }                     // a planner run
  func (t *TicketRuns) planTools(tr *ticketRun) []tools.Tool   // the six tools below
  func (t *TicketRuns) planning(tr *ticketRun) error           // planLock on the run's ticket
  ```
  Tools: `list_tickets()`, `get_ticket(id)`, `create_subticket(repo, title, body, dependsOn)`
  (answers `Created <id>.`), `delete_subticket(id)` (`Deleted <id>.`),
  `set_dependencies(id, dependsOn)` (`<id> waits for [<ids>].`), `plan_done(summary)` (records
  the trimmed summary in `tr.summary`). Each refuses unless the run's ticket is `planning` and
  the run is its last run; a write refuses anything but that ticket's subtickets; an unknown
  repo is `"<repo>" is not a repository of this project`.

- [ ] **Step 1: Write the failing tests**

Create `internal/runner/plantools_test.go`:

```go
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/tools"
)

// planning makes an open ticket, hands it to RUN-1 as its planner, and returns that run.
func (f *fixture) planning(title string) (string, *ticketRun) {
	f.t.Helper()
	v, err := f.tickets.Create(f.project, NewTicket{Title: title})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.tickets.StartPlan(f.project, v.ID, "RUN-1"); err != nil {
		f.t.Fatal(err)
	}
	return v.ID, &ticketRun{project: f.project, ticket: v.ID, run: "RUN-1", plan: true}
}

func runTool(t *testing.T, ts []tools.Tool, name, args string) (string, error) {
	t.Helper()
	for _, tool := range ts {
		if tool.Name() == name {
			if tool.NeedsConfirm() {
				t.Errorf("%s asks for a confirmation", name)
			}
			return tool.Run(context.Background(), json.RawMessage(args))
		}
	}
	t.Fatalf("no tool %s", name)
	return "", nil
}

func TestThePlannerToolsWriteSubticketsOfItsTicketOnly(t *testing.T) {
	f := newFixture(t, t.TempDir())
	api := filepath.Join(f.repo, "api")
	if err := os.MkdirAll(api, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, api, "init", "-q", "-b", "main")
	id, tr := f.planning("goal")
	if _, err := f.tickets.Create(f.project, NewTicket{Title: "other"}); err != nil {
		t.Fatal(err)
	}
	ts := f.tr.planTools(tr)

	out, err := runTool(t, ts, "create_subticket", `{"repo":"api","title":"Schema","body":"Add the table."}`)
	if err != nil || out != "Created TCK-3." {
		t.Fatalf("create = %q, %v", out, err)
	}
	_, err = runTool(t, ts, "create_subticket", `{"repo":"web","title":"UI"}`)
	refusal(t, err, `"web" is not a repository of this project`)
	if _, err := runTool(t, ts, "create_subticket", `{"title":"Handler","dependsOn":["TCK-3"]}`); err != nil {
		t.Fatal(err)
	}
	kid, _ := f.tickets.Get(f.project, "TCK-4")
	if kid.Parent != id || kid.By != "run RUN-1" || kid.Status != TicketOpen || kid.Repo != "" ||
		!slices.Equal(kid.DependsOn, []string{"TCK-3"}) {
		t.Fatalf("subticket = %+v", kid)
	}
	out, err = runTool(t, ts, "list_tickets", `{}`)
	if err != nil || !strings.Contains(out, `"id":"TCK-4"`) || !strings.Contains(out, `"title":"other"`) {
		t.Errorf("list = %s, %v", out, err)
	}
	out, err = runTool(t, ts, "get_ticket", `{"id":"TCK-3"}`)
	if err != nil || !strings.Contains(out, "Add the table.") || !strings.Contains(out, `"repo":"api"`) {
		t.Errorf("get = %s, %v", out, err)
	}
	out, err = runTool(t, ts, "set_dependencies", `{"id":"TCK-4","dependsOn":[]}`)
	if err != nil || out != "TCK-4 waits for []." {
		t.Errorf("set_dependencies = %q, %v", out, err)
	}
	_, err = runTool(t, ts, "set_dependencies", `{"id":"TCK-2","dependsOn":["TCK-3"]}`)
	refusal(t, err, "RUN-1 only changes the subtickets of the ticket it plans")
	_, err = runTool(t, ts, "delete_subticket", `{"id":"TCK-2"}`)
	refusal(t, err, "RUN-1 only changes the subtickets of the ticket it plans")
	if out, err := runTool(t, ts, "delete_subticket", `{"id":"TCK-4"}`); err != nil || out != "Deleted TCK-4." {
		t.Fatalf("delete = %q, %v", out, err)
	}
	if _, err := f.tickets.Get(f.project, "TCK-4"); !errors.Is(err, ErrNoTicket) {
		t.Errorf("the deleted subticket = %v", err)
	}
	if _, err := runTool(t, ts, "plan_done", `{"summary":"  "}`); err == nil {
		t.Error("an empty summary was recorded")
	}
	if _, err := runTool(t, ts, "plan_done", `{"summary":" One step. "}`); err != nil {
		t.Fatal(err)
	}
	if s := tr.summary.Load(); s == nil || *s != "One step." {
		t.Errorf("summary = %v", s)
	}
}

func TestThePlannerToolsActOnlyWhileTheirRunPlans(t *testing.T) {
	f := newFixture(t, t.TempDir())
	id, tr := f.planning("goal")
	stale := &ticketRun{project: f.project, ticket: id, run: "RUN-0", plan: true}
	for _, c := range [][2]string{{"create_subticket", `{"title":"x"}`}, {"list_tickets", `{}`}} {
		_, err := runTool(t, f.tr.planTools(stale), c[0], c[1])
		refusal(t, err, "RUN-0 does not plan TCK-1")
	}
	if _, err := f.tickets.PlanReview(f.project, id, "RUN-1", "done"); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{
		{"list_tickets", `{}`}, {"get_ticket", `{"id":"TCK-1"}`}, {"plan_done", `{"summary":"x"}`},
		{"create_subticket", `{"title":"x"}`},
	} {
		_, err := runTool(t, f.tr.planTools(tr), c[0], c[1])
		refusal(t, err, "TCK-1 is review, not planning")
	}
	if tr.summary.Load() != nil {
		t.Error("plan_done recorded a summary outside planning")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/runner/ -run 'ThePlannerTools' -count=1`
Expected: FAIL to build: `unknown field plan in struct literal of type ticketRun`.

- [ ] **Step 3: Mark a planner run**

In `internal/runner/ticketruns.go`, in `type ticketRun struct`, change the first field line to:

```go
	project, ticket, run string
	// plan marks a planner run: its turn end sends the ticket to review.
	plan    bool
	place   ticketPlace
```

(keep `summary`, `ctx`, `cancel`, `mu` and `gone` as they are; gofmt aligns the block).

- [ ] **Step 4: Write the tools**

Create `internal/runner/plantools.go`:

```go
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/gigovich/aigem/internal/tools"
)

// planTool is one ticket tool of a planner run.
type planTool struct {
	name, desc, schema string
	run                func(ctx context.Context, args json.RawMessage) (string, error)
}

func (p *planTool) Name() string            { return p.name }
func (p *planTool) Description() string     { return p.desc }
func (p *planTool) Schema() json.RawMessage { return json.RawMessage(p.schema) }
func (*planTool) NeedsConfirm() bool        { return false }

func (p *planTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	return p.run(ctx, args)
}

// planTicket is a ticket as a planner reads it.
type planTicket struct {
	ID        string    `json:"id"`
	Repo      string    `json:"repo,omitempty"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Parent    string    `json:"parent,omitempty"`
	DependsOn []string  `json:"dependsOn,omitempty"`
	Body      string    `json:"body,omitempty"`
	Comments  []Comment `json:"comments,omitempty"`
}

const (
	idSchema   = `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`
	depsSchema = `{"type":"array","items":{"type":"string"},"description":"ids of the tickets it waits for"}`
)

// planTools are a planner run's ticket tools. Each acts only while the run's ticket is planning
// under this run, and every change goes through Tickets with the run's id, so the rules hold.
func (t *TicketRuns) planTools(tr *ticketRun) []tools.Tool {
	return []tools.Tool{
		&planTool{"list_tickets", "List the project's tickets: id, repo, title, status, parent and dependencies.",
			`{"type":"object","properties":{}}`,
			func(context.Context, json.RawMessage) (string, error) {
				if err := t.planning(tr); err != nil {
					return "", err
				}
				views, err := t.tickets.List(tr.project)
				if err != nil {
					return "", err
				}
				out := make([]planTicket, 0, len(views))
				for _, v := range views {
					out = append(out, planTicket{ID: v.ID, Repo: v.Repo, Title: v.Title, Status: v.Status,
						Parent: v.Parent, DependsOn: v.DependsOn})
				}
				return jsonText(out)
			}},
		&planTool{"get_ticket", "Read one ticket of the project with its body and discussion.", idSchema,
			func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				if err := t.planning(tr); err != nil {
					return "", err
				}
				v, err := t.tickets.Get(tr.project, in.ID)
				if err != nil {
					return "", err
				}
				return jsonText(planTicket{ID: v.ID, Repo: v.Repo, Title: v.Title, Status: v.Status,
					Parent: v.Parent, DependsOn: v.DependsOn, Body: v.Body, Comments: v.Comments})
			}},
		&planTool{"create_subticket", "Create a subticket of the ticket you plan. repo is one of the " +
			`project's repositories, "" for the project directory; dependsOn lists the tickets it waits for.`,
			`{"type":"object","properties":{"repo":{"type":"string"},"title":{"type":"string"},` +
				`"body":{"type":"string"},"dependsOn":` + depsSchema + `},"required":["title"]}`,
			func(ctx context.Context, args json.RawMessage) (string, error) {
				var in struct {
					Repo      string   `json:"repo"`
					Title     string   `json:"title"`
					Body      string   `json:"body"`
					DependsOn []string `json:"dependsOn"`
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				if err := t.planRepo(ctx, tr.project, in.Repo); err != nil {
					return "", err
				}
				v, err := t.tickets.create(tr.project, NewTicket{Repo: in.Repo, Title: in.Title, Body: in.Body,
					Parent: tr.ticket, DependsOn: in.DependsOn, By: "run " + tr.run}, tr.run)
				if err != nil {
					return "", err
				}
				return "Created " + v.ID + ".", nil
			}},
		&planTool{"delete_subticket", "Delete a subticket of the ticket you plan.", idSchema,
			func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				if err := t.tickets.remove(tr.project, in.ID, tr.run); err != nil {
					return "", err
				}
				return "Deleted " + in.ID + ".", nil
			}},
		&planTool{"set_dependencies", "Replace the tickets a subticket of the ticket you plan waits for.",
			`{"type":"object","properties":{"id":{"type":"string"},"dependsOn":` + depsSchema +
				`},"required":["id","dependsOn"]}`,
			func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct {
					ID        string   `json:"id"`
					DependsOn []string `json:"dependsOn"`
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				v, err := t.tickets.update(tr.project, in.ID, TicketPatch{DependsOn: &in.DependsOn}, tr.run)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("%s waits for [%s].", v.ID, strings.Join(v.DependsOn, ", ")), nil
			}},
		&planTool{"plan_done", "Call this once the plan is complete, with a short summary for the person " +
			"who reviews it. Do not call it while you still need an answer.",
			`{"type":"object","properties":{"summary":{"type":"string",` +
				`"description":"The plan in a few sentences."}},"required":["summary"]}`,
			func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct {
					Summary string `json:"summary"`
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				if err := t.planning(tr); err != nil {
					return "", err
				}
				summary := strings.TrimSpace(in.Summary)
				if summary == "" {
					return "", errors.New("a summary is required")
				}
				tr.record(summary)
				return "Recorded. End your turn now; a person reviews the plan.", nil
			}},
	}
}

// planning refuses a planner tool call unless the run's ticket is planning under this run.
func (t *TicketRuns) planning(tr *ticketRun) error {
	v, err := t.tickets.Get(tr.project, tr.ticket)
	if err != nil {
		return err
	}
	return planLock([]Ticket{v.Ticket}, tr.ticket, tr.run)
}

// planRepo refuses a repository that is not one of the project's checkouts; "" is the project
// directory itself.
func (t *TicketRuns) planRepo(ctx context.Context, project, repo string) error {
	if repo == "" {
		return nil
	}
	repos, err := t.projects.Repositories(ctx, project)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(repos, func(r Repository) bool { return r.Name == repo }) {
		return refuse("%q is not a repository of this project", repo)
	}
	return nil
}

func jsonText(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run:

```bash
gofmt -w internal/runner/plantools.go internal/runner/plantools_test.go internal/runner/ticketruns.go
go test -race ./internal/runner/ -run 'ThePlannerTools|Ticket' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/runner/plantools.go internal/runner/plantools_test.go internal/runner/ticketruns.go
git commit -m "feat(runner): the planner's ticket tools"
```

---

### Task 5: `TicketRuns.Plan` and the end of a planner turn

**Files:**
- Create: `internal/runner/ticketplan.go`
- Modify: `internal/runner/ticketruns.go` (`TicketRunsConfig.Planned`, `TicketRuns.planned`,
  `onTurn`, `ticketText`)
- Test: `internal/runner/ticketplan_test.go` (new), `internal/runner/ticketruns_test.go`
  (`newFixture` reports plans)

**Interfaces:**
- Consumes: `StartPlan`, `PlanReview`, `plannable` (Task 1); `RunRequest.Dir`, `Profile`,
  `readOnlyProfile` (Task 3); `planTools`, `ticketRun.plan` (Task 4).
- Produces:
  ```go
  type TicketRunsConfig struct { ...; Planned func(project string, v TicketView, comment string) }
  func (t *TicketRuns) Plan(ctx context.Context, project, id string) (RunView, error)
  func (t *TicketRuns) openPlanner(ctx context.Context, project string, tk TicketView,
      move func(run string) (TicketView, error)) (RunView, error)
  func (t *TicketRuns) review(project, id, run, comment string)    // PlanReview + Planned
  func planPrompt(tk Ticket, draft []TicketView, repos []Repository) string
  func ticketText(tk Ticket) string                                // ticketPrompt without the rule
  const planRule = "Split this ticket into subtickets ..."
  ```
  At a planner's turn end, while the ticket is `planning` under it: `review` with the
  `plan_done` summary, else the last message; `interrupted` and `the turn ended with an error:
  ...` win over `plan_done`. A turn start takes `review` back to `planning`.

- [ ] **Step 1: Write the failing tests**

In `internal/runner/ticketruns_test.go`, in `newFixture`'s `NewTicketRuns(TicketRunsConfig{...})`
add after the `Finished` callback:

```go
		Planned: func(_ string, v TicketView, comment string) {
			f.mu.Lock()
			f.told = append(f.told, v.Status+": "+comment)
			f.mu.Unlock()
		},
```

Create `internal/runner/ticketplan_test.go`:

```go
package runner

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/uisession"
)

func (f *fixture) openTicket(title string) string {
	f.t.Helper()
	v, err := f.tickets.Create(f.project, NewTicket{Title: title, Body: "Plan " + title + "."})
	if err != nil {
		f.t.Fatal(err)
	}
	return v.ID
}

func (f *fixture) plan(id string) RunView {
	f.t.Helper()
	v, err := f.tr.Plan(context.Background(), f.project, id)
	if err != nil {
		f.t.Fatalf("Plan %s: %v", id, err)
	}
	return v
}

// firstMessage is the first thing a run was told.
func (f *fixture) firstMessage(run string) string {
	f.t.Helper()
	evs, err := f.runs.Events(run, 0, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Kind == uisession.KindUserMessage {
			return ev.Text
		}
	}
	return ""
}

func TestAPlannerRunSplitsTheTicketAndHandsItToAPerson(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	id := f.openTicket("big goal")
	f.script.then(
		call("create_subticket", `{"title":"Schema"}`),
		call("create_subticket", `{"title":"Handler","dependsOn":["TCK-2"]}`),
		call("write_file", `{"path":"x.txt","content":"x"}`),
		call("plan_done", `{"summary":"Two steps."}`),
		say("Planned."),
	)
	v := f.plan(id)
	if v.TicketID != id || v.Mode != ModeAutonomous || v.Root != f.repo || v.Worktree != "" {
		t.Fatalf("run = %+v", v)
	}
	got := f.next(id, 0)
	if got.Status != TicketReview || lastComment(got) != "Two steps." || got.Progress == nil ||
		got.Progress.Total != 2 || !slices.Equal(got.Runs, []string{v.ID}) {
		t.Fatalf("ticket = %s %q %+v", got.Status, lastComment(got), got.Progress)
	}
	if k, _ := f.tickets.Get(f.project, "TCK-3"); k.Status != TicketOpen || k.By != "run "+v.ID ||
		!slices.Equal(k.DependsOn, []string{"TCK-2"}) {
		t.Errorf("draft = %+v", k)
	}
	if pathExists(filepath.Join(f.repo, "x.txt")) {
		t.Error("the planner wrote a file")
	}
	if rv, _ := f.runs.Get(v.ID); !rv.Live {
		t.Error("the planner run ended at review; Revise needs it")
	}
	if msg := f.firstMessage(v.ID); !strings.Contains(msg, "# TCK-1: big goal") || !strings.Contains(msg, planRule) {
		t.Errorf("first message = %q", msg)
	}
	waitUntil(t, func() bool { return f.toldAbout("review: Two steps.") })
}

func TestAPlannerTurnAlwaysEndsInReview(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	ask := f.openTicket("ask")
	f.script.then(say("Should the API be versioned?"))
	f.plan(ask)
	if got := f.next(ask, 0); got.Status != TicketReview || lastComment(got) != "Should the API be versioned?" {
		t.Fatalf("without plan_done = %s %q", got.Status, lastComment(got))
	}

	broken := f.openTicket("broken")
	f.script.then(call("plan_done", `{"summary":"Done."}`), func(context.Context) (llm.Message, error) {
		return llm.Message{}, errors.New("the model is gone")
	})
	f.plan(broken)
	if got := f.next(broken, 0); got.Status != TicketReview ||
		!strings.HasPrefix(lastComment(got), "the turn ended with an error: ") {
		t.Fatalf("an error after plan_done = %s %q", got.Status, lastComment(got))
	}

	slow := f.openTicket("slow")
	f.script.then(hold())
	v := f.plan(slow)
	waitUntil(t, func() bool { rv, _ := f.runs.Get(v.ID); return rv.Running })
	if err := f.runs.Apply(v.ID, RunOp{Op: OpInterrupt}); err != nil {
		t.Fatal(err)
	}
	if got := f.next(slow, 0); got.Status != TicketReview || lastComment(got) != "interrupted" {
		t.Fatalf("interrupted = %s %q", got.Status, lastComment(got))
	}

	f.script.then(call("plan_done", `{"summary":"Asked and answered."}`), say("ok"))
	if err := f.runs.Apply(v.ID, RunOp{Op: OpSubmit, Text: "go on"}); err != nil {
		t.Fatal(err)
	}
	if got := f.next(slow, 1); got.Status != TicketReview || lastComment(got) != "Asked and answered." {
		t.Fatalf("a person's turn in the planner = %s %q", got.Status, lastComment(got))
	}
}

func TestPlanRefusesATicketThatCannotBePlanned(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, gitRepo(t, "main"))
	goal := f.openTicket("goal")
	kid, err := f.tickets.Create(f.project, NewTicket{Title: "kid", Parent: goal})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.tr.Plan(ctx, f.project, kid.ID)
	refusal(t, err, "TCK-2 is a subticket; only a top-level ticket is planned")
	_, err = f.tr.Plan(ctx, f.project, goal)
	refusal(t, err, "TCK-1 already has subtickets")
	ready := f.ready("ready")
	_, err = f.tr.Plan(ctx, f.project, ready)
	refusal(t, err, "TCK-3 is ready, not open")

	f.script.then(say("Stuck."))
	run := f.start(ready)
	f.next(ready, 0)
	if _, err := f.tickets.Update(f.project, ready, TicketPatch{Status: ptr(TicketOpen)}); err != nil {
		t.Fatal(err)
	}
	_, err = f.tr.Plan(ctx, f.project, ready)
	refusal(t, err, ready+" has a live run "+run.ID+"; stop it first")

	out, err := f.tickets.Create(f.project, NewTicket{Repo: "../elsewhere", Title: "outside"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.tr.Plan(ctx, f.project, out.ID)
	refusal(t, err, `"../elsewhere" is not a repository of this project`)
}

func TestTwoPlanClicksOpenOnePlanner(t *testing.T) {
	f := newFixture(t, t.TempDir())
	id := f.openTicket("goal")
	f.script.then(hold())
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() { _, errs[i] = f.tr.Plan(context.Background(), f.project, id) })
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("plans = %v, want exactly one to start", errs)
	}
	refused := errs[0]
	if refused == nil {
		refused = errs[1]
	}
	refusal(t, refused, "TCK-1 is planning, not open")
	if got, _ := f.tickets.Get(f.project, id); len(got.Runs) != 1 {
		t.Errorf("runs = %v, want one", got.Runs)
	}
	if n := len(f.runs.List()); n != 1 {
		t.Errorf("%d runs are left, want the planner only", n)
	}
}

func TestThePlannerPromptCarriesTheTicketRepositoriesDraftAndRule(t *testing.T) {
	got := planPrompt(Ticket{ID: "TCK-1", Title: "goal", Body: "Make it fast.",
		Comments: []Comment{{By: "you", Text: "Split it in two."}}},
		[]TicketView{{Ticket: Ticket{ID: "TCK-2", Repo: "api", Title: "Schema"}},
			{Ticket: Ticket{ID: "TCK-3", Title: "Handler", DependsOn: []string{"TCK-2"}}}},
		[]Repository{{Dir: "/p"}, {Name: "api", Dir: "/p/api"}})
	for _, want := range []string{
		"# TCK-1: goal", "Make it fast.", "you: Split it in two.", "## Repositories",
		`- "" (the project directory)`, "- api\n", "## Your draft", "- TCK-2: Schema (repo api)\n",
		"- TCK-3: Handler (waits for TCK-2)\n", planRule,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, got)
		}
	}
	if first := planPrompt(Ticket{ID: "TCK-1", Title: "goal"}, nil, nil); strings.Contains(first, "draft") {
		t.Errorf("a first plan has a draft section:\n%s", first)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/runner/ -run 'Planner|Plan' -count=1`
Expected: FAIL to build: `unknown field Planned in struct literal`, `f.tr.Plan undefined`.

- [ ] **Step 3: Report plans and decide planner turns**

In `internal/runner/ticketruns.go`:

In `TicketRunsConfig` add after `Finished`:

```go
	// Planned is told when a planner run's ticket reaches review, with the comment it wrote.
	Planned func(project string, v TicketView, comment string)
```

In `type TicketRuns struct` add `planned  func(string, TicketView, string)` after `finished`.
In `NewTicketRuns` set `planned: cfg.Planned,` in the literal and add after the `finished` nil
check:

```go
	if t.planned == nil {
		t.planned = func(string, TicketView, string) {}
	}
```

Replace the `switch ev.Kind { ... }` at the end of `onTurn` with:

```go
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
```

Replace `ticketPrompt` with:

```go
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
```

- [ ] **Step 4: Open the planner**

Create `internal/runner/ticketplan.go`:

```go
package runner

import (
	"context"
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
```

- [ ] **Step 5: Run the tests to verify they pass**

Run:

```bash
gofmt -w internal/runner/ticketplan.go internal/runner/ticketplan_test.go \
  internal/runner/ticketruns.go internal/runner/ticketruns_test.go
go test -race ./internal/runner/ -run 'Plan|Ticket|Worktree|Merge|Stop|Delet|Restart' -count=1
```

Expected: PASS, including every part 2 coordinator test.

- [ ] **Step 6: Commit**

```bash
git add internal/runner/ticketplan.go internal/runner/ticketplan_test.go \
  internal/runner/ticketruns.go internal/runner/ticketruns_test.go
git commit -m "feat(runner): a planner run splits a ticket and hands it to review"
```

---

### Task 6: Approve, Reject, Revise; Stop, delete and restart of a planner

**Files:**
- Modify: `internal/runner/ticketplan.go` (`Approve`, `Reject`, `Revise`, `endPlanner`,
  `reviseNote`)
- Modify: `internal/runner/ticketruns.go` (`detach`, `Recover`)
- Test: `internal/runner/ticketplan_test.go` (append)

**Interfaces:**
- Consumes: `Tickets.Approve`, `Reject`, `Revise` (Task 2); `openPlanner`, `review` (Task 5).
- Produces:
  ```go
  func (t *TicketRuns) Approve(project, id string) (TicketView, error)
  func (t *TicketRuns) Reject(project, id, reason string) (TicketView, error)
  func (t *TicketRuns) Revise(ctx context.Context, project, id, feedback string) (TicketView, error)
  ```
  Approve and Reject change the ticket first, then let go of the planner run and stop it.
  Revise needs `review`; it sends the feedback to the live last run, else opens a new planner
  run whose first message carries the draft and the discussion. `Stop`/`Remove` of a planner run
  in `planning` or `review` sends the ticket to `review` with the part 2 reasons; `Recover` sends
  every `planning` ticket whose last run is not live to `review` with `the daemon restarted`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/runner/ticketplan_test.go` (add `"time"` to its imports):

```go
// planned plans a ticket into one subticket "Schema" (TCK-2 when it is the first ticket) and
// waits for review.
func (f *fixture) planned(id string) RunView {
	f.t.Helper()
	f.script.then(call("create_subticket", `{"title":"Schema"}`), call("plan_done", `{"summary":"One step."}`),
		say("ok"))
	v := f.plan(id)
	f.next(id, 0)
	f.idle(v.ID)
	return v
}

func TestApproveMakesTheDraftReadyAndStopsThePlanner(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	id := f.openTicket("goal")
	v := f.planned(id)
	got, err := f.tr.Approve(f.project, id)
	if err != nil || got.Status != TicketReady {
		t.Fatalf("approve = %s, %v", got.Status, err)
	}
	if k, _ := f.tickets.Get(f.project, "TCK-2"); k.Status != TicketReady || !k.Runnable {
		t.Errorf("subticket = %s runnable %v, want ready and runnable", k.Status, k.Runnable)
	}
	if rv, _ := f.runs.Get(v.ID); rv.Live {
		t.Error("the planner run is still live after approval")
	}
	time.Sleep(200 * time.Millisecond)
	if again, _ := f.tickets.Get(f.project, id); len(again.Comments) != 1 || again.Status != TicketReady {
		t.Errorf("a late turn end changed the ticket: %s %q", again.Status, lastComment(again))
	}
	_, err = f.tr.Approve(f.project, id)
	refusal(t, err, "TCK-1 is ready, not in review")
}

func TestRejectDeletesTheDraftAndOpensTheTicket(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	id := f.openTicket("goal")
	v := f.planned(id)
	got, err := f.tr.Reject(f.project, id, "too big")
	if err != nil || got.Status != TicketOpen || got.Progress != nil || lastComment(got) != "Plan rejected: too big" {
		t.Fatalf("reject = %+v, %v", got, err)
	}
	if _, err := f.tickets.Get(f.project, "TCK-2"); !errors.Is(err, ErrNoTicket) {
		t.Errorf("the draft after reject = %v", err)
	}
	if rv, _ := f.runs.Get(v.ID); rv.Live {
		t.Error("the planner run is still live after rejection")
	}
	f.script.then(say("Again?"))
	f.plan(id)
	if again := f.next(id, 2); again.Status != TicketReview || len(again.Runs) != 2 {
		t.Errorf("a second plan = %s with runs %v", again.Status, again.Runs)
	}
}

func TestReviseSendsTheFeedbackToTheLivePlanner(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	id := f.openTicket("goal")
	f.planned(id)
	f.script.then(call("create_subticket", `{"title":"Handler","dependsOn":["TCK-2"]}`),
		call("plan_done", `{"summary":"Two steps."}`), say("ok"))
	if _, err := f.tr.Revise(context.Background(), f.project, id, "Add the handler."); err != nil {
		t.Fatal(err)
	}
	got := f.next(id, 2)
	if got.Status != TicketReview || lastComment(got) != "Two steps." || got.Progress == nil ||
		got.Progress.Total != 2 || len(got.Runs) != 1 {
		t.Fatalf("after revise = %s %q %+v runs %v", got.Status, lastComment(got), got.Progress, got.Runs)
	}
	if c := got.Comments[1]; c.By != "you" || c.Text != "Add the handler." {
		t.Errorf("feedback comment = %+v", c)
	}
	_, err := f.tr.Revise(context.Background(), f.project, f.openTicket("other"), "x")
	refusal(t, err, "is open, not in review")
}

func TestReviseOpensANewPlannerWhenTheOldOneIsGone(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	id := f.openTicket("goal")
	v := f.planned(id)
	if err := f.tr.Stop(v.ID); err != nil {
		t.Fatal(err)
	}
	f.script.then(call("plan_done", `{"summary":"Kept it."}`), say("ok"))
	if _, err := f.tr.Revise(context.Background(), f.project, id, "Keep it as is."); err != nil {
		t.Fatal(err)
	}
	got := f.next(id, 3)
	if got.Status != TicketReview || lastComment(got) != "Kept it." || len(got.Runs) != 2 {
		t.Fatalf("after revise = %s %q runs %v", got.Status, lastComment(got), got.Runs)
	}
	msg := f.firstMessage(got.Runs[1])
	for _, want := range []string{"## Your draft", "- TCK-2: Schema", "you: Keep it as is.", planRule} {
		if !strings.Contains(msg, want) {
			t.Errorf("the new planner's first message lacks %q:\n%s", want, msg)
		}
	}
}

func TestStopDeleteAndRestartLeaveThePlanInReview(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	stopped := f.openTicket("stopped")
	f.script.then(call("create_subticket", `{"title":"Schema"}`), hold())
	v := f.plan(stopped)
	waitUntil(t, func() bool {
		_, err := f.tickets.Get(f.project, "TCK-2")
		rv, _ := f.runs.Get(v.ID)
		return err == nil && rv.Running && f.script.left() == 0
	})
	if err := f.tr.Stop(v.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := f.tickets.Get(f.project, stopped)
	if got.Status != TicketReview || len(got.Comments) != 1 || lastComment(got) != "stopped by a person" ||
		got.Progress == nil || got.Progress.Total != 1 {
		t.Fatalf("stopped = %s %v %+v", got.Status, got.Comments, got.Progress)
	}
	time.Sleep(200 * time.Millisecond)
	if again, _ := f.tickets.Get(f.project, stopped); len(again.Comments) != 1 {
		t.Errorf("a late turn end wrote %q", lastComment(again))
	}

	deleted := f.openTicket("deleted")
	f.script.then(hold())
	v = f.plan(deleted)
	waitUntil(t, func() bool { rv, _ := f.runs.Get(v.ID); return rv.Running })
	if err := f.tr.Remove(v.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.tickets.Get(f.project, deleted); got.Status != TicketReview ||
		lastComment(got) != "the run was deleted" {
		t.Fatalf("deleted = %s %q", got.Status, lastComment(got))
	}

	restarted := f.openTicket("restarted")
	if _, err := f.tickets.StartPlan(f.project, restarted, "RUN-9"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tickets.create(f.project, NewTicket{Title: "kept", Parent: restarted}, "RUN-9"); err != nil {
		t.Fatal(err)
	}
	f.tr.Recover()
	got, _ = f.tickets.Get(f.project, restarted)
	if got.Status != TicketReview || lastComment(got) != "the daemon restarted" || got.Progress == nil ||
		got.Progress.Total != 1 {
		t.Fatalf("restarted = %s %q %+v", got.Status, lastComment(got), got.Progress)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```bash
go test ./internal/runner/ -run 'Approve|Reject|Revise|LeaveThePlan' -count=1
```

Expected: FAIL to build: `f.tr.Approve undefined`.

- [ ] **Step 3: Decide a plan**

Append to `internal/runner/ticketplan.go` (add `"errors"` to its imports):

```go
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
```

- [ ] **Step 4: Stop, delete and restart send a plan to review**

In `internal/runner/ticketruns.go`, in `detach` replace

```go
	v, err := t.tickets.Get(tr.project, tr.ticket)
	if err == nil && (v.Status == TicketRunning || v.Status == TicketBlocked) {
		t.finish(tr.project, tr.ticket, run, TicketBlocked, reason, v.MergePending)
	}
	return true
```

with

```go
	v, err := t.tickets.Get(tr.project, tr.ticket)
	switch {
	case err != nil:
	case tr.plan && (v.Status == TicketPlanning || v.Status == TicketReview):
		t.review(tr.project, tr.ticket, run, reason)
	case !tr.plan && (v.Status == TicketRunning || v.Status == TicketBlocked):
		t.finish(tr.project, tr.ticket, run, TicketBlocked, reason, v.MergePending)
	}
	return true
```

and replace `Recover` with:

```go
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
```

- [ ] **Step 5: Run the tests to verify they pass**

Run:

```bash
gofmt -w internal/runner/ticketplan.go internal/runner/ticketplan_test.go internal/runner/ticketruns.go
go test -race ./internal/runner/ -count=1
```

Expected: PASS apart from the known macOS failures and flakes.

- [ ] **Step 6: Commit**

```bash
git add internal/runner/ticketplan.go internal/runner/ticketplan_test.go internal/runner/ticketruns.go
git commit -m "feat(runner): approve, reject and revise a plan; stop, delete and restart keep it"
```

---

### Task 7: HTTP routes, daemon wiring and activity

**Files:**
- Modify: `internal/web/api_tickets.go` (seam methods, handlers)
- Modify: `internal/web/server.go` (routes)
- Modify: `cmd/aigem/webtickets.go` (adapter, `ticketPlanned`)
- Modify: `cmd/aigem/webcmd.go` (`Planned: backend.ticketPlanned`)
- Test: `internal/web/api_tickets_test.go`, `cmd/aigem/webtickets_test.go` (append)

**Interfaces:**
- Consumes: `TicketRuns.Plan`, `Approve`, `Reject`, `Revise`, `TicketRunsConfig.Planned`
  (Tasks 5, 6); `Tickets.StartPlan`, `PlanReview` (Task 1, in the cmd test).
- Produces:
  ```go
  // web.TicketsBackend gains:
  PlanTicket(ctx context.Context, project, id string) (Run, error)
  ApproveTicket(ctx context.Context, project, id string) (Ticket, error)
  RejectTicket(ctx context.Context, project, id, reason string) (Ticket, error)
  ReviseTicket(ctx context.Context, project, id, text string) (Ticket, error)
  ```
  Routes `POST .../plan` (201 run), `.../approve`, `.../reject` `{reason}`, `.../revise`
  `{text}` (200 ticket); other methods 405; a missing, blank or over-16-KiB reason or text, or
  an unknown field, is 400; refusals 409. Activity `ticket.planned`, `ticket.approved`,
  `ticket.rejected`.

- [ ] **Step 1: Write the failing tests**

In `internal/web/api_tickets_test.go` add a field `decided string` to `type ticketsBackend
struct` (after `patched TicketPatch`), and append:

```go
func (b *ticketsBackend) PlanTicket(_ context.Context, project, id string) (Run, error) {
	if id == "TCK-2" {
		return Run{}, Conflict("TCK-2 is a subticket; only a top-level ticket is planned")
	}
	return Run{ID: "RUN-6", Mode: "autonomous", TicketID: id, Status: "open", Live: true}, nil
}

func (b *ticketsBackend) ApproveTicket(_ context.Context, project, id string) (Ticket, error) {
	if id == "TCK-3" {
		return Ticket{}, Conflict("TCK-3 is done, not in review")
	}
	return Ticket{ID: id, Status: "ready"}, nil
}

func (b *ticketsBackend) RejectTicket(_ context.Context, project, id, reason string) (Ticket, error) {
	if id == "TCK-3" {
		return Ticket{}, Conflict("TCK-3 is done, not in review")
	}
	b.tmu.Lock()
	b.decided = reason
	b.tmu.Unlock()
	return Ticket{ID: id, Status: "open"}, nil
}

func (b *ticketsBackend) ReviseTicket(_ context.Context, project, id, text string) (Ticket, error) {
	b.tmu.Lock()
	b.decided = text
	b.tmu.Unlock()
	return Ticket{ID: id, Status: "planning"}, nil
}

func (b *ticketsBackend) lastDecided() string {
	b.tmu.Lock()
	defer b.tmu.Unlock()
	return b.decided
}

func TestThePlanRoutesAnswerAndRefuse(t *testing.T) {
	srv, b := newTicketsServer(t)
	base := "/api/projects/PRJ-1/tickets/"
	res := api(t, srv, http.MethodPost, base+"TCK-1/plan", "")
	if res.StatusCode != http.StatusCreated || decode[Run](t, res).TicketID != "TCK-1" {
		t.Errorf("plan = %d", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, base+"TCK-2/plan", "")
	if res.StatusCode != http.StatusConflict || !strings.Contains(readBody(t, res), "only a top-level ticket") {
		t.Errorf("a refused plan = %d, want 409 with the sentence", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, base+"TCK-1/approve", "")
	if res.StatusCode != http.StatusOK || decode[Ticket](t, res).Status != "ready" {
		t.Errorf("approve = %d", res.StatusCode)
	}
	if res := api(t, srv, http.MethodPost, base+"TCK-3/approve", ""); res.StatusCode != http.StatusConflict {
		t.Errorf("a refused approve = %d, want 409", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, base+"TCK-1/reject", `{"reason":"too big"}`)
	if res.StatusCode != http.StatusOK || b.lastDecided() != "too big" {
		t.Errorf("reject = %d, reason %q", res.StatusCode, b.lastDecided())
	}
	res = api(t, srv, http.MethodPost, base+"TCK-3/reject", `{"reason":"late"}`)
	if res.StatusCode != http.StatusConflict || !strings.Contains(readBody(t, res), "not in review") {
		t.Errorf("a refused reject = %d, want 409 with the sentence", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, base+"TCK-1/revise", `{"text":"split it"}`)
	if res.StatusCode != http.StatusOK || b.lastDecided() != "split it" {
		t.Errorf("revise = %d, text %q", res.StatusCode, b.lastDecided())
	}
	for _, c := range []struct{ path, body string }{
		{"TCK-1/reject", `{}`},
		{"TCK-1/revise", `{"text":"  "}`},
		{"TCK-1/reject", `{"reason":"x","why":"y"}`},
		{"TCK-1/revise", `{"text":"` + strings.Repeat("a", 16<<10+1) + `"}`},
	} {
		if res := api(t, srv, http.MethodPost, base+c.path, c.body); res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s %.40s = %d, want 400", c.path, c.body, res.StatusCode)
		}
	}
	for _, p := range []string{"plan", "approve", "reject", "revise"} {
		if res := api(t, srv, http.MethodGet, base+"TCK-1/"+p, ""); res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", p, res.StatusCode)
		}
	}
}
```

Append to `cmd/aigem/webtickets_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```bash
go test ./internal/web/ ./cmd/aigem/ -run 'PlanRoutes|PlansNeed' -count=1
```

Expected: FAIL to build: `b.PlanTicket undefined` / `unknown field Planned`.

- [ ] **Step 3: Routes and handlers**

In `internal/web/api_tickets.go` add to `TicketsBackend` after `MergeTicket`:

```go
	PlanTicket(ctx context.Context, project, id string) (Run, error)
	ApproveTicket(ctx context.Context, project, id string) (Ticket, error)
	RejectTicket(ctx context.Context, project, id, reason string) (Ticket, error)
	ReviseTicket(ctx context.Context, project, id, text string) (Ticket, error)
```

and append:

```go
func (s *Server) handlePlanTicket(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	run, err := b.PlanTicket(r.Context(), r.PathValue("id"), r.PathValue("tid"))
	if err != nil {
		writeRunError(w, "planning a ticket", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, run)
}

func (s *Server) handleApproveTicket(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	t, err := b.ApproveTicket(r.Context(), r.PathValue("id"), r.PathValue("tid"))
	if err != nil {
		writeRunError(w, "approving a plan", err)
		return
	}
	writeJSON(w, t)
}

func (s *Server) handleRejectTicket(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSONLimit(w, r, &req, maxCommentBytes+1<<10); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	switch {
	case strings.TrimSpace(req.Reason) == "":
		http.Error(w, "a reason is required", http.StatusBadRequest)
		return
	case len(req.Reason) > maxCommentBytes:
		http.Error(w, "a reason is at most 16 KiB", http.StatusBadRequest)
		return
	}
	t, err := b.RejectTicket(r.Context(), r.PathValue("id"), r.PathValue("tid"), req.Reason)
	if err != nil {
		writeRunError(w, "rejecting a plan", err)
		return
	}
	writeJSON(w, t)
}

func (s *Server) handleReviseTicket(w http.ResponseWriter, r *http.Request) {
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
	switch {
	case strings.TrimSpace(req.Text) == "":
		http.Error(w, "feedback is required", http.StatusBadRequest)
		return
	case len(req.Text) > maxCommentBytes:
		http.Error(w, "feedback is at most 16 KiB", http.StatusBadRequest)
		return
	}
	t, err := b.ReviseTicket(r.Context(), r.PathValue("id"), r.PathValue("tid"), req.Text)
	if err != nil {
		writeRunError(w, "revising a plan", err)
		return
	}
	writeJSON(w, t)
}
```

In `internal/web/server.go`, after the two `.../merge` lines, add:

```go
	s.api("POST /api/projects/{id}/tickets/{tid}/plan", s.handlePlanTicket)
	s.mux.HandleFunc("/api/projects/{id}/tickets/{tid}/plan", methodNotAllowed("POST"))
	s.api("POST /api/projects/{id}/tickets/{tid}/approve", s.handleApproveTicket)
	s.mux.HandleFunc("/api/projects/{id}/tickets/{tid}/approve", methodNotAllowed("POST"))
	s.api("POST /api/projects/{id}/tickets/{tid}/reject", s.handleRejectTicket)
	s.mux.HandleFunc("/api/projects/{id}/tickets/{tid}/reject", methodNotAllowed("POST"))
	s.api("POST /api/projects/{id}/tickets/{tid}/revise", s.handleReviseTicket)
	s.mux.HandleFunc("/api/projects/{id}/tickets/{tid}/revise", methodNotAllowed("POST"))
```

- [ ] **Step 4: The adapter, the activity and the wiring**

Append to `cmd/aigem/webtickets.go`:

```go
func (b *webBackend) PlanTicket(ctx context.Context, project, id string) (web.Run, error) {
	if err := b.ticketRunsReady(project); err != nil {
		return web.Run{}, err
	}
	v, err := b.ticketRuns.Plan(ctx, project, id)
	if err != nil {
		return web.Run{}, ticketRunError(err)
	}
	return webRun(v), nil
}

func (b *webBackend) ApproveTicket(_ context.Context, project, id string) (web.Ticket, error) {
	if err := b.ticketRunsReady(project); err != nil {
		return web.Ticket{}, err
	}
	v, err := b.ticketRuns.Approve(project, id)
	if err != nil {
		return web.Ticket{}, ticketRunError(err)
	}
	b.recordActivity(web.Activity{Kind: "ticket.approved", Text: "Approved the plan of " + v.ID + ": " + v.Title})
	return webTicket(v), nil
}

func (b *webBackend) RejectTicket(_ context.Context, project, id, reason string) (web.Ticket, error) {
	if err := b.ticketRunsReady(project); err != nil {
		return web.Ticket{}, err
	}
	v, err := b.ticketRuns.Reject(project, id, reason)
	if err != nil {
		return web.Ticket{}, ticketRunError(err)
	}
	b.recordActivity(web.Activity{
		Kind: "ticket.rejected", Text: "Rejected the plan of " + v.ID + ": " + firstLine(reason),
	})
	return webTicket(v), nil
}

func (b *webBackend) ReviseTicket(ctx context.Context, project, id, text string) (web.Ticket, error) {
	if err := b.ticketRunsReady(project); err != nil {
		return web.Ticket{}, err
	}
	v, err := b.ticketRuns.Revise(ctx, project, id, text)
	if err != nil {
		return web.Ticket{}, ticketRunError(err)
	}
	return webTicket(v), nil
}

// ticketPlanned records a plan that reached review in the activity feed.
func (b *webBackend) ticketPlanned(_ string, v runner.TicketView, comment string) {
	a := web.Activity{Kind: "ticket.planned", Text: "Plan of " + v.ID + " in review: " + firstLine(comment)}
	if n := len(v.Runs); n > 0 {
		a.RunRef = v.Runs[n-1]
	}
	b.recordActivity(a)
}
```

In `cmd/aigem/webcmd.go`, change the `runner.TicketRunsConfig{...}` literal to:

```go
		backend.ticketRuns = runner.NewTicketRuns(runner.TicketRunsConfig{
			Runs: runs, Tickets: tickets, Projects: projects,
			Finished: backend.ticketFinished, Planned: backend.ticketPlanned,
		})
```

- [ ] **Step 5: Run the tests to verify they pass**

Run:

```bash
gofmt -w internal/web/api_tickets.go internal/web/server.go internal/web/api_tickets_test.go \
  cmd/aigem/webtickets.go cmd/aigem/webcmd.go cmd/aigem/webtickets_test.go
go build ./... && go test -race ./internal/web/ ./cmd/aigem/ -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/web/api_tickets.go internal/web/server.go internal/web/api_tickets_test.go \
  cmd/aigem/webtickets.go cmd/aigem/webcmd.go cmd/aigem/webtickets_test.go
git commit -m "feat(web): plan, approve, reject and revise routes"
```

---

### Task 8: UI - Plan, Approve, Reject and Revise on the ticket page

**Files:**
- Modify: `internal/web/_ui/src/lib/api.ts`
- Modify: `internal/web/_ui/src/screens/Task.tsx`
- Test: `internal/web/_ui/src/screens/tickets.test.tsx` (append)

**Interfaces:**
- Consumes: the routes of Task 7.
- Produces: `api.planTicket(project, id)` -> `Run`, `api.approveTicket(project, id)`,
  `api.rejectTicket(project, id, reason)`, `api.reviseTicket(project, id, text)` -> `Ticket`.
  Ticket page: no status moves on a draft subticket (its parent is `planning` or `review`);
  "Plan" on a top-level `open` ticket without subtickets; "Stop" whenever the
  last run is live (also on a parent); in `review` "Approve", "Reject" and "Revise"; Reject and
  Revise toggle a box (`Reason` / `Feedback` textarea, "Reject plan" / "Send feedback" and
  "Cancel", Ctrl/Cmd+Enter sends, Escape cancels); refusals in the page's alert.

- [ ] **Step 1: Write the failing tests**

Append to `internal/web/_ui/src/screens/tickets.test.tsx`:

```ts
const RUN6: Run = { ...RUN5, id: 'RUN-6', title: 'Plan TCK-6: Big goal', running: false, ticketId: 'TCK-6' }
const REVIEW = [
  ticket('TCK-6', { title: 'Big goal', status: 'review', runs: ['RUN-6'], progress: { done: 0, total: 2 } }),
  ticket('TCK-7', { title: 'Schema', parent: 'TCK-6', by: 'run RUN-6' }),
  ticket('TCK-8', { title: 'Handler', parent: 'TCK-6', by: 'run RUN-6', dependsOn: ['TCK-7'] }),
]

test('Plan is offered on an open top-level ticket without subtickets and starts a planner', async () => {
  const h = await openTask('TCK-6', [...PLAN, ticket('TCK-6', { title: 'Big goal' })], {
    'POST /api/projects/PRJ-1/tickets/TCK-6/plan': () => new Response(JSON.stringify(RUN6), { status: 201 }),
  })
  await userEvent.click(screen.getByRole('button', { name: 'Plan' }))
  await waitFor(() =>
    expect(h.sent.some((s) => s.method === 'POST' && s.path === '/api/projects/PRJ-1/tickets/TCK-6/plan')).toBe(true),
  )
  act(() => navigate({ screen: 'task', id: 'TCK-1' }))
  await screen.findByRole('heading', { level: 1, name: 'Delete sessions' })
  expect(screen.queryByRole('button', { name: 'Plan' })).not.toBeInTheDocument()
  act(() => navigate({ screen: 'task', id: 'TCK-3' }))
  await screen.findByRole('heading', { level: 1, name: 'HTTP endpoint' })
  expect(screen.queryByRole('button', { name: 'Plan' })).not.toBeInTheDocument()
})

test('a plan in review is approved, or rejected with a reason', async () => {
  const h = await openTask(
    'TCK-6',
    REVIEW,
    {
      'POST /api/projects/PRJ-1/tickets/TCK-6/approve': () =>
        new Response("TCK-6's plan has no subtickets; revise or reject it", { status: 409 }),
      'POST /api/projects/PRJ-1/tickets/TCK-6/reject': () => json(ticket('TCK-6', { title: 'Big goal' })),
    },
    [RUN6],
  )
  expect(screen.getByRole('button', { name: 'Stop' })).toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'Approve' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('has no subtickets')
  await userEvent.click(screen.getByRole('button', { name: 'Reject' }))
  expect(screen.getByRole('button', { name: 'Reject plan' })).toBeDisabled()
  await userEvent.type(screen.getByLabelText('Reason'), 'too big')
  await userEvent.click(screen.getByRole('button', { name: 'Reject plan' }))
  await waitFor(() => expect(screen.queryByLabelText('Reason')).not.toBeInTheDocument())
  const sent = h.sent.find((s) => s.path === '/api/projects/PRJ-1/tickets/TCK-6/reject')
  expect(JSON.parse(sent!.body)).toEqual({ reason: 'too big' })
})

test('Revise sends the feedback from the keyboard, and Escape closes the box', async () => {
  const h = await openTask(
    'TCK-6',
    REVIEW,
    { 'POST /api/projects/PRJ-1/tickets/TCK-6/revise': () => json(REVIEW[0]) },
    [RUN6],
  )
  await userEvent.click(screen.getByRole('button', { name: 'Reject' }))
  await userEvent.type(screen.getByLabelText('Reason'), '{Escape}')
  expect(screen.queryByLabelText('Reason')).not.toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'Revise' }))
  await userEvent.type(screen.getByLabelText('Feedback'), 'Add a migration.')
  await userEvent.keyboard('{Control>}{Enter}{/Control}')
  await waitFor(() => expect(h.sent.some((s) => s.path === '/api/projects/PRJ-1/tickets/TCK-6/revise')).toBe(true))
  const sent = h.sent.find((s) => s.path === '/api/projects/PRJ-1/tickets/TCK-6/revise')
  expect(JSON.parse(sent!.body)).toEqual({ text: 'Add a migration.' })
})

test('a draft subticket page offers no status moves', async () => {
  await openTask('TCK-7', REVIEW, {}, [RUN6])
  expect(screen.queryByRole('button', { name: 'Mark ready' })).not.toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'Close' })).not.toBeInTheDocument()
})

test('a ticket being planned offers Stop but no decisions', async () => {
  const planning = REVIEW.map((t) => (t.id === 'TCK-6' ? { ...t, status: 'planning' as const } : t))
  await openTask('TCK-6', planning, {}, [{ ...RUN6, running: true }])
  expect(screen.getByRole('button', { name: 'Stop' })).toBeInTheDocument()
  for (const name of ['Approve', 'Reject', 'Revise', 'Plan']) {
    expect(screen.queryByRole('button', { name })).not.toBeInTheDocument()
  }
})
```

- [ ] **Step 2: Run the tests to verify they fail**

Run in `internal/web/_ui`:

```bash
NODE_OPTIONS=--no-experimental-webstorage npx vitest run src/screens/tickets.test.tsx
```

Expected: FAIL: `Unable to find an accessible element with the role "button" and name "Plan"`.

- [ ] **Step 3: The calls**

In `internal/web/_ui/src/lib/api.ts` add after `mergeTicket`:

```ts
  planTicket: (project: string, id: string, signal?: AbortSignal) =>
    json<Run>(`/api/projects/${encodeURIComponent(project)}/tickets/${encodeURIComponent(id)}/plan`, {
      method: 'POST',
      signal,
    }),
  approveTicket: (project: string, id: string, signal?: AbortSignal) =>
    json<Ticket>(`/api/projects/${encodeURIComponent(project)}/tickets/${encodeURIComponent(id)}/approve`, {
      method: 'POST',
      signal,
    }),
  rejectTicket: (project: string, id: string, reason: string, signal?: AbortSignal) =>
    json<Ticket>(`/api/projects/${encodeURIComponent(project)}/tickets/${encodeURIComponent(id)}/reject`, {
      ...body({ reason }),
      signal,
    }),
  reviseTicket: (project: string, id: string, text: string, signal?: AbortSignal) =>
    json<Ticket>(`/api/projects/${encodeURIComponent(project)}/tickets/${encodeURIComponent(id)}/revise`, {
      ...body({ text }),
      signal,
    }),
```

- [ ] **Step 4: The buttons and the box**

In `internal/web/_ui/src/screens/Task.tsx`:

After `const [busy, setBusy] = useState(false)` add:

```tsx
  const [asking, setAsking] = useState<'' | 'reject' | 'revise'>('')
```

After `const parent = tickets.find((x) => x.id === t.parent)` add (a draft has no moves on
the server, so the page offers none):

```tsx
  const draft = parent?.status === 'planning' || parent?.status === 'review'
```

Replace the whole `{!isParent && ( <div className="ml-auto flex gap-1.5"> ... </div> )}` block
in the header with:

```tsx
          <div className="ml-auto flex gap-1.5">
            {!t.parent && !isParent && t.status === 'open' && (
              <button
                type="button"
                disabled={busy}
                onClick={() => void act(() => api.planTicket(project, t.id))}
                className={BUTTON}
              >
                Plan
              </button>
            )}
            {t.runnable && (
              <button
                type="button"
                disabled={busy}
                onClick={() => void act(() => api.runTicket(project, t.id))}
                className={BUTTON}
              >
                Run
              </button>
            )}
            {lastRun && lastLive && (
              <button
                type="button"
                disabled={busy}
                onClick={() => void act(() => api.stopRun(lastRun))}
                className={BUTTON}
              >
                Stop
              </button>
            )}
            {t.status === 'review' && (
              <>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void act(() => api.approveTicket(project, t.id))}
                  className={BUTTON}
                >
                  Approve
                </button>
                {(['reject', 'revise'] as const).map((k) => (
                  <button
                    key={k}
                    type="button"
                    aria-expanded={asking === k}
                    onClick={() => setAsking(asking === k ? '' : k)}
                    className={BUTTON}
                  >
                    {k === 'reject' ? 'Reject' : 'Revise'}
                  </button>
                ))}
              </>
            )}
            {!isParent && t.status === 'blocked' && t.mergePending && (
              <button
                type="button"
                disabled={busy}
                onClick={() => void act(() => api.mergeTicket(project, t.id))}
                className={BUTTON}
              >
                Retry merge
              </button>
            )}
            {!isParent &&
              !draft &&
              personMoves(t.status).map((to) => (
                <button key={to} type="button" onClick={() => void change({ status: to })} className={BUTTON}>
                  {MOVE_LABEL[to]}
                </button>
              ))}
          </div>
```

After the `{error && ( <p role="alert" ...> )}` block in the header add:

```tsx
        {asking && t.status === 'review' && (
          <PlanAnswer
            key={asking}
            kind={asking}
            busy={busy}
            onCancel={() => setAsking('')}
            onSend={(text) =>
              void act(async () => {
                await (asking === 'reject'
                  ? api.rejectTicket(project, t.id, text)
                  : api.reviseTicket(project, t.id, text))
                setAsking('')
              })
            }
          />
        )}
```

Add after the `Section` component:

```tsx
function PlanAnswer({
  kind,
  busy,
  onSend,
  onCancel,
}: {
  kind: 'reject' | 'revise'
  busy: boolean
  onSend: (text: string) => void
  onCancel: () => void
}) {
  const [text, setText] = useState('')
  const send = () => {
    if (text.trim() && !busy) onSend(text)
  }
  return (
    <div className="mt-2 flex gap-2">
      <textarea
        aria-label={kind === 'reject' ? 'Reason' : 'Feedback'}
        value={text}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) send()
          if (e.key === 'Escape') onCancel()
        }}
        rows={2}
        placeholder={
          kind === 'reject' ? 'Why is this plan wrong? ⌘↵ to send' : 'What should the planner change? ⌘↵ to send'
        }
        className={`flex-1 ${INPUT}`}
      />
      <div className="flex flex-col gap-1.5">
        <button type="button" disabled={!text.trim() || busy} onClick={send} className={BUTTON}>
          {kind === 'reject' ? 'Reject plan' : 'Send feedback'}
        </button>
        <button type="button" onClick={onCancel} className={BUTTON}>
          Cancel
        </button>
      </div>
    </div>
  )
}
```

- [ ] **Step 5: Run the tests and checks**

Run in `internal/web/_ui`:

```bash
npm run lint && npm run check && NODE_OPTIONS=--no-experimental-webstorage npx vitest run
```

Expected: PASS, including the part 2 tests for Run, Stop and Retry merge.

- [ ] **Step 6: Commit**

```bash
git add internal/web/_ui/src/lib/api.ts internal/web/_ui/src/screens/Task.tsx \
  internal/web/_ui/src/screens/tickets.test.tsx
git commit -m "feat(web): plan, approve, reject and revise on the ticket page"
```

---

### Task 9: UI - the draft badge and the "Needs you" filter

**Files:**
- Modify: `internal/web/_ui/src/screens/Task.tsx` (draft badge, "Add subticket" in planning)
- Modify: `internal/web/_ui/src/state/tickets.ts`, `src/screens/Tickets.tsx` (filter)
- Test: `internal/web/_ui/src/screens/tickets.test.tsx`, `src/state/tickets.test.ts` (append)

**Interfaces:**
- Consumes: `REVIEW`, `RUN6` test fixtures (Task 8).
- Produces: `TicketFilter = 'active' | 'ready' | 'needs' | 'all'`; `needs` shows `blocked` and
  `review`. On the ticket page, while the ticket is `planning` or `review`, each subticket row
  of the Overview list carries a "Draft" badge; "Add subticket" is hidden in `planning`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/web/_ui/src/screens/tickets.test.tsx`:

```ts
test('the subtickets of a plan in progress or in review are drafts', async () => {
  await openTask('TCK-6', [...PLAN, ...REVIEW], {}, [RUN6])
  expect(screen.getAllByText('Draft')).toHaveLength(2)
  expect(screen.getByRole('button', { name: 'Add subticket' })).toBeInTheDocument()
  act(() => navigate({ screen: 'task', id: 'TCK-1' }))
  await screen.findByRole('heading', { level: 1, name: 'Delete sessions' })
  expect(screen.queryByText('Draft')).not.toBeInTheDocument()
})

test('a ticket being planned cannot get a subticket by hand', async () => {
  const planning = REVIEW.map((t) => (t.id === 'TCK-6' ? { ...t, status: 'planning' as const } : t))
  await openTask('TCK-6', planning, {}, [RUN6])
  expect(screen.getAllByText('Draft')).toHaveLength(2)
  expect(screen.queryByRole('button', { name: 'Add subticket' })).not.toBeInTheDocument()
})

test('Needs you lists the blocked tickets and the plans in review', async () => {
  await openTickets([...PLAN, ticket('TCK-9', { title: 'Stuck one', status: 'blocked' }), ...REVIEW])
  await userEvent.click(screen.getByRole('radio', { name: 'Needs you' }))
  expect(screen.getByText('Stuck one')).toBeInTheDocument()
  expect(screen.getByText('Big goal')).toBeInTheDocument()
  expect(screen.queryByText('Runner change')).not.toBeInTheDocument()
  expect(screen.queryByText('Schema')).not.toBeInTheDocument()
})
```

Append to `internal/web/_ui/src/state/tickets.test.ts`:

```ts
test('needs you holds the blocked tickets and the plans in review', () => {
  const more = [...all, t('TCK-6', 'blocked'), t('TCK-7', 'review')]
  expect(treeRows(more, 'needs', '', new Set()).map((r) => r.ticket.id)).toEqual(['TCK-6', 'TCK-7'])
})
```

- [ ] **Step 2: Run the tests to verify they fail**

Run in `internal/web/_ui`:

```bash
NODE_OPTIONS=--no-experimental-webstorage npx vitest run src/screens/tickets.test.tsx src/state/tickets.test.ts
```

Expected: FAIL: no "Draft" text, no radio "Needs you", and a type error on `'needs'`.

- [ ] **Step 3: The badge**

In `internal/web/_ui/src/screens/Task.tsx`, in the Overview subticket row (the
`kids.map((k) => ( <button ...> ... </button> ))`), add before `<TicketStatusLabel ticket={k} />`:

```tsx
                      {(t.status === 'planning' || t.status === 'review') && (
                        <span className="rounded-[0.1875rem] border border-line-strong px-1 text-[0.6875rem] text-fg-subtle">
                          Draft
                        </span>
                      )}
```

In the same Overview list header, wrap the "Add subticket" button so a person cannot add one
while the ticket is being planned:

```tsx
                    {t.status !== 'planning' && (
                      <button type="button" onClick={() => setAdding(true)} className={`ml-auto ${BUTTON}`}>
                        Add subticket
                      </button>
                    )}
```

- [ ] **Step 4: The filter**

In `internal/web/_ui/src/state/tickets.ts` change the type and the `shown` line:

```ts
export type TicketFilter = 'active' | 'ready' | 'needs' | 'all'
```

```ts
  if (filter === 'needs') return t.status === 'blocked' || t.status === 'review'
```

(the second replaces `if (filter === 'blocked') return t.status === 'blocked'`).

In `internal/web/_ui/src/screens/Tickets.tsx` replace the segment
`{ value: 'blocked', label: 'Blocked' },` with:

```tsx
                { value: 'needs', label: 'Needs you' },
```

- [ ] **Step 5: Run the tests and checks**

Run in `internal/web/_ui`:

```bash
npm run lint && npm run check && NODE_OPTIONS=--no-experimental-webstorage npx vitest run
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/web/_ui/src/screens/Task.tsx internal/web/_ui/src/state/tickets.ts \
  internal/web/_ui/src/screens/Tickets.tsx internal/web/_ui/src/screens/tickets.test.tsx \
  internal/web/_ui/src/state/tickets.test.ts
git commit -m "feat(web): draft badge and the Needs you filter"
```

---

### Task 10: Docs, full check, manual check

**Files:**
- Modify: `docs/web.md` (routes table, a "Planner" section, the screens paragraph)
- Modify: `CHANGELOG.md` (one entry under Unreleased/Added)

- [ ] **Step 1: Document**

In `docs/web.md`, in the routes table after the
`POST /api/projects/{id}/tickets/{tid}/merge` row add:

```markdown
| `POST /api/projects/{id}/tickets/{tid}/plan` | starts a planner run on an open top-level ticket; 201 with the run, 409 |
| `POST /api/projects/{id}/tickets/{tid}/approve` | approves the plan in review; 200 with the ticket, 409 |
| `POST /api/projects/{id}/tickets/{tid}/reject` | `{reason}`; deletes the draft, back to `open`; 200 with the ticket, 409 |
| `POST /api/projects/{id}/tickets/{tid}/revise` | `{text}`; sends the feedback to the planner; 200 with the ticket, 409 |
```

After the "Runs on tickets" section add:

```markdown
## Planner

"Plan" on a top-level `open` ticket without subtickets moves it to `planning` and opens a
planner run: an autonomous run with the `read-only` capability profile (read, list and search
tools; no writes, no shell) rooted at the ticket's repository main checkout. Its first message
is the ticket, the discussion, the project's repositories and the rule: split the ticket into
subtickets that each fit one autonomous run, link them with dependencies, and call `plan_done`
with a short summary. Its ticket tools are `list_tickets`, `get_ticket`, `create_subticket`,
`delete_subticket`, `set_dependencies` and `plan_done`; they act only while the ticket is
`planning` under that run, and write only its subtickets.

The plan is a draft of ordinary `open` subtickets (`by: run RUN-n`). While the parent is
`planning` or `review` its status is not derived from them, a draft has no status moves, and
no ticket outside the plan may wait for one. In `planning` a person cannot add, delete or
relink the parent's subtickets; in `review` they can.

At the end of every planner turn the ticket goes to `review` with the summary of `plan_done`,
else the agent's last message, or "interrupted" / "the turn ended with an error: ...". Then a
person decides:

- "Approve": every subticket becomes `ready`, the planner run is stopped and the parent
  follows its subtickets again. Refused while the plan has no subticket.
- "Reject" with a reason: the subtickets are deleted, the run is stopped and the ticket is
  `open` again with the comment "Plan rejected: <reason>".
- "Revise" with feedback: the feedback is a comment, the ticket goes back to `planning` and
  the feedback goes to the planner run; when that run is gone, a new one starts with the draft
  and the discussion.

Stopping the planner run moves the ticket to `review` with "stopped by a person", deleting it
with "the run was deleted", and a daemon restart with "the daemon restarted"; the draft stays.
Typing into a live planner run moves the ticket back to `planning`. Activity:
`ticket.planned`, `ticket.approved` and `ticket.rejected`. The Tickets screen's "Needs you"
filter lists `blocked` tickets and plans in `review`.
```

In the screens paragraph near the top replace "A runnable ticket can be run; the worktrees
screen lists the branches runs left behind." with "An open ticket can be planned and a
runnable one run; the worktrees screen lists the branches runs left behind." The old sentence
spans two lines; reflow that paragraph to at most 100 characters per line.

In `CHANGELOG.md` under `## [Unreleased]` / `### Added`, above the part 2 entry, add:

```markdown
- Web UI: the planner (part 3 of agent tickets). "Plan" opens a read-only run that splits a
  ticket into draft subtickets; a person approves, rejects or revises the plan. The Tickets
  list's "Needs you" filter shows blocked tickets and plans in review.
```

Max 100 characters per prose line; table rows may be longer, as the existing rows are.

- [ ] **Step 2: Full check**

```bash
go build ./... && go test -race ./internal/... ./cmd/aigem/... 2>&1 | grep -v '^ok' | head -20
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./...
cd internal/web/_ui && npm run lint && npm run check && \
  NODE_OPTIONS=--no-experimental-webstorage npx vitest run && cd -
make web && make build
```

Expected: only the known macOS failures and flakes, and the old QF1003.

- [ ] **Step 3: Manual check with a real model**

Start `bin/aigem web --addr 127.0.0.1:7799` (stop an older one on that port first). Add a
throwaway git repository with a `main` branch and a little code as a project, and
`.aigem/project.json` `{"check": "true"}` committed. Create a ticket that needs two or three
steps and press "Plan": the ticket shows `planning`, the run screen shows only read tools and
the ticket tools, and the ticket reaches `review` with a summary and "Draft" subtickets. Press
"Revise", ask for a change, send: the same run continues and the ticket comes back to
`review`. Press "Stop", then "Revise" again: a second planner run starts with the draft. Press
"Approve": the subtickets become `ready`, the parent `ready`, the planner run closed. Press
"Run" on the first runnable subticket and watch it reach `done`. Check "Needs you" on the
Tickets screen and the three activity kinds.

- [ ] **Step 4: Commit**

```bash
git add docs/web.md CHANGELOG.md
git commit -m "docs: the planner"
```
