# Web UI QA Round Two Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the five findings the second headless QA pass of 2026-09-13 left open: the
daemon's duplicate Close frame that logs a console error on every run switch, the skill action
a phone cannot reach, a diff line that breaks on a lone carriage return, two plural strings,
and a drawer that lets Tab escape.

**Architecture:** One daemon change in `internal/web/ws.go` (remember that the peer closed
first and do not answer twice). Four client changes inside the existing layering
(`lib -> state -> ui -> shell -> screens`). No wire change.

**Tech Stack:** Go 1.22+ with `github.com/gobwas/ws v1.4.0` (`internal/web`), React 19 +
TypeScript + Vitest + Testing Library under `internal/web/_ui`.

**Spec:** No written spec. Requirements are the second QA report in this session. The one
diagnosis worth restating: the browser's console error `Close received after close` is Chrome
reporting a second Close frame from the server. `readClientOps`' control handler
(`internal/web/ws.go:256-268`, `wsutil.ControlHandler`) already answers the client's Close
with a Close, and `wsConn.close()` (`ws.go:188`) then writes another. The earlier client-side
"defer the close until open" change was harmless but was not the fix.

## Global Constraints

- No code comments unless they explain hidden logic; then one or two lines.
- Line length: Go and TS up to 120 characters; Markdown up to 100 with hard breaks.
- Hyphens only, never em dashes, in code, docs, tests and commit messages.
- After UI edits run `make web-check` (eslint, tsc, vitest); it must be clean. Never run
  `npx prettier` in `internal/web/_ui`.
- After Go edits run `gofmt -l internal cmd`, `go test ./...`, `golangci-lint run`.
- Every task ends deployed (`make web && make install && systemctl --user restart aigem-web`,
  then `curl -s https://tba.tail74d52.ts.net/healthz` answers `{"ok":true}`) and checked
  headlessly. Playwright is installed in the session scratchpad, under
  `/tmp/claude-1000/-home-gigovich-work-devinlab-aigem/`
  `a9f61633-f8a4-4ee7-adb0-2d2af0d3a9af/scratchpad/pw` (one path, split here), with a
  signed-in `state.json`; launch with `chromium.launch({ channel: 'chrome' })`. If it is gone,
  `npm i playwright` in a scratch directory and sign in once by opening
  `https://tba.tail74d52.ts.net/?token=<token>` (token from
  `journalctl --user -u aigem-web --no-pager | grep -o 'token=[^ ]*' | tail -1 | cut -d= -f2`),
  then save `storageState`.
- The daemon runs from `~/work`. Close any run a check creates:
  `DELETE https://tba.tail74d52.ts.net/api/runs/{id}` with `Authorization: Bearer <token>`.
- Commit on `main` and `git push origin main` at the end of every task.

---

### Task 1: One Close frame per connection

**Files:**
- Modify: `internal/web/ws.go` (`wsConn` struct at lines 71-82, `readClientOps` control
  closure at 254-269, `close()` at 176-191)
- Test: `internal/web/runsocket_test.go` (the `dialRunSocket`/`controlClient` helpers exist;
  `controlClient` holds `conn net.Conn` and `read io.Reader`, see `control_test.go:50-70`)

**Interfaces:**
- Consumes: `wsutil.ControlHandler{...}.Handle(h)` returns `wsutil.ClosedError` when the frame
  it handled was a Close (gobwas `wsutil` documents this; `runsocket_test.go:514` already
  matches on it with `errors.As`).
- Produces: `wsConn.peerClosed atomic.Bool`; `close()` writes its Close frame only when the
  peer has not already sent one.

- [ ] **Step 1: Write the failing test**

Append to `internal/web/runsocket_test.go`:

