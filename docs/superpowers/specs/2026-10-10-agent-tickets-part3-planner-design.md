# Agent Tickets Part 3: Planner - Design

Date: 2026-10-10. Status: approved in conversation; written for review.

Part 3 of `2026-10-09-agent-tickets-design.md`. Builds on part 1 (tickets) and part 2 (runs on
tickets).

## What this is

A person presses "Plan" on a ticket. The daemon opens a planner run: an agent that reads the code
and splits the ticket into subtickets linked with dependencies. The ticket shows `planning` while
it works and `review` when the plan waits for a person. The person approves the plan, rejects it,
or asks the planner to revise it. Approval is the one human gate of the whole system: after it the
subtickets are `ready`, and part 4's dispatcher (or a person with "Run") picks them up.

Decisions taken with the user on 2026-10-10:

- **The draft is real subtickets.** The planner creates ordinary subtickets with status `open`.
  A person can edit the draft with the existing UI. This answers the part 1 question: a parent's
  status is not derived from its subtickets while it is `planning` or `review`.
- **The planner reads code and writes tickets.** Read-only tools rooted at the repository's main
  checkout, plus ticket tools. No file writes, no shell, no worktree.
- **Approve, Reject or Revise.** Revise sends the person's feedback to the same planner run, which
  edits its draft and finishes again.

## Statuses and rules

- **Who can be planned.** A top-level ticket with status `open` and no subtickets. "Plan" moves it
  to `planning` and opens a planner run. Refusals (409):
  `<id> is a subticket; only a top-level ticket is planned`, `<id> is <status>, not open`,
  `<id> already has subtickets`, `<id> has a live run <run>; stop it first`.
- **No derive while planning.** While a parent is `planning` or `review`, its status is not
  derived from its subtickets (`settleParents` skips it). Approval hands it back to derivation.
- **Draft subtickets.** The planner's subtickets have status `open` and `by: run RUN-n`. While the
  parent is `planning` or `review`:
  - a draft subticket has no status moves (409 `<id> is part of a plan in review`), so nothing
    can make it `ready`, run it or close it;
  - no ticket outside the plan may depend on a draft subticket (409
    `<id> is a draft of <parent>'s plan`);
  - in `planning` a person cannot add, delete or relink subtickets of that parent (409
    `<parent> is being planned`); in `review` a person can, with the existing UI.
- **Approve** (`review` only): every subticket becomes `ready`, the planner run is stopped, and
  the parent's status is derived again (so it becomes `ready`). Refused with zero subtickets:
  `<id>'s plan has no subtickets; revise or reject it`.
- **Reject** (`review` only), with a reason: the subtickets are deleted, the planner run is
  stopped, the ticket goes back to `open`, and the reason is a comment
  (`Plan rejected: <reason>`).
- **Revise** (`review` only), with feedback: the feedback is a comment by `you`, the ticket goes
  to `planning`, and the feedback is sent to the planner run as a new message. When that run is
  no longer live, a new planner run is opened; its first message carries the draft and the
  feedback.
- **End of a planner turn.** The ticket goes to `review` in every case; `review` is "needs a
  person" for a plan, so no new status is needed:
  - with `plan_done(summary)`: the summary is a comment;
  - without it: the agent's last message is the comment;
  - interrupted or ended with an error (also after `plan_done`): `interrupted` /
    `the turn ended with an error: ...`.
- **Stop, delete, restart.** Stopping the planner run moves the ticket to `review` with
  `stopped by a person`; deleting it, `the run was deleted`; a daemon restart moves every
  `planning` ticket to `review` with `the daemon restarted`. The draft stays in all three.
- **One run per ticket.** The planner run is appended to the ticket's `Runs`; part 2's rules
  apply: only the last run changes the ticket, and "Stop" is offered while it is live. Planner
  runs share the daemon's limit on live runs.
- `Delete` of a ticket stays refused while it is `planning` (part 1) or has subtickets.

