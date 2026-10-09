# Agent Tickets - Design

Date: 2026-10-09. Status: draft for review.

Replaces sections B (Tickets) and C (Runs on tickets) of
`2026-09-13-projects-phase-two-design.md`. Section A (Projects) of that spec is built and stays.

## What this is

A ticket system where agents are first-class: an agent splits a goal into subtickets with
dependencies, a person approves that plan, and then agents pick up ready tickets by themselves
and do the work. The person sets goals, approves plans and handles what is blocked.

Decisions taken with the user on 2026-10-09:

- Agents **pick up work themselves** and **plan**: split a ticket into subtickets and link them
  with dependencies.
- The one human gate is **approving the plan**. After that agents run, commit, check and merge
  on their own (as in the 2026-09-13 section C). Anything that fails becomes `blocked` and waits
  for a person.
- **N slots per project**: the daemon keeps up to N autonomous runs per project and fills a free
  slot with a runnable ticket. The queue can be paused.
- Tickets stay local to the daemon: no tracker sync, nothing is pushed.
- Agents have no names of their own and people have no assignees. An author is `you` or
  `run RUN-n`.
- Approach: extend the 2026-09-13 design (a registry in `runner`, HTTP in `web`, agent tools on
  top of the same registry). Not chosen: tickets as files in the repository (conflicts between
  parallel worktrees), one long-running manager agent (costly, hard to test and to explain).

## Sub-projects

Each has its own spec section, plan and review cycle, and ships on its own.

| Part | Delivers | Needs |
| --- | --- | --- |
| 1. Tickets | tickets, subtickets, dependencies, statuses, API, Tickets and ticket screens | - |
| 2. Runs | a run in a worktree per ticket, commit, check, merge, Runs tab, Worktrees screen | 1 |
| 3. Planner | a run that splits a ticket (`planning` -> `review`), ticket tools, approval | 1 |
| 4. Dispatcher | N slots per project, picks runnable tickets, pause/resume | 2, 3 |

This document designs part 1 in full. Parts 2-4 get their own sections later; the statuses and
fields they need are defined now so the contract does not change under them.

---

## Part 1. Tickets

### Model

```go
type Ticket struct {
    ID        string    // "TCK-1", per project, never reused
    Repo      string    // Repository.Name; empty means the project itself
    Title     string
    Body      string    // markdown
    Status    string    // see below
    Parent    string    // parent ticket id; empty for a top-level ticket
    DependsOn []string  // tickets that must be done first
    By        string    // creator: "you" | "run RUN-7"
    Created   time.Time
    Updated   time.Time
    Comments  []Comment
    Runs      []string  // run ids, oldest first (filled by part 2)
}

type Comment struct {
    At   time.Time
    By   string // "you" | "run RUN-7"
    Text string // markdown
}
```

The API view adds two computed fields: `runnable` (see Rules) and, on a parent, `progress`
(`{done, total}` over its subtickets).

### Statuses

| Status | Meaning | Used from |
| --- | --- | --- |
| `open` | created, not planned or not approved | 1 |
| `planning` | a planner run is splitting it | 3 |
| `review` | the plan waits for a person | 3 |
| `ready` | may be picked up | 1 |
| `running` | a run works on it | 2 |
| `blocked` | needs a person | 1 |
| `done` | finished | 1 |
| `closed` | dropped without doing it | 1 |

Moves a person may make in part 1 (anything else is 409):

- `open` -> `ready`, `ready` -> `open`
- `blocked` -> `open` or `ready`
- any status except `running`, `planning`, `review` -> `closed`
- `closed` -> `open`
- `ready` -> `done` and `blocked` -> `done` (work done by hand)

`running`, `planning` and `review` are set only by the daemon (parts 2 and 3).

### Rules

- **One level.** A ticket with a `Parent` cannot itself be a parent. A parent must be top-level.
- **A parent is a container.** It is never runnable and has no status moves of its own once it
  has subtickets. Its status is derived from its subtickets on every change:
  - all subtickets `done` or `closed`, at least one `done` -> `done`;
  - all `closed` -> `closed`;
  - any `blocked` -> `blocked`;
  - any `running` -> `running`;
  - otherwise `ready` if any subticket is `ready`, else `open`.
  A top-level ticket without subtickets is an ordinary ticket with the moves above.
- **Dependencies** point to tickets of the same project, never to the ticket itself, its parent
  or its own subtickets. Every write checks the whole graph for a cycle and refuses one with 409
  naming the path ("TCK-4 already waits for TCK-5").
- **Runnable** is computed, never stored: status `ready`, not a parent, and every `DependsOn`
  ticket is `done`. The dispatcher (part 4) reads only this.
- **Delete** is refused with 409 while another ticket depends on it, while it has subtickets, or
  while it is `running`/`planning`. Closing is the normal way out. Deleted ids are not reused.