```go
// A browser that leaves a run sends one Close frame and expects one back. A
// second one from the daemon is what Chrome reports as "Close received after
// close" on every run switch.
func TestAClientCloseIsAnsweredWithExactlyOneCloseFrame(t *testing.T) {
	b := &fakeBackend{}
	srv := newTestServer(t, Config{Backend: b})
	id := openRun(t, srv)
	c := dialRunSocket(t, srv, id, "")

	closing := ws.NewCloseFrame(ws.NewCloseFrameBody(ws.StatusNormalClosure, ""))
	closing = ws.MaskFrameInPlace(closing)
	if err := ws.WriteFrame(c.conn, closing); err != nil {
		t.Fatal(err)
	}

	if err := c.conn.SetReadDeadline(time.Now().Add(testWait)); err != nil {
		t.Fatal(err)
	}
	var closes int
	for {
		hdr, err := ws.ReadHeader(c.read)
		if err != nil {
			break
		}
		if hdr.OpCode == ws.OpClose {
			closes++
		}
		if _, err := io.CopyN(io.Discard, c.read, hdr.Length); err != nil {
			break
		}
	}
	if closes != 1 {
		t.Fatalf("the daemon sent %d close frames after the client's, want exactly 1", closes)
	}
}
```

Add `io` to the test file's imports if it is not there. The loop ends when the daemon closes
the TCP connection (`ReadHeader` returns an error), which `close()` does right after its frame.

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./internal/web/ -run TestAClientCloseIsAnswered -count=1 -v`

Expected: FAIL with `the daemon sent 2 close frames after the client's, want exactly 1`.

If it reports 1 already, the control handler's reply is not reaching the wire in this path;
read `readClientOps` again and write down which of the two writes actually happens before
changing anything - the fix below must leave exactly one.

- [ ] **Step 3: Remember that the peer closed first**

In `internal/web/ws.go`:

1. Add `"sync/atomic"` to the imports. In the `wsConn` struct, after `mu sync.Mutex`, add:

```go
	// peerClosed is set once the client has sent its own Close frame. The
	// control handler answers that frame; close() must not answer it again.
	peerClosed atomic.Bool
```

2. In `readClientOps`, the control closure returns `err` at its end. Before that `return err`,
   add:

```go
		var closed wsutil.ClosedError
		if errors.As(err, &closed) {
			c.peerClosed.Store(true)
		}
```

   (`errors` is already imported by the package; check the file's import block and add it if
   this file lacks it.)

3. In `close()`, wrap the frame write:

```go
	if !c.peerClosed.Load() {
		_ = ws.WriteFrame(c.conn, ws.NewCloseFrame(ws.NewCloseFrameBody(ws.StatusNormalClosure, "")))
	}
```

- [ ] **Step 4: Run the test, then the package and lint**

Run: `go test ./internal/web/ -run TestAClientCloseIsAnswered -count=1 -v`

Expected: PASS.

Run: `gofmt -l internal cmd && golangci-lint run && go test ./internal/web/ ./cmd/aigem/`

Expected: clean, `0 issues.`, `ok`. `TestAClosingRunEndsItsSocketsWithoutCuttingAFrame` and the
disconnect tests must still pass: a daemon-initiated close still writes its one frame.

- [ ] **Step 5: Deploy and look**

Deploy per Global Constraints. Headless check with the scratchpad playwright: create a run
over the API (`POST /api/runs` with the bearer token and `{}` body), open
`/chat/<that id>`, wait two seconds, click a different session row in the list, wait a
second, and collect `page.on('console')` errors: none must contain `WebSocket`. Repeat once
clicking immediately after `page.goto(..., { waitUntil: 'commit' })`. Then close the run over
the API.

- [ ] **Step 6: Commit and push**

```bash
git add internal/web/ws.go internal/web/runsocket_test.go
git commit -m "fix(web): answer a client's close frame once"
git push origin main
```

---

### Task 2: A skill can be run from its page on a phone

On a phone the skill detail fills the screen and the inspector, which holds "Run in a
session", starts closed. The detail header gets the action when the layout is a phone's.

