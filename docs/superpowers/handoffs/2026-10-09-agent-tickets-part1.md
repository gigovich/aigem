# Hand-off: agent tickets part 1

Date: 2026-10-09. Status: part 1 done and pushed to main (eea42d0..c06f918).
Spec: `docs/superpowers/specs/2026-10-09-agent-tickets-design.md`.
Plan: `docs/superpowers/plans/2026-10-09-agent-tickets-part1.md`.

## Decide before part 3 (planner)

- A parent's status is always derived from its subtickets (`settleParents`). So `planning` and
  `review` cannot stay on a parent once the planner adds the first subticket. Options: do not
  derive while the parent is `planning`/`review`, or keep the plan as a draft until approval.

## Decisions taken during part 1

- "Add subticket" is on every top-level ticket, not only on existing parents.
- A row on the Tickets screen opens the ticket page; there is no inspector for tickets (spec
  updated).
- A subticket also waits for its parent's dependencies (`runnable`, cycle check, UI "waits for").
- Cycle search uses one shared visited set; a 600-ticket chain checks in about 0.04 s.

## Open follow-ups (small, none blocks part 2)

- Palette lists every ticket as "Open ticket" with an empty query; may need a cap.
- `UpdateTicket` reads the old status outside the registry lock; two parallel closes can both
  write `ticket.closed` to the activity feed.
- Deleting a parent's last subticket leaves the parent at its last derived status.
- `derive([])` returns `closed`; subtickets in `planning`/`review` map the parent to `open`.
- A dropped (`closed`) dependency never makes a ticket runnable (spec says "every dependency
  done"); product decision if that should change.
- Repo name in a new ticket is not checked against the project's repositories (matters when agents
  create tickets in part 3).
- Comment size limit applies to the raw JSON body, so a heavily escaped comment under 16 KiB can be
  refused; the 64 KiB cap is on the whole create request.
- A failed stale ticket-list read still shows a banner (same as the runs list).
- Each parent row's collapse button is an extra Tab stop in the grid.
- Progress wording differs: "N of M done" (side panel) and "N/M done" (overview).
- No busy guard on status buttons; a double click sends two PATCHes.
- Test gaps: delete of running/planning tickets, comment/delete/unavailable paths in
  `cmd/aigem`, Cmd/Ctrl+Enter send, dependency removal, palette gating, text filter.
- Long lines over the limit in a few tests and in the `docs/web.md` routes table.

## Not ours, seen while working

- On macOS these fail on main too: `TestLoadRefusesAnUnresolvableWorkingDirectory`,
  `TestLoadErrorNamesTheDirectoryAndKeepsTheCause` (runner), `TestSetupSandboxIsPrivate`
  (testenv). Flaky: `TestSpecEvictionSettingsReachTheAgent` (runner),
  `TestSkillsBrowserAndDispatch` (tui).
- Local `golangci-lint` is v1 and cannot read this repo; use
  `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run ./...`.
- UI tests need `NODE_OPTIONS=--no-experimental-webstorage` on Node 25.
