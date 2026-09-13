# Projects Phase Two - Design

Date: 2026-09-13. Status: draft for review.

## What this is

The browser UI's three placeholder screens - Tickets, Task, Worktrees - and the "+ New
project" button become real. A person adds a project (a directory on the daemon's machine),
writes tickets against the repositories inside it, and runs a ticket: the daemon opens an
autonomous run in its own git worktree, and when the run finishes cleanly its branch is merged
into the repository's main branch.

Decisions taken with the user on 2026-09-13:

- Tickets are local. The daemon stores them itself; there is no tracker integration.
- A project is added from the UI by naming a directory. It is saved by the daemon.
- A finished run merges into main by itself. This is the one place the design refuses to be
  quiet: a run that cannot merge cleanly, or whose check command fails, leaves its branch and
  worktree in place and marks the ticket blocked, and a person decides.

## What exists already

- `runner.ModeAutonomous` (`internal/runner/session.go:30-80`): auto-approval of reversible
  tools, no persisted path grants, a capability subset, a turn budget. Runs cannot yet be
  opened in it - `Runs.Create` refuses any mode but interactive.
- `runner.Env` (`internal/runner/env.go`): a project's skills, hooks, MCP servers and system
  prompt, loaded from one directory. The web daemon holds exactly one, for the directory it was
  started in. `Env.NewTools()` roots the sandbox at that directory.
- `store.File[T]`: atomic JSON documents in the state directory (`runs.json` uses it).
- No git helper in Go. The TUI reads file changes through the tools registry, not git.
- The wire (`docs/web.md`): runs, a control stream with a revision counter, a feature map,
  `run.updated` notifications. Every new collection follows the same pattern.

## Scope, in three sub-projects

Each is one spec section, one plan, one review cycle, and ships on its own.

| Sub-project | Delivers | Depends on |
| --- | --- | --- |
| A. Projects | project registry, repositories under it, per-project environments, the sidebar switcher, runs scoped to a project | nothing |
| B. Tickets | local tickets per repository, the Tickets screen, the Task screen's overview and discussion | A |
| C. Runs on tickets | autonomous runs in worktrees, commit, check, auto-merge, Stop run, the Task screen's changes and runs, the Worktrees screen | A, B |

Out of scope for phase two: epics, tracker sync, scheduling or automatic triggering of runs,
pull requests, running more than one autonomous run per ticket at once, and projects on
another machine.

---

## A. Projects

### Model

```go
type Project struct {
    ID      string    // "PRJ-1", assigned by the daemon
    Name    string    // defaults to the directory's base name
    Dir     string    // absolute, must exist, must be a directory
    Created time.Time
}

type Repository struct {
    Name string // directory name under the project
    Dir  string // absolute
    Main string // the branch a run merges into: "main" or "master", read once at discovery
}
```

A project is a directory. Its repositories are the direct child directories that contain a
`.git`, plus the project directory itself when it is a git checkout. Discovery runs on demand
(`GET /api/projects/{id}/repos`), not on a timer.

Persistence: `$XDG_STATE_HOME/aigem/projects.json` through `store.File[[]Project]`. A daemon
without a state directory serves no projects (feature absent).

### Environments

One `runner.Env` per project, loaded on first use with `runner.Load(Options{Cwd: p.Dir})` and
kept for the daemon's life. The daemon's own directory stays a project with no record - the
"This daemon's directory" entry the sidebar already draws - so every run has a project. Loading
an environment dials MCP servers and runs SessionStart hooks, so it happens outside the request
that triggered it the way `OpenRun` already does, and a project whose load fails is listed with
the error and cannot open runs until it succeeds.

Project skills stay gated by the existing trust flow. `POST /api/skills/trust` gains a
`project` field; the Skills screen shows the current project's catalogue.

### Runs

