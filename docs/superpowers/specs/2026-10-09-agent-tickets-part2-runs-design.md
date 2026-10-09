# Agent Tickets Part 2: Runs on Tickets - Design

Date: 2026-10-09. Status: approved in conversation; implementation by subagents.

Part 2 of `2026-10-09-agent-tickets-design.md`. Replaces section C of
`2026-09-13-projects-phase-two-design.md`, keeping most of its mechanics.

## What this is

A runnable ticket is started with "Run". The daemon gives the ticket its own git worktree and an
autonomous agent run inside it. When the agent says it is finished, the daemon commits, runs the
repository's check and merges into `main`. Anything that goes wrong makes the ticket `blocked`
with a reason a person can act on. Part 4's dispatcher will press "Run" by itself; part 2 adds the
button and everything behind it.

Decisions taken with the user on 2026-10-09:

- **Explicit finish signal.** The agent calls the tool `ticket_done(summary)`. A turn that ends
  without it makes the ticket `blocked`, with the agent's last message as a comment. A turn end
  alone never merges.
- **Merge straight into `main`** in the repository's own checkout, only when that checkout is
  clean and on `main`. Otherwise the ticket is `blocked` with the reason and a "Retry merge"
  button. (The user works on `main` in the same checkout; this is the accepted cost.)
- A `blocked` ticket keeps its run alive, so a person can type into it; the agent continues and
  the daemon evaluates again after that turn.
- Nothing is pushed. Remotes are the person's business.

## Lifecycle

### Start

`POST /api/projects/{id}/tickets/{tid}/run` (the "Run" button on the ticket page):

1. Refused with 409 and a sentence when the ticket is not `runnable`, is already `running`, its
   repository is not a git checkout, has no `main` (or `master`) branch, or a worktree for this
   ticket already exists.
2. `git worktree add <project dir>/.aigem/worktrees/<TCK-n> -b aigem/<TCK-n> <main>` in the
   ticket's repository (the project directory itself when `repo` is empty). Dependencies were
   merged into `main` before this ticket became runnable, so their work is present.
3. A run opens in `ModeAutonomous` in the project's environment, with the tool sandbox rooted at
   the worktree (`Env.NewToolsAt(dir)`) and the extra tool `ticket_done`.
4. The first message is the ticket: title, body, the discussion so far, and the rule: "Work in
   this worktree. When the work is complete and committed, call ticket_done with a short summary.
   If you are stuck or need a decision, explain why and stop without calling it."
5. The ticket becomes `running`; the run id is appended to its `Runs`.

### End of a turn

Evaluated by the daemon once per `turn_end` of a ticket run:

- **`ticket_done` was called in this turn:**
  1. `git add -A && git commit -m "aigem: <title> (TCK-n, RUN-m)"` in the worktree if anything is
     uncommitted.
  2. If the repository declares a check (`.aigem/project.json` in the repository:
     `{"check": "make test"}`), it runs in the worktree through the shell with a 15 minute
     budget. Failure: `blocked`, comment with the last 4 KiB of output.
  3. Merge (below). Success: `done`, comment with the agent's summary and the merge commit; the
     worktree is removed; the branch `aigem/<TCK-n>` is kept.