**Files:**
- Modify: `internal/web/_ui/src/screens/Skills.tsx:196-201` (the detail header row)
- Test: `internal/web/_ui/src/screens/screens.test.tsx`

**Interfaces:**
- Consumes: `compose(text)` from `@/state/app` (already imported in `Skills.tsx`); `phone` from
  the store (already read at `Skills.tsx:36`); `chosen.userInvocable`.

- [ ] **Step 1: Write the failing test**

Append to `internal/web/_ui/src/screens/screens.test.tsx` (it imports `mountApp`, `RUN`,
`setViewport`, `act`, `screen`, `waitFor`, `within`, `userEvent`, `navigate`, and defines
`SKILLS` and `go`):

```ts
// The inspector holds the action on a wide screen; on a phone the skill's own
// page is all there is, so the action has to be on it.
test('on a phone a skill can be run from its page', async () => {
  const user = userEvent.setup()
  await mountApp({ runs: [RUN], skills: SKILLS })
  act(() => setViewport(400))
  act(() => navigate({ screen: 'skills', id: 'code-review' }))
  await user.click(await screen.findByRole('button', { name: 'Run in a session' }))
  await waitFor(() => expect(window.location.pathname).toBe('/chat'))
  expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('/skill:code-review ')
})

test('on a wide screen the action stays in the inspector alone', async () => {
  await mountApp({ runs: [RUN], skills: SKILLS })
  go('skills')
  await screen.findByRole('heading', { name: 'code-review', level: 1 })
  const aside = screen.getByRole('complementary', { name: 'Inspector' })
  expect(within(aside).getByRole('button', { name: 'Run in a session' })).toBeInTheDocument()
  expect(screen.getAllByRole('button', { name: 'Run in a session' })).toHaveLength(1)
})
```

- [ ] **Step 2: Run them to make sure they fail**

Run:

```sh
cd internal/web/_ui && npx vitest run src/screens/screens.test.tsx -t "run from its page|inspector alone"
```

Expected: the first FAILS (no such button at 400px; the inspector is closed); the second passes.

- [ ] **Step 3: Put the action on the page for a phone**

In `internal/web/_ui/src/screens/Skills.tsx`, in the detail header's
`<div className="flex flex-wrap items-center gap-[10px]">`, after the `<StatusChip ... />`:

```tsx
                  {phone && chosen.userInvocable && (
                    <button
                      type="button"
                      onClick={() => compose(`/skill:${chosen.name} `)}
                      className="ml-auto h-[26px] cursor-pointer rounded-md border border-line px-[10px] text-[11.5px] text-fg-muted hover:border-line-strong hover:text-fg"
                    >
                      Run in a session
                    </button>
                  )}
```

- [ ] **Step 4: Run the tests, then everything**

Run: `cd internal/web/_ui && npx vitest run src/screens/screens.test.tsx` then `make web-check`

Expected: PASS, clean.

- [ ] **Step 5: Deploy, look, commit, push**

Deploy. Headlessly at 400x800 open `/skills/aigem-deploy`: the button is visible; tap it: the
URL is `/chat` and the composer holds `/skill:aigem-deploy `.

```bash
git add internal/web/_ui/src
git commit -m "fix(web): let a phone run a skill from the skill's own page"
git push origin main
```

---

### Task 3: A carriage return stays inside its line, and counts read right

`split()` in `src/lib/diff.ts:112-117` keeps a lone `\r` inside a line on purpose, but
`DiffView.tsx:73` draws lines with `whitespace-pre-wrap`, which the browser breaks on `\r`. The
line is one diff line and two visual rows. Draw the character as its symbol instead. And the
run header says "1 files" and "1 events".