`RunRequest` and the run record gain `ProjectID` (empty means the daemon's directory). A run
opens in its project's environment and is rooted at the project directory, exactly as today's
runs are rooted at the daemon's. The session list is filtered to the current project, with an
"all projects" toggle.

### API

| Route | |
| --- | --- |
| `GET /api/projects` | the registry, plus the daemon's directory as `{"id":"","name":...}` |
| `POST /api/projects` | `{"dir":"/abs/path","name":"optional"}`; 201 with the record; 400 for a path that is not an absolute existing directory, or is already registered |
| `DELETE /api/projects/{id}` | forgets the project; 409 while it has an open run; its tickets and worktrees stay on disk |
| `GET /api/projects/{id}/repos` | discovery, as above |
| `POST /api/runs` | accepts `projectId` |

Feature key: `projects`. Control frames: `project.updated` (added, removed, load failed).

### UI

- The sidebar's Projects block lists projects; the active one is a store value saved in
  `localStorage` per browser. "+" opens a modal with a path input and a name input; the daemon's
  400 sentence is shown as text.
- Route `/projects/{id}` selects; every list screen (Sessions, Skills, Tickets, Worktrees)
  reads the selection.
- The Worktrees screen, in this sub-project, lists repositories with their main branch and
  "no worktrees yet"; C fills it.

### Security

The daemon serves one signed-in person on their own machine, and a project directory is a path
that person typed. The sandbox roots at the project directory as it does today. `DELETE`
never removes files.

---

## B. Tickets

### Model

```go
type Ticket struct {
    ID       string    // "TCK-1", per project, never reused
    Repo     string    // Repository.Name; empty means the project itself
    Title    string
    Body     string    // markdown
    Status   string    // "open" | "running" | "blocked" | "done" | "closed"
    Created  time.Time
    Updated  time.Time
    Comments []Comment
    Runs     []string  // run ids, oldest first (filled by C)
}

type Comment struct {
    At   time.Time
    By   string // "you" | "run RUN-7"
    Text string // markdown
}
```

Status transitions: `open -> running` when a run starts (C); `running -> done` after a merge,
`running -> blocked` after a failed check or merge, `blocked -> open` when a person resets it;
`open|blocked|done -> closed` by a person. A closed ticket can be reopened.

Persistence: `$XDG_STATE_HOME/aigem/projects/{projectID}/tickets.json` via
`store.File[[]Ticket]`. Bodies are capped at 64 KiB and comments at 16 KiB, like every JSON
body this API takes.

### API

| Route | |
| --- | --- |
| `GET /api/projects/{id}/tickets` | all tickets, oldest first; `?status=` filters |
| `POST /api/projects/{id}/tickets` | `{"repo","title","body"}`; 201 |
| `GET /api/projects/{id}/tickets/{tid}` | one ticket with comments |
| `PATCH /api/projects/{id}/tickets/{tid}` | any of `title`, `body`, `repo`, `status` (only the person's transitions) |
| `POST /api/projects/{id}/tickets/{tid}/comments` | `{"text"}`; 201 |
| `DELETE /api/projects/{id}/tickets/{tid}` | 409 while running; otherwise gone |

Feature key: `tickets` (present with `projects`). Control frame: `ticket.updated` carrying
`{projectId, id}`.

### UI

- Tickets screen: a grid (the canvas's table: id, title, repo, status, updated, runs) with the
  `/` filter and a status segmented control; "New ticket" opens a form (repo select, title,
  body). Selecting a row fills the inspector: status, repo, timestamps, "Open", "Run" (C).
- Task screen `/task/{tid}`: tabs Overview (body, rendered through `Markdown`), Discussion
  (comments, a composer), Changes and Runs (empty until C). Status is changed from the header.
- Palette: "New ticket", "Open ticket" (navigates), per-ticket search by title.

---

## C. Runs on tickets

### What "Run" does

1. Refuses if the ticket is `running`, or its repository's main checkout is dirty, or another
   run already holds a worktree for this ticket.
2. Creates `git worktree add <project>/.aigem/worktrees/<tid>-<runid> -b aigem/<tid>` from the
   repository's main branch.
3. Opens a run in `ModeAutonomous`, in the project's environment, with the sandbox rooted at
   the worktree (`Env.NewToolsAt(dir)`, a new method that roots a registry elsewhere than
   `e.Cwd`). The first message is the ticket: title, body, and the discussion so far, plus one
   sentence saying the run works in a branch and must leave the tree committed.
4. Sets the ticket `running`, records the run id on it, announces `ticket.updated`.

### What happens when the turn ends

Evaluated once, by the daemon, when the run's turn ends (the `turn_end` event):

1. If the turn ended with an error or an interrupt: ticket `blocked`, a comment from the run
   with the error, worktree kept.
2. `git add -A && git commit` in the worktree with the message `aigem: <ticket title>
   (TCK-n, RUN-m)`; a worktree with nothing to commit is `blocked` with "the run changed
   nothing".
3. If the repository declares a check command (`.aigem/project.json` in the repository:
   `{"check": "make test"}`), it runs in the worktree with a 15 minute budget; failure is
   `blocked` with the last 4 KiB of output as a comment.
4. Merge: in the repository's main checkout, `git merge --no-ff aigem/<tid>` on main. A
   conflict aborts the merge (`git merge --abort`), the ticket is `blocked` with the conflict
   list, the branch and worktree stay.
5. On success: ticket `done`, a comment with the merge commit, worktree removed, branch kept.

A run's turn budget is the autonomous default. A second turn on the same run (a person typing
into it) is allowed while the ticket is `blocked`; the evaluation runs again after it.

### Stop run

The Run screen's missing button. `POST /api/runs/{id}/stop`: interrupts the turn, closes the
run, removes the worktree and its branch, sets the ticket `blocked` with "stopped by a
person". Confirmed by a dialog, as the canvas draws it.

### Worktrees screen

Per repository: the branches `aigem/*` with their worktree path, ticket, run, and state
(running, kept after a block, merged). Actions: Open run, Open ticket, Discard (removes the
worktree and branch; refused while the run is live).

### API additions

| Route | |
| --- | --- |
| `POST /api/projects/{id}/tickets/{tid}/run` | starts a run; 201 with the run record; 409 for the refusals above |
| `POST /api/runs/{id}/stop` | as above |
| `GET /api/projects/{id}/worktrees` | the list above |
| `DELETE /api/projects/{id}/worktrees/{name}` | discard |

`Run` gains `projectId`, `ticketId`, `worktree`, `branch`. `run.updated` already carries the
record.

### Git

A small package `internal/gitx` wrapping `exec.Command("git", ...)` with a context, a working
directory, and a bounded output buffer: `MainBranch`, `IsClean`, `WorktreeAdd`,
`WorktreeRemove`, `CommitAll`, `Merge`, `MergeAbort`, `BranchDelete`. Errors carry git's own
stderr, trimmed, because that is the sentence a person needs.

### Safety

- The autonomous session cannot leave its worktree: the sandbox root is the worktree.
- The merge runs in the main checkout only when `IsClean` says so, and only fast-forward or
  clean merge; every other outcome leaves the branch for a person.
- One autonomous run per ticket; one merge at a time per repository (a mutex per repository
  in the daemon).
- Nothing pushes. Remotes are the person's business.

---

## Order of work

1. Sub-project A, then its plan and execution.
2. B.
3. C.

Each plan follows the day-one conventions: failing test first, task review, deploy to the
`aigem-web` unit, headless check, push to `main`.