- **No `ticket_done`:** `blocked`, comment from the run with its last assistant message (or "the
  turn ended with an error: ..." / "interrupted"). Worktree and run stay.

A further turn on the same run (a person typed into it while `blocked`) moves the ticket back to
`running` and is evaluated again when it ends.

### Merge

- Only in the repository's main checkout, only when `git status` is clean and the current branch
  is `main`; otherwise `blocked` with "the main checkout has uncommitted changes" or "the main
  checkout is on <branch>, not main".
- `git merge --no-ff aigem/<TCK-n>`. A conflict runs `git merge --abort`; `blocked` with the list
  of conflicting files.
- One merge at a time per repository (a mutex per repository in the daemon).
- `POST /api/projects/{id}/tickets/{tid}/merge` ("Retry merge") repeats the merge for a ticket
  whose branch is committed and checked; 409 with the same sentences when it still cannot.

### Stop

`POST /api/runs/{id}/stop` ("Stop" on the ticket page and the run screen): interrupts the turn
and ends the session, keeping the run record and the worktree. The ticket becomes `blocked` with
"stopped by a person".

### Edge cases

- Daemon restart closes every run: on start, a ticket that is `running` with no live run becomes
  `blocked` with "the daemon restarted"; its worktree stays.
- Deleting the run that drives a ticket (the trash button): the ticket becomes `blocked` with
  "the run was deleted".
- `DELETE /api/projects/{id}/worktrees/{name}` (Discard on the Worktrees screen) removes the
  worktree and its branch; 409 while its run is live.

## Components

| Unit | Responsibility |
| --- | --- |
| `internal/gitx` | `exec.Command("git", ...)` with a context, a directory and a bounded output buffer: `MainBranch`, `IsClean`, `CurrentBranch`, `WorktreeAdd`, `WorktreeRemove`, `BranchDelete`, `CommitAll`, `Merge`, `MergeAbort`, `ConflictFiles`, `Worktrees`. Errors carry git's stderr, trimmed. |
| `Env.NewToolsAt(dir)` | the environment's tool registry rooted at another directory |
| `ticket_done` tool | only on ticket runs; records "the agent finished, with this summary" on the run; touches nothing else |
| `runner.TicketRuns` | the coordinator: start, end-of-turn decision, commit/check/merge, stop, retry merge, restart recovery, worktree list and discard. Uses `Runs`, `Tickets`, `gitx`. `Runs` stays unaware of tickets beyond the record fields. |
| `Tickets` daemon methods | `Start(project, id, run)` -> `running`; `Finish(project, id, status, comment)` -> `done`/`blocked`. They bypass the person-move rules; person moves are unchanged. |

Records: `Run` gains `ticketId`, `worktree`, `branch` (`projectId` exists). A ticket's `Runs`
is filled.

## API

| Route | |
| --- | --- |
| `POST /api/projects/{id}/tickets/{tid}/run` | start; 201 with the run record; 409 with a reason |
| `POST /api/runs/{id}/stop` | stop the run; 204 |
| `POST /api/projects/{id}/tickets/{tid}/merge` | retry merge; 200 with the ticket; 409 with a reason |
| `GET /api/projects/{id}/worktrees` | `aigem/*` branches: name, path, ticket, run, state (`running`, `kept`, `merged`) |
| `DELETE /api/projects/{id}/worktrees/{name}` | discard; 204; 409 while its run is live |

Activity: `ticket.done` and `ticket.blocked` (with the reason). Control frames: the existing
`ticket.updated` and `run.updated`.

## UI

- Ticket page: "Run" (when runnable), "Stop" (when running), "Retry merge" (when blocked on a
  merge); the Runs tab lists the ticket's runs with status and a link to each run.
- Run screen: the existing missing "Stop" button calls the stop route.
- Worktrees screen: per repository the `aigem/*` branches with worktree path, ticket, run, state;
  actions Open run, Open ticket, Discard (confirmed by a dialog).

## Safety

- The autonomous session cannot leave its worktree: the sandbox root is the worktree.
- The main checkout is only ever touched by the merge, and only when clean and on `main`.
- One run per ticket; one merge at a time per repository.
- Nothing pushes.

## Testing

- `gitx` against real temporary repositories (`git init` in `t.TempDir()`), including a
  conflict and a dirty checkout.
- `TicketRuns` with a fake model/session: done -> commit, check pass/fail, merge success,
  conflict, dirty checkout, wrong branch, no `ticket_done`, a second turn after `blocked`, stop,
  restart recovery, deleting the driving run. Tests set `XDG_STATE_HOME`.
- HTTP routes: success and every refusal code.
- UI (vitest): Run/Stop/Retry merge visibility and calls, Runs tab, Worktrees screen and Discard.

## Out of scope

Pushing, pull requests, more than one run per ticket at a time, scheduling (part 4), the planner
and ticket tools beyond `ticket_done` (part 3), choosing a model per ticket.
