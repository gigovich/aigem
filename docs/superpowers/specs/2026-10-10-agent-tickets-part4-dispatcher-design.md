# Agent Tickets Part 4: Dispatcher - Design

Date: 2026-10-10. Status: approved in conversation; written for review.

Part 4 of `2026-10-09-agent-tickets-design.md`. Builds on part 1 (tickets), part 2 (runs on
tickets) and part 3 (planner).

## What this is

The dispatcher presses "Run" by itself. Each project has a number of slots; while fewer tickets
are `running` than there are slots, the daemon starts the oldest runnable ticket with part 2's
`TicketRuns.Start`. A person sets the slots, can pause the queue, and handles what is blocked.

Decisions taken with the user on 2026-10-10:

- **Slots and Pause per project.** `slots` is 0 to 8, default 0 (off: nothing starts by itself
  until a person opts in). Pause stops new starts and keeps the number; it does not stop runs that
  are already working.
- **A slot is a `running` ticket**, whoever started it (the dispatcher or a person with "Run").
  Blocked tickets and planner runs do not take a slot, so one stuck ticket does not stop the
  queue. The daemon's limit of 32 live runs still applies.
- The dispatcher never plans. "Plan" stays a person's action; approving a plan is the human gate.

## Rules

- **Settings.** `Project` gains `Slots int` and `Paused bool`, saved in `projects.json`.
  `Projects.SetDispatch(id, slots, paused)` refuses a slot count outside 0-8
  (`slots must be between 0 and 8`).
- **A pass.** For every project with `Slots > 0` and not `Paused`:
  1. count its tickets with status `running`;
  2. while the count is below `Slots`, take the runnable tickets (part 1's `runnable`: `ready`,
     not a parent, every dependency `done`) oldest first by ticket number, skip any already being
     started, and call `TicketRuns.Start`;
  3. each successful start counts as one more `running` ticket.
- **When a pass runs.** After any ticket change, after a project change (settings, add, remove),
  after a ticket run's outcome is recorded, and every 30 seconds as a safety net. Wake-ups that
  arrive during a pass cause one more pass, never more.
- **A failed start.**
  - A `*TicketRefusal`, the ticket's own fault (`<path> is in the way of the worktree for <id>;
    remove it`, not a git checkout, the worktree could not be added, a bad repository): the
    ticket moves `ready` -> `blocked` with the comment `the dispatcher could not start it:
    <error>` by `aigem`, so the same broken ticket is not retried in a loop. A refusal because
    the ticket is no longer runnable (it changed between the read and the start) or because a
    person's "Run" is starting it is ignored.
  - Any other error (`ErrTooManyRuns`, a project env that does not load, a model or login
    error, a `Runs.Create` error): nothing is blocked. The dispatcher logs it and holds: the
    pass ends and wake-ups are ignored until the next 30 second tick. Without the hold an env
    load failure, which notifies the project and so wakes the dispatcher, would loop, and every
    start would run git before it meets the run limit.
  - `ErrRunsClosed` or `Close`: the dispatcher stops; a start that `Close` cancelled never
    blocks its ticket.
- **Pause.** No new starts; running tickets go on. Resume (or a higher slot count) runs a pass.
  Lowering the slots below the running count stops nothing; the queue just waits.
- **Restart.** Part 2's `Recover` blocks the tickets that were running (`the daemon restarted`);
  the first pass after it starts the next ready tickets.

## Components

- `runner.Projects`: the two fields, `SetDispatch`, and both in `ProjectView` and its notify.
- `runner.Tickets`: `Block(project, id, reason string) (TicketView, error)`, only from `ready`
  (else `<id> is <status>, not ready`), inside the registry lock.
- `runner.Dispatcher` (`internal/runner/dispatcher.go`): `NewDispatcher(cfg)` with `Projects`,
  `Tickets`, `TicketRuns` and a `Started func(project string, v TicketView, run string)`
  callback; `Wake()` (never blocks), `Close()` (cancels the start in flight and waits). One
  goroutine per daemon; its only state is the hold flag. A ticket it blocks is reported
  through `TicketRuns`' `Finished` callback, like a run's outcome.
- `cmd/aigem`: builds the dispatcher after `TicketRuns.Recover`; the ticket notify and the
  project notify call `Wake()` (every run outcome goes through a ticket change);
  `Close()` runs before `TicketRuns.Close`. `Started` writes the activity.

## API

| Route | Does |
| --- | --- |
| `PATCH /api/projects/{id}` | `{slots?, paused?}`; 200 with the project, 400 out of range, 404 |

`GET /api/projects` returns `slots` and `paused` with every project. Activity:
`ticket.started` (`Started TCK-n: <title>`), written only for the dispatcher's starts; a failed
start shows as the existing `ticket.blocked`.

## UI

- Tickets screen header, for the selected project: a "Slots" select (Off, 1-8), a Pause / Resume
  button, and "n of N running"; a "Paused" badge while paused. Errors show in the page's alert.
- The daemon's own directory (no project id) shows no dispatcher controls.
- `docs/web.md`: the route row and a "Dispatcher" section; `CHANGELOG.md`: one line.

## Testing

- `runner`: `SetDispatch` range and persistence; `Block`; the dispatcher fills slots oldest
  first, respects dependencies, counts a person's "Run", ignores blocked tickets and planners,
  stops on Pause and fills on Resume, blocks a broken ticket once, holds on `ErrTooManyRuns`
  and on any other error that is not a refusal (blocks nothing, ignores wake-ups until the
  tick), leaves a ticket a person is starting, never blocks after `Close`, and stops on
  `Close`. Passes run synchronously in the tests; one test runs the goroutine. Real git
  repositories and the scripted model, as in part 2.
- `web`, `cmd/aigem`: the PATCH route (200, 400, 404, 405), the wiring, `ticket.started`.
- UI: the select, Pause / Resume, the count, the badge.
- A manual check with a real model: slots 2, three ready tickets, pause and resume.

## Out of scope

Priorities and ordering other than oldest first, slots per repository, planning by the
dispatcher, a global slot count, stopping runs when the slots are lowered.