### Registry

`runner.Tickets`, built like `runner.Projects` and `runner.Runs`: one mutex, an in-memory table
per project, `store.File[[]Ticket]` at `$XDG_STATE_HOME/aigem/projects/{projectID}/tickets.json`.
It owns every rule above. HTTP (part 1) and agent tools (parts 2-3) call the same methods, so no
caller can bypass a rule.

```go
List(project string) ([]TicketView, error)
Get(project, id string) (TicketView, error)
Create(project string, t NewTicket) (TicketView, error) // Repo, Title, Body, Parent, DependsOn, By
Update(project, id string, p TicketPatch) (TicketView, error) // Status, DependsOn
Comment(project, id, by, text string) (TicketView, error)
Delete(project, id string) error
```

- Every change validates, applies, then writes the file under the lock. If the write fails, the
  change is rolled back and the error returned (the lesson from deleting runs).
- The highest id ever handed out survives a restart and a delete, so ids are never reused.
- A change notifies once per touched ticket; a subticket change that moves the parent's derived
  status notifies the parent too.
- Removing a project leaves `tickets.json` on disk, like its other files.

### API

| Route | |
| --- | --- |
| `GET /api/projects/{id}/tickets` | all tickets, oldest first; `?status=`, `?parent=` filter |
| `POST /api/projects/{id}/tickets` | `{repo, title, body, parent, dependsOn}` -> 201 |
| `GET /api/projects/{id}/tickets/{tid}` | one ticket with comments |
| `PATCH /api/projects/{id}/tickets/{tid}` | `{status?, dependsOn?}` -> 200 |
| `POST /api/projects/{id}/tickets/{tid}/comments` | `{text}` -> 201 |
| `DELETE /api/projects/{id}/tickets/{tid}` | 204 |

Title and body cannot be edited in part 1 (the user scoped editing out); `PATCH` refuses them
with 400. HTTP always passes `By: "you"`.

Errors: 404 for an unknown project or ticket; 409 for a broken rule, with a sentence a person
can act on; 400 for bad JSON, an empty title or an unknown field; body capped at 64 KiB and
comments at 16 KiB like every JSON body the API takes.

Feature key `tickets` (present together with `projects`). Control frame
`ticket.updated {projectId, id}`; the page re-reads the list, as it does for `run.updated`.
Activity entries: `ticket.created` and `ticket.closed` only, to keep the feed readable.

### UI

**Tickets screen** (`/tickets`, scoped to the selected project, replaces the placeholder):

- A tree table in the existing grid style: columns Id, Title, Repo, Status. Subtickets are
  indented under their parent; a parent row is collapsible and shows "n/m done".
- A row that is `ready` but not runnable shows "waits for TCK-3" with the ids it waits on.
- A status segmented control: Active (default: everything not `done`/`closed`), Ready, Blocked,
  All; plus the `/` filter on title and id.
- "New ticket" opens a form: repository (select), title, body, parent (optional, top-level
  tickets only), depends on (multi-select). Selecting a row fills the inspector.
- Live: a `ticket.updated` from another tab or from the daemon refreshes the table, with the
  "latest request wins" guard used for the runs list.

**Ticket page** (`/task/{tid}`, replaces the Task placeholder):

- Header: back button to the parent (or to Tickets), id, title, status, "waits for ..." when not
  runnable, and buttons only for the moves a person may make from the current status.
- Under the header: repository, `created by you` or `created by run RUN-12`, times.
- Tabs: Overview (body as markdown), Discussion (comments with author, composer), Runs (empty
  until part 2).
- Right panel: Parent with progress, Waits for (with "+ add dependency" and remove), Blocks
  (tickets that wait for this one), Repository. A refused dependency shows the 409 sentence.
- On a parent: the Overview tab lists the subtickets as tree rows with "Add subticket"; the panel
  shows progress instead of dependencies; no status buttons.
- Palette: "New ticket", "Open ticket" (search by id and title).

### Testing

- `runner`: every rule (one level, derived parent status, cycle refusal including indirect
  cycles, allowed and refused moves, runnable, delete refusals), ids not reused after restart and
  delete, rollback when the file cannot be written, parallel writes under `-race`. Tests set
  `XDG_STATE_HOME` to a temp dir.
- `web`: each route's success and error codes, `ticket.updated` on the control stream, feature
  key present only with projects.
- UI (vitest): tree rendering and collapse, "waits for", status filter, new ticket form, ticket
  page tabs, adding a dependency that makes a cycle shows the error, a parent shows subtickets, a
  live update from another tab, stale list responses ignored.

### Out of scope for part 1

Editing title and body, assignees and agent identities, more than one level of subtickets,
labels and priorities, tracker sync, running a ticket (part 2), the planner (part 3), the
dispatcher (part 4).
