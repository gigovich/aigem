# Delete sessions

Status: implemented; manual Post-Completion checks pending.

## Overview

- Today the web UI can only "close" a session, and a closed session stays in the list forever.
  "Close" is a confusing extra concept for the person using the UI.
- Replace it with one action: **delete**. Every row in the sessions list gets a trash button.
  Deleting a live session ends it and deletes it in one step; deleting a closed one just
  deletes it.
- Delete removes the run from the table and deletes the conversation from disk: the journal
  (events, blobs, artifacts) and the saved session (`sessions/<id>.json` plus its
  `.precompact-<n>.json` backups), so it also disappears from TUI `/resume`.
- The "closed" tag in the list stays: a daemon restart still leaves old sessions closed
  (readable, not continuable).

## Context (from discovery)

- `internal/runner/runs.go` - the run registry. Table = `byID` + `order`, saved by
  `saveLocked`. `next` (highest run number) is rebuilt on load from the ids in the table.
  `CloseRun` has no production caller besides the web backend; `shutdown` closes sessions
  itself; `closeSession` saves the session (`Local.Save()`) before releasing it.
  Every reader already handles a missing row (`lr == nil` / `byID[id] != lr`).
- `internal/uisession/journal.go` - `<state>/journal/<session-id>/` (`events.jsonl`, `blobs/`,
  `artifacts.json`); `journalDir` validates the id. No delete helper.
- `internal/session/session.go` - `<state>/sessions/<id>.json`; `List()` is what TUI
  `/resume` reads. `internal/agent/compact.go` writes `<id>.precompact-<n>.json` beside it.
  No delete helper.
- `internal/web/api_runs.go`, `server.go`, `backend.go` - `DELETE /api/runs/{id}` means
  "close" today and answers `200` with the run JSON. The web UI is the only client.
- `cmd/aigem/webbackend.go` - `webBackend.CloseRun` with `closeMu` read-before-close,
  activity `run.closed`.
- Callers of `RunsBackend.CloseRun` in tests: `cmd/aigem/webphase1_test.go` (162, 165, 514,
  670), `cmd/aigem/webprojects_test.go:129`, fake in `internal/web/backend_test.go:151`,
  plus `internal/web/api_runs_test.go`.
- UI: `src/lib/api.ts` (`closeRun`), `src/App.tsx` (close confirm modal),
  `src/screens/Chat.tsx` (`SessionRow` `×`), `src/ui/Modal.test.tsx` ("Close this session?"
  title). On `run.updated` the page refetches the runs list, so other tabs drop the row with
  no new event kind. A tab showing `/chat/{deleted}` gets a 404 from the socket `diagnose()`
  and shows "this run no longer exists".
- `docs/web.md:148` and the paragraph near line 369 describe `DELETE` as "close, not delete".

## Development Approach