## The planner run

- An autonomous run with the `read-only` capability profile (read, list, search tools). It is
  rooted at the ticket's repository main checkout (`<project dir>/<repo>`, the project directory
  when `repo` is empty). `RunRequest` gains the profile and a root that is not a worktree; an
  autonomous run still needs a `TicketID`.
- Its prompt is built at that root (as in part 2). Its first message: the ticket title, body and
  discussion; the project's repositories by name; the rule: "Split this ticket into subtickets
  that each fit one autonomous run of a coding agent. Link them with dependencies. Read the code
  as much as you need; you cannot change it. When the plan is complete, call plan_done with a
  short summary."
- Ticket tools, registered only into this run. Each acts only while the run's ticket is
  `planning` and the run is its last run, and only on that ticket's subtickets:
  - `list_tickets()` and `get_ticket(id)`: read the project's tickets;
  - `create_subticket(repo, title, body, dependsOn)`: `repo` must be a repository of the project
    (`""` is the project itself), else `"<repo>" is not a repository of this project`;
  - `delete_subticket(id)` and `set_dependencies(id, dependsOn)`: for revisions;
  - `plan_done(summary)`: records the summary; the daemon acts at the end of the turn.

  All writes go through `Tickets` methods, so every part 1 rule (one level, cycles, limits) holds.

## Components

- `runner.Tickets`: the derive skip, the draft guards, and the moves `StartPlan`, `PlanReview`,
  `Approve`, `Reject`, `Revise`, each inside the registry lock and checking the last run.
- `runner.TicketRuns` (part 2's coordinator) gains `Plan`, `Approve`, `Reject`, `Revise`; its
  `Stop`, `Remove` and `Recover` handle planner runs (`review` instead of `blocked`). The ticket
  tools live next to `ticket_done`.
- `runner.Runs` / `cmd/aigem`: open a run with a capability profile and a non-worktree root.
- `web`: routes and handlers; `cmd/aigem`: adapters and activity.
- UI: ticket page buttons, draft badge, reason and feedback boxes; Tickets list filter.

## API

All on `/api/projects/{id}/tickets/{tid}/`; refusals are 409 with the reason as text.

| Route | Does |
| --- | --- |
| `POST plan` | starts a planner run; 201 with the run |
| `POST approve` | approves the plan; 200 with the ticket |
| `POST reject` | `{reason}`; deletes the draft, back to `open`; 200 with the ticket |
| `POST revise` | `{text}`; sends the feedback to the planner; 200 with the ticket |

Activity: `ticket.planned` (review reached, with the summary or reason), `ticket.approved`,
`ticket.rejected`.

## UI

- Ticket page: "Plan" on a top-level `open` ticket without subtickets, next to "Run". In `review`:
  "Approve", "Reject" and "Revise"; Reject and Revise open a small text box (reason, feedback).
  "Stop" while the planner run is live.
- In `planning` and `review` the Overview tab marks the subtickets with a "Draft" badge.
- Tickets screen: the "Blocked" filter becomes "Needs you" and lists `blocked` and `review`.
- `docs/web.md`: routes, a "Planner" section; `CHANGELOG.md`: one line.

## Testing

- `runner`: no derive while `planning`/`review`; the draft guards; approve, reject and revise,
  each with its refusals; the tools' guards (other ticket, not planning, stale run, unknown
  repo); end of turn with and without `plan_done`, interrupted, error; Stop, delete and restart
  moving to `review`; revise with a dead run opens a new one. Real git repositories and the
  scripted model, as in part 2.
- `web`, `cmd/aigem`: routes, 409s, activity.
- UI: buttons per status, the draft badge, the reason and feedback boxes, the filter.
- A manual check with a real model: plan, revise, approve, then run one subticket.

## Out of scope

The dispatcher (part 4), plans deeper than one level, editing a ticket's title or body by hand,
planning a ticket that already has subtickets.