**Files:**
- Modify: `internal/web/_ui/src/ui/DiffView.tsx:73` (where a line's text is rendered)
- Modify: `internal/web/_ui/src/screens/Run.tsx:166`
- Test: `internal/web/_ui/src/screens/Changes.test.tsx`,
  `internal/web/_ui/src/screens/screens.test.tsx`

**Interfaces:**
- Consumes: nothing new. `count()` in `src/lib/format.ts:6` only groups digits and knows no
  nouns, so the plural is written inline the way `Run.tsx:236` already does.

- [ ] **Step 1: Write the failing tests**

Append to `internal/web/_ui/src/screens/Changes.test.tsx` (follow the file's existing
pattern for mounting a run and opening the Changes segment; copy the setup of "draws a diff of
what the run changed"):

```ts
// A lone carriage return is a character inside a line - a progress bar's
// output - and the browser must not turn it into a second row.
test('a carriage return inside a line is drawn as a symbol, on one row', async () => {
  // ...same mount as "draws a diff of what the run changed", with the artifact
  // { path: '/w/p.log', old: '', new: 'step 1\rstep 2\n', oldBytes: 0, newBytes: 14 }
  const line = await screen.findByText(/step 1/)
  expect(line.textContent).toBe('step 1␍step 2')
})
```

Append to `internal/web/_ui/src/screens/screens.test.tsx`:

```ts
test('the run header counts in the singular when there is one', async () => {
  const h = await mountApp({ runs: [RUN] })
  go('run')
  act(() => navigate({ screen: 'run', id: 'r-1' }))
  await waitFor(() => expect(h.runSocket()).toBeTruthy())
  h.runSocket()?.open()
  h.emit({ seq: 1, time: '2026-09-08T12:00:01Z', kind: EventKind.TurnStart })
  expect(await screen.findByText('1 event')).toBeInTheDocument()
})
```

- [ ] **Step 2: Run them to make sure they fail**

Run:

```sh
cd internal/web/_ui && npx vitest run src/screens/Changes.test.tsx src/screens/screens.test.tsx -t "carriage|singular"
```

Expected: both FAIL (`step 1\rstep 2` is rendered raw; the header says `1 events`).

- [ ] **Step 3: Draw the symbol and count properly**

In `internal/web/_ui/src/ui/DiffView.tsx:76`, the `whitespace-pre-wrap` element renders
`{text}`; render `{text.replace(/\r/g, '␍')}` (U+240D) instead. That is the one edit.

In `internal/web/_ui/src/screens/Run.tsx:166`, replace
`` {view === 'events' ? `${rows.length} events` : `${run.files.length} files`} `` with the
pluralised form `` `${rows.length} event${rows.length === 1 ? '' : 's'}` `` and the same for
files. Line 236 already pluralises; leave it.

- [ ] **Step 4: Run the tests, then everything**

Run:

```sh
cd internal/web/_ui && npx vitest run src/screens/Changes.test.tsx src/screens/screens.test.tsx` then `make web-check
```

Expected: PASS, clean.

- [ ] **Step 5: Deploy, look, commit, push**

Deploy. Headlessly open `/run/RUN-12` (a closed run with one changed file) and click Changes:
the header reads `1 file`.

```bash
git add internal/web/_ui/src
git commit -m "fix(web): keep a carriage return on its line, and count one file as one"
git push origin main
```

---

### Task 4: The drawer keeps Tab inside it

**Files:**
- Modify: `internal/web/_ui/src/App.tsx:234-246` (the drawer markup) and the focus effect at
  `:143-156`
- Test: `internal/web/_ui/src/shell/layout.test.tsx`

- [ ] **Step 1: Write the failing test**

Append to `internal/web/_ui/src/shell/layout.test.tsx`:

```ts
// A drawer over the page is a dialog: Tab cycles inside it and never reaches
// what it covers.
test('Tab stays inside the open drawer', async () => {
  const user = userEvent.setup()
  await mountApp({ runs: [RUN] })
  act(() => setViewport(400))
  await user.click(screen.getByRole('button', { name: 'Open navigation' }))
  const nav = screen.getByRole('navigation', { name: 'Navigation' })
  const buttons = within(nav).getAllByRole('button')
  expect(screen.getByRole('dialog', { name: 'Navigation menu' })).toBeInTheDocument()

  buttons[buttons.length - 1]!.focus()
  await user.tab()
  expect(document.activeElement).toBe(buttons[0])

  await user.tab({ shift: true })
  expect(document.activeElement).toBe(buttons[buttons.length - 1])
})
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `cd internal/web/_ui && npx vitest run src/shell/layout.test.tsx -t "Tab stays"`

Expected: FAIL - no dialog named "Navigation menu"; focus leaves the drawer.

- [ ] **Step 3: Trap Tab in the drawer**

In `internal/web/_ui/src/App.tsx`, the drawer container becomes:

```tsx
            <div
              ref={drawer}
              role="dialog"
              aria-modal="true"
              aria-label="Navigation menu"
              onKeyDown={(e) => {
                if (e.key !== 'Tab') return
                const items = drawer.current?.querySelectorAll<HTMLElement>('button, [href], input, select, textarea')
                if (!items || items.length === 0) return
                const first = items[0]!
                const last = items[items.length - 1]!
                if (e.shiftKey && document.activeElement === first) {
                  e.preventDefault()
                  last.focus()
                } else if (!e.shiftKey && document.activeElement === last) {
                  e.preventDefault()
                  first.focus()
                }
              }}
              className="fixed inset-y-0 left-0 z-[65] flex w-[240px] shadow-panel"
            >
              {sidebar}
            </div>
```

The `querySelectorAll` string is over 120 characters on one line; break it across two with a
constant `const FOCUSABLE = 'button, [href], input, select, textarea'` declared above `App`.

- [ ] **Step 4: Run the tests, then everything**

Run:

```sh
cd internal/web/_ui && npx vitest run src/shell/layout.test.tsx src/a11y.test.tsx` then `make web-check
```

Expected: PASS, clean. The a11y test "every control on the shell has a name" must still
pass: the dialog carries `aria-label`.

- [ ] **Step 5: Deploy, look, commit, push**

Deploy. Headlessly at 400x800 open the drawer, press Tab as many times as there are buttons
plus one, and read `document.activeElement`: it is inside the `nav`.

```bash
git add internal/web/_ui/src
git commit -m "fix(web): keep Tab inside the phone drawer"
git push origin main
```

---

### Task 5: Review cycle and changelog

- [ ] **Step 1: Two reviewers on the whole range**

Dispatch two review subagents with worktree isolation over `git diff <commit before Task 1>..HEAD`:
one for code quality and simplicity, one for functionality, security and tests. Ask the
second to trace: a client that sends Close while the pump is mid-frame (the write lock and
`wsCloseWait` path in `close()`); a control-frame error that is not `ClosedError` (a bad ping)
must still get the daemon's own Close; a `\r\n` file still splits into lines (only a lone `\r`
becomes the symbol); the Tab trap when the drawer holds only one focusable element.

- [ ] **Step 2: Apply what they find, rerun `go test ./...` and `make web-check`**

- [ ] **Step 3: Changelog**

In `CHANGELOG.md`, the `Added` bullet beginning `- The browser UI runs the slash commands:`
gets one sentence before its final period, wrapped at 80, hyphens only:

```
A phone can run a skill from the skill's page, and the drawer keeps focus.
```

and under `### Fixed` (create the heading under `[Unreleased]` if it is missing):

```
- The web daemon answered a browser's Close frame twice, which Chrome logged as
  an error on every run switch.
```

- [ ] **Step 4: Deploy, commit, push**

```bash
make web && make install && systemctl --user restart aigem-web
git add -A
git commit -m "fix(web): close what the review cycle found; note the second QA round"
git push origin main
```

---

## Out of scope, on purpose

- Provider sign-in, "Make default" confirm and "Load them" were not exercised by QA and are
  not changed here.
- Projects phase two and a Stop run button: need a spec.