- **testing approach**: Regular (code first, then tests in the same task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change
- Go tests that touch the state dir must set `XDG_STATE_HOME` to a temp dir (otherwise a
  test can overwrite real state)

## Testing Strategy

- Go unit tests next to the changed code (`go test ./...`, `go test -race` for runner).
- UI tests with vitest. On Node 25 run them as
  `NODE_OPTIONS=--no-experimental-webstorage npm test` (plain `npm test` breaks on
  `localStorage` for an unrelated reason).
- No e2e suite in the repo; the last check is manual in the browser.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- keep plan in sync with actual work done

## Solution Overview

- **API**: `DELETE /api/runs/{id}` changes meaning from "close" to "delete" and answers `204`
  (was `200` + run JSON); `404` for an unknown or already deleted run. No close endpoint any
  more. A contract change, but the UI ships inside the same binary, so both sides change
  together.
- **Runner `Remove(id)`**, done so that no reader has to change:
  - in **one** critical section: find the row, detach `sess`/`release`, delete it from `byID`
    and `order`, append its record (status `removed`) to a new `removed` list, save;
  - outside the lock: read `Meta()` for the session id, `closeSession` (ends the turn,
    saves), `notify`, then delete the journal and the session files.
  - A second concurrent `Remove` finds no row and gets `ErrNoRun`; `shutdown`, `sync`,
    `publish`, `setModel`, `Apply` and the rest already treat a missing row as gone.
- **Ids are never reused**: `saveLocked` writes live rows plus `removed` rows (same file
  shape, status `removed`); `NewRuns` puts `removed` rows only into the `removed` list and
  still counts them for `next`. Downgrade note: an older binary turns those rows into closed
  runs with no journal.
- **File deletion** failures are logged, not returned: the run is already gone from every
  list. Deleting files after `closeSession` matters, because that save would otherwise write
  `sessions/<id>.json` back.
- **CloseRun** stays in the runner (about 13 runner tests use it to reach the closed state),
  but leaves the web backend and the HTTP API.
- **Activity**: `run.removed` replaces `run.closed`. `closeMu` in `webBackend` goes away:
  only one `Remove` can succeed, so record the activity when it returns nil.
- **Known limit**: there is no cross-process lock. If a TUI has `/resume`d the same session
  while the web deletes it, the TUI keeps working (journal writes go to a deleted file or
  fail with a logged note) and its next save writes `sessions/<id>.json` back. Documented,
  not prevented.

## Technical Details

- `runner`: `RunRemoved RunStatus = "removed"`; `Remove(id string) error`. The table keeps
  one `removed` row for the highest id handed out when no run holds it, so ids are not reused.
- Session id to delete: `meta.ID` if set, else `rec.SessionID`; empty means the run never
  had a turn, so there are no files.
- `uisession.RemoveJournal(id string) error`: `os.RemoveAll(journalDir(id))`.
- `session.Remove(id string) error`: delete `<id>.json` and `<id>.precompact-*.json`;
  missing files are not an error; id validated like `pathFor`.
- `web.RunsBackend`: `CloseRun` replaced by `RemoveRun(ctx, id) error`.
- UI: `api.closeRun` becomes `api.removeRun(id)`; `SessionRow` `onClose` becomes `onRemove`
  with a trash icon on every row; the confirm modal says
  "Delete this session? This cannot be undone." with a danger "Delete" button; if the open
  session is the deleted one, navigate to `/chat`.

## What Goes Where

- **Implementation Steps**: code, tests and docs in this repo.
- **Post-Completion**: manual browser and TUI check.

## Implementation Steps

### Task 1: File removal helpers

**Files:**
- Modify: `internal/uisession/journal.go`, `internal/uisession/journal_test.go`
- Modify: `internal/session/session.go`, `internal/session/session_test.go`

- [x] add `uisession.RemoveJournal(id string) error` (`journalDir` + `os.RemoveAll`)
- [x] add `session.Remove(id string) error` for `<id>.json` and `<id>.precompact-*.json`
- [x] write tests: each removes only its own session's files and leaves another session
- [x] write tests: missing files are not an error; invalid id (`..`, `a/b`) is refused
- [x] write test: after `session.Remove`, `session.List()` no longer returns the id
- [x] run `go test ./internal/uisession/... ./internal/session/...` - must pass before task 2

### Task 2: Remove a run in the registry

**Files:**
- Modify: `internal/runner/runs.go`
- Modify: `internal/runner/runs_test.go`

- [x] add `RunRemoved`, the `removed` list, and include it in `saveLocked`
- [x] load: `removed` rows go only to the `removed` list and still count for `next`
- [x] add `(*Runs).Remove(id string) error` as described in Solution Overview
- [x] write tests: remove a closed run (gone from List/Get, journal and session files gone,
      notify called once)
- [x] write tests: remove a live run while a turn runs (session closed before files are
      deleted; the call returns after that)
- [x] write tests: remove a run with no session id; unknown id and second remove -> `ErrNoRun`
- [x] write tests: concurrent `Remove` (exactly one nil, others `ErrNoRun`); `Remove` racing
      `Close()` (row never comes back)
- [x] write tests: after reload from the store the run stays hidden and the next id is higher
      than the removed one
- [x] run `go test -race ./internal/runner/...` - must pass before task 3
- ⚠️ `TestLoadRefusesAnUnresolvableWorkingDirectory` and
  `TestLoadErrorNamesTheDirectoryAndKeepsTheCause` (env_test.go) fail on macOS on a clean
  tree too; not related to this work
- ➕ `Remove` returns `ErrRunsClosed` once shutdown started (its closing save would write
  the files back); removed rows keep only id, status and timestamps, no title

### Task 3: HTTP endpoint and backend

**Files:**
- Modify: `internal/web/backend.go`, `internal/web/api_runs.go`, `internal/web/server.go`
- Modify: `internal/web/backend_test.go` (fake), `internal/web/api_runs_test.go`
- Modify: `cmd/aigem/webbackend.go`
- Modify: `cmd/aigem/webphase1_test.go`, `cmd/aigem/webprojects_test.go`

- [x] replace `CloseRun` with `RemoveRun` in `RunsBackend` and `webBackend`
      (activity `run.removed`, drop `closeMu`)
- [x] `DELETE /api/runs/{id}` calls `RemoveRun` and answers `204` (was `200` + run JSON)
- [x] update the fake backend and every `CloseRun` test caller to the new meaning
- [x] write tests: 204 on closed run, 204 on live run, 404 unknown, 404 on second delete
- [x] write backend tests: activity entry recorded once under concurrent deletes;
      `ErrUnavailable` without runs
- [x] run `go test ./internal/web/... ./cmd/aigem/...` - must pass before task 4

### Task 4: Trash button and confirm dialog in the UI

**Files:**
- Modify: `internal/web/_ui/src/lib/api.ts`, `src/screens/Chat.tsx`, `src/App.tsx`
- Modify: `internal/web/_ui/src/screens/chat.test.tsx` (or `screens.test.tsx`),
  `src/ui/Modal.test.tsx`, `src/test/harness.tsx` if the fake API needs it

- [x] rename `api.closeRun` to `api.removeRun` (same `DELETE` route, no body expected)
- [x] `SessionRow`: replace the `×` with a trash icon button "Delete session" on every row
- [x] replace the close confirm modal in `App.tsx` with the delete one, danger button;
      on success refresh runs and leave `/chat/{id}` if it was open
- [x] update existing close tests; write tests: every row has Delete and no Close
- [x] write tests: confirming calls the delete route and the row disappears; cancel does
      nothing; the open session view moves to `/chat`
- [x] run lint, typecheck and vitest - must pass before task 5

### Task 5: Verify acceptance criteria

- [x] live and closed sessions can both be deleted from the list in one step
      (live daemon on a temp state dir: `DELETE` on a live run and on a closed run after a
      restart both answer `204`; then `GET` and a second `DELETE` answer `404`, list drops them)
- [x] journal dir, `sessions/<id>.json` and precompact backups are gone; TUI `/resume` no
      longer lists it (checked on disk for both deleted runs, other run's files kept; `/resume`
      covered by the `session.List()` test - interactive TUI not automatable)
- [x] after a daemon restart the deleted run stays gone and a new run gets a new id
      (`runs.json` keeps a `removed` row; after restart RUN-1 is `404`, new run is RUN-4)
- [x] a second open tab drops the row without reload; a tab on the deleted session shows
      "this run no longer exists" (control socket gets `run.updated` with status `removed`
      and `activity.updated`; `GET` of the run is `404`, which `socket.ts` maps to that text;
      browser itself skipped - not automatable here, covered by vitest)
- [x] run `go test ./...`, `make lint`, and UI checks (`npm run lint`, `npm run check`, vitest)
      (vitest 313/313, eslint and tsc clean. ⚠️ pre-existing on `main` too, not from this
      branch: `go test` fails `TestLoadRefusesAnUnresolvableWorkingDirectory`,
      `TestLoadErrorNamesTheDirectoryAndKeepsTheCause` (runner) and `TestSetupSandboxIsPrivate`
      (testenv) on macOS; golangci-lint v2.6.2 reports one QF1003 in untouched
      `internal/tui/model_add.go`)

### Task 6: [Final] Update documentation

- [x] rewrite `docs/web.md` around line 148 and line 369: `DELETE` now deletes, `204`,
      `run.removed` activity, and the cross-process limit
- [x] add a CHANGELOG entry
- [x] mark this plan done

## Post-Completion

**Manual verification (pending):**
- [ ] in the browser: delete a live and a closed session, check the confirm text and that the
  open session view moves away when its run is deleted
- [ ] with a TUI that `/resume`d the same session: deleting from the web does not crash the TUI
