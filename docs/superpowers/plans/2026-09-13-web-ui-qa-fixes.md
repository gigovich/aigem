# Web UI QA Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the seven defects the headless QA pass of 2026-09-13 found in the browser UI,
in the order a person hits them: unreadable long answers, invisible selection on a phone, an
idle run drawn as "Waiting", no file changes for a closed run, a console error on every run
switch, blank transcript rows, and a drawer that does not take focus.

**Architecture:** All client work stays inside the existing layering
(`lib -> state -> ui -> shell -> screens`). The one daemon change persists a session's file
changes into its journal directory so a closed run can still answer `/api/runs/{id}/artifacts`.
Nothing on the wire changes shape; one route stops answering 409 for closed runs.

**Tech Stack:** Go 1.22+ (`internal/uisession`, `internal/runner`, `internal/web`), React 19 +
TypeScript + Vite + Vitest + Testing Library under `internal/web/_ui`.

**Spec:** No written spec. Requirements are the QA report in this session (ranked list) and the
constraints below. The transcript-prose task is the one design call: prose rows wrap and render
Markdown; tool rows stay one line.

## Global Constraints

- No code comments unless they explain hidden logic; then one or two lines.
- Line length: Go and TS up to 120 characters; Markdown up to 100 with hard breaks.
- Hyphens only, never em dashes, in code, docs, tests and commit messages.
- After UI edits run `make web-check` (eslint, tsc, vitest); it must be clean. Never run
  `npx prettier` in `internal/web/_ui`: there is no config and its defaults rewrite every file.
- After Go edits run `gofmt -l internal cmd`, `go test ./...`, `golangci-lint run`.
- `src/state/run.ts` appends to arrays in place; never memoise on the identity of
  `view.events`, `view.rows`, `view.todos` or `view.files`.
- `src/ui/Markdown.tsx` renders React elements, never HTML; keep it that way.
- Every task ends deployed (`make web && make install && systemctl --user restart aigem-web`,
  then `curl -s https://tba.tail74d52.ts.net/healthz` answers `{"ok":true}`) and checked
  headlessly. Playwright is installed in the session scratchpad, under
  `/tmp/claude-1000/-home-gigovich-work-devinlab-aigem/`
  `a9f61633-f8a4-4ee7-adb0-2d2af0d3a9af/scratchpad/pw` (one path, split here),
  with a signed-in `state.json`; launch with `chromium.launch({ channel: 'chrome' })`. If it is
  gone, `npm i playwright` in a scratch directory and sign in once by opening
  `https://tba.tail74d52.ts.net/?token=<token>` (token from
  `journalctl --user -u aigem-web | grep -o 'token=[^ ]*' | tail -1`), then save `storageState`.
- The daemon runs from `~/work`; a run it opens has that root. Close any run a check creates
  (`DELETE /api/runs/{id}` with `Authorization: Bearer <token>`).
- Commit on `main` and `git push origin main` at the end of every task.

---

### Task 1: Prose rows in the transcript

Every transcript row is one ellipsised line (`src/ui/EventStream.tsx:59-68`), so a long answer
is unreadable and Markdown is shown raw. Rows that carry prose - what the person typed, what the
agent said, the answer a turn ended with - wrap and render Markdown. Tool rows, phase markers
and reasoning stay one line.

**Files:**
- Modify: `internal/web/_ui/src/state/eventrow.ts` (the `EventRow` type and three cases of `toRow`)
- Modify: `internal/web/_ui/src/ui/EventStream.tsx:59-68`
- Test: `internal/web/_ui/src/state/run.test.ts`, `internal/web/_ui/src/screens/chat.test.tsx`

**Interfaces:**
- Consumes: `Markdown({ source, className })` from `src/ui/Markdown.tsx:139`; `readable()` from
  `src/lib/text.ts`.
- Produces: `EventRow.prose: boolean` (true for `user_message`, `assistant_message`, and a
  `turn_end` that carries text).

- [ ] **Step 1: Write the failing row tests**

Append to `internal/web/_ui/src/state/run.test.ts` (the file already imports `toRow`,
`EventKind` and an `ev` helper):

```ts
test('the rows a person reads as prose say so; tool rows do not', () => {
  expect(toRow(ev(1, EventKind.UserMessage, { text: 'hello' }))?.prose).toBe(true)
  expect(toRow(ev(2, EventKind.AssistantMessage, { text: 'hi' }))?.prose).toBe(true)
  expect(toRow(ev(3, EventKind.TurnEnd, { text: 'Done: **two** files.' }))?.prose).toBe(true)
  expect(toRow(ev(4, EventKind.TurnEnd))?.prose).toBe(false)
  expect(toRow(ev(5, EventKind.ToolStart, { name: 'read_file' }))?.prose).toBe(false)
  expect(toRow(ev(6, EventKind.Reasoning, { text: 'thinking' }))?.prose).toBe(false)
})
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `cd internal/web/_ui && npx vitest run src/state/run.test.ts -t "prose"`

Expected: FAIL - `prose` is `undefined`, not `true`.

- [ ] **Step 3: Add `prose` to the row**

In `internal/web/_ui/src/state/eventrow.ts`:

1. In `export type EventRow`, after `mono: boolean`, add:

```ts
  /** Wraps and renders Markdown: what a person typed, what the agent said. */
  prose: boolean
```

2. In `toRow`, the `base` object gets `prose: false,` after `mono: false,`.

3. The `UserMessage` case gets `prose: true,` after `phase: true,`.

4. The `AssistantMessage` case becomes:

```ts
    case EventKind.AssistantMessage:
      return { ...base, glyph: '', color: MUTED, text: e.text ?? '', prose: true }
```

5. The `TurnEnd` case's final `return` becomes:

```ts
      return {
        ...base,
        glyph: e.interrupted ? '■' : '✓',
        color: e.interrupted ? MUTED : 'var(--success)',
        text: e.interrupted ? 'Interrupted' : readable(e.text?.trim() || 'Turn finished'),
        phase: true,
        prose: !e.interrupted && !!e.text?.trim(),
      }
```

- [ ] **Step 4: Run the row tests**

Run: `cd internal/web/_ui && npx vitest run src/state/run.test.ts`

Expected: PASS.

- [ ] **Step 5: Write the failing screen test**

Append to `internal/web/_ui/src/screens/chat.test.tsx`:

```ts
// A long answer is the ordinary case, and a line that ends in an ellipsis is
// not an answer anybody can read.
test('the answer a turn ends with wraps and renders its markdown', async () => {
  const h = await attached()
  h.emit({
    seq: 1,
    time: '2026-09-08T12:00:01Z',
    kind: EventKind.TurnEnd,
    text: 'Listed **83 entries**, including:\n\n- `devinlab/`\n- `docs/`',
  })
  const log = screen.getByRole('log', { name: 'Transcript' })
  const strong = await within(log).findByText('83 entries')
  expect(strong.tagName).toBe('STRONG')
  expect(within(log).getByRole('list')).toBeInTheDocument()
  expect(within(log).getByText('devinlab/').closest('[class*="whitespace-nowrap"]')).toBeNull()
})

test('a tool row stays one line', async () => {
  const h = await attached()
  h.emit({ seq: 1, time: '2026-09-08T12:00:01Z', kind: EventKind.ToolStart, name: 'read_file', args: { path: '/a/b' } })
  const row = await screen.findByText(/read_file/)
  expect(row.className).toContain('whitespace-nowrap')
})
```

- [ ] **Step 6: Run them to make sure they fail**

Run: `cd internal/web/_ui && npx vitest run src/screens/chat.test.tsx -t "wraps|one line"`

Expected: the first FAILS (no `STRONG`, the text is raw in a nowrap span); the second passes
already.

- [ ] **Step 7: Render prose rows through Markdown**

In `internal/web/_ui/src/ui/EventStream.tsx`, add `import { Markdown } from './Markdown'` and
replace the inner text span (the one with `overflow-hidden text-ellipsis whitespace-nowrap`)
with:

```tsx
            {r.prose ? (
              <Markdown
                source={r.text}
                className="min-w-0 max-w-[84ch] text-[12.5px] text-pretty"
              />
            ) : (
              <span
                className="overflow-hidden text-ellipsis whitespace-nowrap"
                style={{
                  fontFamily: r.mono ? 'var(--mono)' : 'var(--sans)',
                  fontSize: r.mono ? '11.5px' : '12.5px',
                  fontWeight: r.phase ? 500 : 400,
                  color: r.phase ? 'var(--fg)' : 'var(--fg-muted)',
                }}
              >
                {r.text}
              </span>
            )}
```

Read `src/ui/Markdown.tsx` first: if its root element is a `div`, the surrounding
`<span className="flex min-w-0 items-baseline gap-2">` must become a `div` with the same
classes (a `div` inside a `span` is invalid HTML and React warns). Make that change if needed;
the `meta` and `show all` siblings keep working.

The user row's colour: `Markdown` draws in the page foreground. If the user line should keep
`var(--fg)` weight 500 as today, wrap it: pass `className` with `font-medium` when `r.phase`.

- [ ] **Step 8: Run the screen tests, then everything**

Run: `cd internal/web/_ui && npx vitest run src/screens/chat.test.tsx`

Expected: PASS.

Run: `make web-check`

Expected: clean. The a11y test `every announcement region is present` and the screens tests
render the stream; if one asserts on a nowrap class for a prose row, update it to the new
contract and say so in the commit body.

- [ ] **Step 9: Deploy and look**

Deploy per Global Constraints. Then, with the scratchpad playwright, open
`https://tba.tail74d52.ts.net/run/RUN-4` (the run whose last line reads "Directory listed
successfully..."), screenshot at 1280x800, and confirm the last row wraps onto several lines
with a rendered list. No console errors.

- [ ] **Step 10: Commit and push**

```bash
git add internal/web/_ui/src
git commit -m "feat(web): wrap the prose rows of a transcript and render their markdown"
git push origin main
```

---

### Task 2: Selecting a row on a phone opens the inspector

Below 1120px the inspector starts closed (`src/state/app.ts` `applyWidth`). On a phone,
tapping a model or activity row publishes a panel nobody sees, so "Make default", "Sign in" and
"Open run" are unreachable without the header toggle.

**Files:**
- Modify: `internal/web/_ui/src/state/app.ts` (one new action beside `setInspector`)
- Modify: `internal/web/_ui/src/screens/Models.tsx:183` (`onSelect`)
- Modify: `internal/web/_ui/src/screens/Activity.tsx` (the row button's `onClick`)
- Test: `internal/web/_ui/src/shell/layout.test.tsx`

**Interfaces:**
- Produces: `reveal()` in `src/state/app.ts`: opens the inspector when the layout is narrow.

- [ ] **Step 1: Write the failing test**

Append to `internal/web/_ui/src/shell/layout.test.tsx` (it imports `mountApp`, `RUN`,
`setViewport`, `act`, `screen`, `waitFor`, `within`, `userEvent`):

```ts
// A phone starts with the inspector closed; a selection is the moment it is
// wanted, and a tap that changes nothing visible reads as broken.
test('on a phone, selecting a row opens the inspector', async () => {
  const user = userEvent.setup()
  await mountApp({
    runs: [RUN],
    models: [
      {
        ref: 'openai/gpt-5',
        provider: 'openai',
        name: 'GPT-5',
        contextWindow: 400000,
        needsAuth: true,
        authenticated: true,
        default: true,
      },
    ],
    activity: [{ seq: 1, at: '2026-09-08T12:00:00Z', kind: 'run.opened', text: 'older', runRef: 'r-1' }],
  })
  act(() => setViewport(400))
  await user.click(screen.getByRole('button', { name: 'Open navigation' }))
  await user.click(within(screen.getByRole('navigation', { name: 'Navigation' })).getByRole('button', { name: /Models/ }))
  expect(screen.queryByRole('complementary', { name: 'Inspector' })).not.toBeInTheDocument()

  await user.click(await screen.findByRole('row', { name: /GPT-5/ }))
  const aside = await screen.findByRole('complementary', { name: 'Inspector' })
  expect(within(aside).getByRole('button', { name: 'Make default' })).toBeInTheDocument()

  await user.click(within(aside).getByRole('button', { name: 'Close inspector' }))
  await user.click(screen.getByRole('button', { name: 'Open navigation' }))
  await user.click(within(screen.getByRole('navigation', { name: 'Navigation' })).getByRole('button', { name: /Activity/ }))
  await user.click(await screen.findByRole('button', { name: /older/ }))
  expect(await screen.findByRole('complementary', { name: 'Inspector' })).toHaveTextContent('run.opened')
})
```

If `DataGrid` rows do not carry `role="row"` with the model name as accessible name, select the
row the way `src/screens/screens.test.tsx` "selecting a model fills the inspector" does and
copy that locator.

- [ ] **Step 2: Run it to make sure it fails**

Run: `cd internal/web/_ui && npx vitest run src/shell/layout.test.tsx -t "selecting a row"`

Expected: FAIL - the inspector is not in the document after the row click.

- [ ] **Step 3: Implement `reveal`**

In `internal/web/_ui/src/state/app.ts`, after `setInspector`:

```ts
/** A selection is the moment the panel is wanted where the layout closed it. */
export function reveal() {
  if (store.get().narrow) setInspector(true)
}
```

In `internal/web/_ui/src/screens/Models.tsx`, change the grid's `onSelect` to:

```tsx
        onSelect={(m) => {
          navigate({ screen: 'models', id: m.ref })
          reveal()
        }}
```

and add `reveal` to the `@/state/app` import.

In `internal/web/_ui/src/screens/Activity.tsx`, the row button's `onClick={() => setSelected(key)}`
becomes:

```tsx
                onClick={() => {
                  setSelected(key)
                  reveal()
                }}
```

and add `reveal` to the `@/state/app` import.

- [ ] **Step 4: Run the test, then everything**

Run: `cd internal/web/_ui && npx vitest run src/shell/layout.test.tsx`

Expected: PASS.

Run: `make web-check`

Expected: clean.

- [ ] **Step 5: Deploy and look**

Deploy. With playwright at viewport 400x800 (`isMobile: true`), open `/models`, tap the first
row, screenshot: the inspector sheet is over the page with "Make default". Tap "Close
inspector": it goes.

- [ ] **Step 6: Commit and push**

```bash
git add internal/web/_ui/src
git commit -m "fix(web): open the inspector when a row is chosen on a phone"
git push origin main
```

---

### Task 3: An idle session is "Idle", not "Waiting"

`liveStatus` (`src/state/run.ts:207-216`) and `runStatus` (`src/lib/wire.ts:329-334`) both
answer `waiting` for a live run with nothing in flight, and the dictionary draws `waiting` as
"Waiting" in warning yellow. A healthy run looks stuck.

**Files:**
- Modify: `internal/web/_ui/src/lib/wire.ts` (`StatusKey`, `STATUS`, `runStatus` and its comment)
- Modify: `internal/web/_ui/src/state/run.ts:214`
- Test: `internal/web/_ui/src/lib/wire.test.ts`, `internal/web/_ui/src/a11y.test.tsx:32-33`,
  `internal/web/_ui/src/state/run.test.ts`

**Interfaces:**
- Produces: `StatusKey` gains `'idle'`;
  `STATUS.idle = { label: 'Idle', icon: '○', color: 'var(--fg-subtle)' }`.

- [ ] **Step 1: Update the dictionary test first**

In `internal/web/_ui/src/lib/wire.test.ts`, add to `CANVAS` after `progress`:

```ts
  // Not in the canvas, which draws runs and has no state for a session that is
  // attached with nothing in flight. "Waiting" in warning yellow made a healthy
  // run look stuck.
  idle: { label: 'Idle', icon: '○', color: 'var(--fg-subtle)' },
```

Rename the test to `'the status dictionary is the canvas dictionary plus idle'`. Find the
`runStatus` tests in the same file: any assertion that a live, quiet run is `'waiting'` becomes
`'idle'`.

In `internal/web/_ui/src/a11y.test.tsx:32-33`, the comment and assertion become:

```ts
  // RUN is live with nothing in flight, which is "Idle".
  expect(list).toHaveTextContent('Idle')
```

In `internal/web/_ui/src/state/run.test.ts`, find the `liveStatus` test (search
`liveStatus(`); any expectation of `'waiting'` for a quiet run becomes `'idle'`. If none
exists, append:

```ts
test('a live run with events and nothing in flight is idle', () => {
  const view = applyAll(emptyRun(), [ev(1, EventKind.TurnStart), ev(2, EventKind.TurnEnd)])
  expect(liveStatus({ ...RUN_LIVE, live: true } as Run, view)).toBe('idle')
})
```

where `RUN_LIVE` is a `Run` literal with `id: 'r', mode: 'interactive', status: 'open',
created: '', updated: '', live: true` declared at the top of the test file if the file has no
run fixture yet.

- [ ] **Step 2: Run them to make sure they fail**

Run:

```sh
cd internal/web/_ui && npx vitest run src/lib/wire.test.ts src/a11y.test.tsx src/state/run.test.ts
```

Expected: FAIL - `idle` missing from `STATUS`, "Idle" not in the list, `liveStatus` says
`waiting`.

- [ ] **Step 3: Add the state**

In `internal/web/_ui/src/lib/wire.ts`:

1. Add `| 'idle'` to the `StatusKey` union.
2. Add to `STATUS` after `progress`:
   `idle: { label: 'Idle', icon: '○', color: 'var(--fg-subtle)' },`
3. `runStatus`'s last line `return 'waiting'` becomes `return 'idle'`, and the doc comment's
   last sentence becomes: "- the latter is the design's word for a run parked on something,
   while an open session with nothing in flight is `idle`."

In `internal/web/_ui/src/state/run.ts:214`, `if (view.seq > 0) return 'waiting'` becomes
`if (view.seq > 0) return 'idle'`.

- [ ] **Step 4: Run everything**

Run: `make web-check`

Expected: clean. `src/ui/StatusChip.tsx` needs no change: it indexes `STATUS`.

- [ ] **Step 5: Deploy, look, commit, push**

Deploy. Open `/chat` headlessly, create nothing; the closed runs read "Stopped". Open a new
session through the `New` button, confirm the chip reads "Idle" in a neutral colour, then close
that run over the API.

```bash
git add internal/web/_ui/src
git commit -m "fix(web): call an attached session with nothing in flight idle"
git push origin main
```

---

### Task 4: A closed run keeps its file changes

`Runs.Artifacts` (`internal/runner/runs.go:558-569`) answers `ErrRunClosed` once the session is
gone, because the changes live only in `Local.artifacts`. After a daemon restart no run's
changes can be viewed. The session writes them beside its journal whenever it saves, and a
closed run reads them back from there.

**Files:**
- Modify: `internal/uisession/journal.go` (a `putArtifacts` on `journal`, a `ReadArtifacts`
  beside `ReadBlob`)
- Modify: `internal/uisession/persist.go:74-83` (`Save`)
- Modify: `internal/runner/runs.go:558-569` (`Artifacts`)
- Modify: `internal/web/backend.go` (the comment on `RunArtifacts`, if it says closed runs 409)
- Modify: `docs/web.md:333`, `:364`, `:391` (the three places that say artifacts need the session)
- Modify: `internal/web/_ui/src/screens/Changes.tsx:30-53` (drop the closed-run early return)
- Test: `internal/uisession/journal_test.go`, `internal/runner/runs_test.go:200-214`,
  `internal/web/_ui/src/screens/Changes.test.tsx`

**Interfaces:**
- Produces: `func ReadArtifacts(id string) (map[string]tools.FileChange, error)` in
  `internal/uisession`; a missing file is `fs.ErrNotExist` (wrapped), which `Runs.Artifacts`
  turns into an empty map.

- [ ] **Step 1: Write the failing journal test**

Append to `internal/uisession/journal_test.go` (its other tests isolate the state directory
with `t.Setenv("XDG_STATE_HOME", t.TempDir())`; do the same):

```go
func TestArtifactsAreKeptBesideTheJournal(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	j, err := openJournal("20260913-000000-abcdef")
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	want := map[string]tools.FileChange{
		"/w/a.go": {Path: "/w/a.go", Old: "x", New: "y"},
		"/w/b.go": {Path: "/w/b.go", New: "z", Created: true},
	}
	if !j.putArtifacts(want) {
		t.Fatalf("putArtifacts failed: %v", j.err)
	}
	got, err := ReadArtifacts("20260913-000000-abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadArtifacts = %+v, want %+v", got, want)
	}
	if _, err := ReadArtifacts("20260913-000000-nothing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a session that never wrote artifacts = %v, want fs.ErrNotExist", err)
	}
}
```

Add `errors`, `io/fs`, `reflect` and `github.com/gigovich/aigem/internal/tools` to the test
file's imports as needed.

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./internal/uisession/ -run TestArtifactsAreKept -count=1`

Expected: compile error - `putArtifacts` and `ReadArtifacts` are undefined.

- [ ] **Step 3: Write and read the file**

In `internal/uisession/journal.go`, after `putBlob`:

```go
// putArtifacts replaces the record of what this session changed on disk. It
// is written whole and renamed into place, so a reader never sees half of it.
func (j *journal) putArtifacts(arts map[string]tools.FileChange) bool {
	if j == nil || !j.open {
		return false
	}
	body, err := json.Marshal(arts)
	if err != nil {
		j.note(err)
		return false
	}
	f, err := os.CreateTemp(j.dir, "artifacts-*.part")
	if err != nil {
		j.note(err)
		return false
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(body); err != nil {
		f.Close()
		j.note(err)
		return false
	}
	if err := f.Close(); err != nil {
		j.note(err)
		return false
	}
	if err := os.Rename(tmp, filepath.Join(j.dir, "artifacts.json")); err != nil {
		j.note(err)
		return false
	}
	return true
}

// ReadArtifacts returns what a session changed on disk, for a session that is
// closed or belongs to a daemon that has since restarted. A session that never
// wrote any is the underlying not-exist error.
func ReadArtifacts(id string) (map[string]tools.FileChange, error) {
	dir, err := journalDir(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "artifacts.json"))
	if err != nil {
		return nil, err
	}
	var out map[string]tools.FileChange
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("uisession: artifacts of %s: %w", id, err)
	}
	return out, nil
}
```

Add `"github.com/gigovich/aigem/internal/tools"` to the file's imports (check `local.go`
already imports it; `journal.go` may not).

- [ ] **Step 4: Run the journal test**

Run: `go test ./internal/uisession/ -run TestArtifactsAreKept -count=1`

Expected: PASS.

- [ ] **Step 5: Write them on every save**

In `internal/uisession/persist.go`, `Save` becomes:

```go
func (l *Local) Save() error {
	l.mu.Lock()
	if l.id == "" || l.ag == nil {
		l.mu.Unlock()
		return nil
	}
	s := &session.Session{Meta: l.metaLocked(), Messages: l.ag.Messages()}
	arts := make(map[string]tools.FileChange, len(l.artifacts))
	for k, v := range l.artifacts {
		arts[k] = v
	}
	j := l.journal
	l.mu.Unlock()
	j.putArtifacts(arts)
	return session.Save(s, time.Now())
}
```

`putArtifacts` on a nil journal is a no-op (it checks `j == nil`), which is what a session
without a state directory gets.

- [ ] **Step 6: Write the failing runner test**

In `internal/runner/runs_test.go`, `TestAClosedRunRefusesWhatNeedsItsSession` (line ~200)
currently expects `Artifacts` to return `ErrRunClosed`. Delete those three lines from that test
and append a new one:

```go
// The changes a run made are worth more after it is over than during it, and
// a daemon restart is the ordinary way a run ends.
func TestAClosedRunStillListsWhatItChanged(t *testing.T) {
	built := &lastSession{}
	runs := newRunsRecording(t, "", nil, nil, built)
	v := create(t, runs, runner.RunRequest{})
	// A turn against the dead backend fails at once, but it is what gives the
	// session an id and a journal to write beside.
	if err := runs.Apply(v.ID, runner.RunOp{Op: runner.OpSubmit, Text: "go"}); err != nil {
		t.Fatal(err)
	}
	l := built.get(t)
	for deadline := time.Now().Add(5 * time.Second); l.Running() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	l.RecordFileChange("/w/a.go", "x", "y", false)
	if err := runs.CloseRun(v.ID); err != nil {
		t.Fatal(err)
	}
	arts, err := runs.Artifacts(v.ID)
	if err != nil {
		t.Fatalf("Artifacts of a closed run: %v", err)
	}
	if c, ok := arts["/w/a.go"]; !ok || c.New != "y" {
		t.Errorf("Artifacts = %+v, want the recorded change", arts)
	}

	// One that never had a turn has nothing, and says so as an empty set.
	w := create(t, runs, runner.RunRequest{})
	if err := runs.CloseRun(w.ID); err != nil {
		t.Fatal(err)
	}
	if arts, err := runs.Artifacts(w.ID); err != nil || len(arts) != 0 {
		t.Errorf("Artifacts of a run with no turn = %+v, %v; want empty and no error", arts, err)
	}
}
```

`lastSession` (`runs_test.go:791`) and `newRunsRecording` (top of the file) exist; `time` may
need adding to the imports. The test must run under an isolated state directory - `TestMain` in
`testmain_test.go` does this for every runner test; confirm by reading it.

- [ ] **Step 7: Run it to make sure it fails**

Run: `go test ./internal/runner/ -run TestAClosedRunStillLists -count=1`

Expected: FAIL - `Artifacts` returns `ErrRunClosed`.

- [ ] **Step 8: Read from the journal for a closed run**

In `internal/runner/runs.go`, `Artifacts` becomes:

```go
func (r *Runs) Artifacts(id string) (map[string]tools.FileChange, error) {
	rec, sess, err := r.row(id)
	if err != nil {
		return nil, err
	}
	if sess != nil {
		return sess.Local.Artifacts(), nil
	}
	// The session is gone; what it changed was written beside its journal on
	// every save. A run that never had a turn has no journal and changed nothing.
	if rec.SessionID == "" {
		return map[string]tools.FileChange{}, nil
	}
	arts, err := uisession.ReadArtifacts(rec.SessionID)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]tools.FileChange{}, nil
	}
	return arts, err
}
```

`r.row` returns `(rec Run, sess *Session, err error)` - check its signature at the top of the
file and adjust the names. `io/fs` and `errors` are already imported in `runs.go`.

- [ ] **Step 9: Run the Go suites**

Run:

```sh
gofmt -l internal cmd && golangci-lint run && go test ./internal/uisession/ ./internal/runner/ ./internal/web/
```

Expected: clean, `0 issues.`, all `ok`. If `internal/web/api_runs_test.go` has a case asserting
409 for artifacts of a closed run, change it to expect 200 with `[]` and note it in the commit
body.

- [ ] **Step 10: Documentation**

In `docs/web.md`:
- Line ~333 (`409 - the run has no live session. Its timeline still reads; its artifacts and
  its socket do not ...`) becomes: "`409` - the run has no live session. Its timeline and its
  artifacts still read; its socket does not, because it lives with the session."
- Line ~364 (`the 409 belongs to the routes that need the session - the socket and the
  artifacts - and never to the record`) becomes: "the `409` belongs to the socket, which needs
  the session, and never to the record."
- The paragraph at ~391 about `GET /api/runs/{id}/artifacts` gets one sentence after its
  first: "A closed run answers from what its session wrote beside its journal on every save, so
  a run that ended with a daemon restart still lists what it changed; one that never had a turn
  answers `[]`."

Lines at most 100 chars, hard breaks, hyphens only. If `internal/web/backend.go`'s
`RunArtifacts` doc comment says closed runs are refused, correct it in one line.

Commit the daemon half:

```bash
git add internal docs/web.md
git commit -m "feat(uisession,runner,web): keep a run's file changes after its session ends"
```

- [ ] **Step 11: The Changes screen reads closed runs**

In `internal/web/_ui/src/screens/Changes.test.tsx`, the test
`a closed conversation says why it has no changes to show` (line 55) covers the closed-run
empty state. Replace it with one that mounts a closed run
(`{ ...RUN, status: 'closed', live: false }`), stubs
`'/api/runs/r-1/artifacts'` with one artifact
`[{ path: '/w/a.go', created: false, old: 'x\n', new: 'y\n', oldBytes: 2, newBytes: 2 }]`
(match the `Artifact` shape in `src/lib/wire.ts`), opens the run screen's Changes segment, and
asserts the path `/w/a.go` is shown. Follow the file's existing pattern for mounting and
switching to the Changes segment.

Run: `cd internal/web/_ui && npx vitest run src/screens/Changes.test.tsx`

Expected: FAIL - the closed-run empty state is drawn instead.

Then in `internal/web/_ui/src/screens/Changes.tsx`: delete the `if (!live) return` inside the
effect and its comment, delete the `if (!live) { return <EmptyState ... /> }` block, keep the
`live` prop in the signature only if something else uses it - if nothing does, remove it from
`Changes` and from the call in `src/screens/Run.tsx`, where
`<Changes runId={runId} live={record.live} seq={run.writes} />` becomes
`<Changes runId={runId} seq={run.writes} />`.

Run: `cd internal/web/_ui && npx vitest run src/screens/Changes.test.tsx` then `make web-check`

Expected: PASS, clean.

- [ ] **Step 12: Deploy, look, commit, push**

Deploy. Headlessly open `/run/RUN-4` (closed; it listed a directory and changed nothing) and
click Changes: it says "This run has not changed any files." rather than "closed". Then open a
new session, send `Create a file named qa-touch.txt containing the word ok in the current
directory, then stop.`, wait for the turn, close the run over the API, reload `/run/<id>` and
click Changes: `qa-touch.txt` is listed with its content. Delete `~/work/qa-touch.txt`
afterwards. This costs one model turn.

```bash
git add internal/web/_ui/src
git commit -m "feat(web): show what a closed run changed"
git push origin main
```

---

### Task 5: No console error when leaving a run that is still connecting

`connectRun`'s `close()` (`src/lib/socket.ts:196-203`) calls `ws.close()` on a socket whose
handshake has not finished. Chrome logs "WebSocket connection ... failed: Close received after
close" on every run switch.

**Files:**
- Modify: `internal/web/_ui/src/lib/socket.ts:196-203`
- Test: `internal/web/_ui/src/lib/socket.test.ts`

- [ ] **Step 1: Write the failing test**

`FakeSocket` (`src/test/fakesocket.ts`) has `open()`, `drop()` and a `readyState` that its
`close()` sets to `FakeSocket.CLOSED`. Append to `src/lib/socket.test.ts`:

```ts
// Chrome reports a close on a socket still connecting as an error on the
// console. Leaving a run before its handshake finished is the ordinary way to
// reach it, so the close waits for the open.
test('closing while the handshake is in flight closes after it opens', async () => {
  stubFetch((path) => (path.includes('/events') ? ok([]) : ok({ id: 'r1', live: true })))
  const { conn } = attach(0)
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
  const ws = FakeSocket.last
  conn.close()
  expect(ws.readyState).toBe(FakeSocket.CONNECTING)
  ws.open()
  expect(ws.readyState).toBe(FakeSocket.CLOSED)
})
```

If `FakeSocket.open()` does not invoke `onopen`, make it do so (read the method); the deferred
close relies on the browser's own `onopen`.

- [ ] **Step 2: Run it to make sure it fails**

Run: `cd internal/web/_ui && npx vitest run src/lib/socket.test.ts -t "handshake"`

Expected: FAIL at the first assertion - today the close is immediate and the state is CLOSED.

- [ ] **Step 3: Defer the close until the socket is open**

In `internal/web/_ui/src/lib/socket.ts`, `close` becomes:

```ts
    close: () => {
      stopped = true
      if (timer) clearTimeout(timer)
      timer = null
      const ws = socket
      socket = null
      if (!ws) return
      if (ws.readyState === WebSocket.CONNECTING) ws.onopen = () => ws.close()
      else ws.close()
    },
```

`ws.onclose` already returns early when `socket !== ws`, so the deferred close reports nothing.

- [ ] **Step 4: Run the socket tests, then everything**

Run: `cd internal/web/_ui && npx vitest run src/lib/socket.test.ts` then `make web-check`

Expected: PASS, clean.

- [ ] **Step 5: Deploy, look, commit, push**

Deploy. Headlessly open `/chat`, click three different session rows in a row while collecting
`page.on('console')` errors: none.

```bash
git add internal/web/_ui/src
git commit -m "fix(web): close a run socket after it opens, not during its handshake"
git push origin main
```

---

### Task 6: No blank transcript rows

RUN-4 shows rows that carry only a timestamp. They are `assistant_message` events with empty
text, which the model emits before a tool call.

**Files:**
- Modify: `internal/web/_ui/src/state/eventrow.ts` (the `AssistantMessage` case)
- Test: `internal/web/_ui/src/state/run.test.ts`

- [ ] **Step 1: Confirm the cause, then write the failing test**

Run against the live daemon (token as in Global Constraints):

```sh
curl -s -H "Authorization: Bearer $T" 'https://tba.tail74d52.ts.net/api/runs/RUN-4/events' \
  | python3 -c "import sys,json; [print(e['seq'], e['kind'], repr(e.get('text',''))[:40]) for e in json.load(sys.stdin) if e['kind'] in ('assistant_message','content','reasoning') and not (e.get('text') or '').strip()]"
```

Expected: several `assistant_message` lines with `''`. If instead the empty rows are another
kind, treat that kind the same way below and say so in the commit body.

Append to `src/state/run.test.ts`:

```ts
test('an assistant message with nothing in it is not a row', () => {
  expect(toRow(ev(1, EventKind.AssistantMessage, { text: '' }))).toBeNull()
  expect(toRow(ev(2, EventKind.AssistantMessage, { text: '  \n' }))).toBeNull()
  expect(toRow(ev(3, EventKind.AssistantMessage, { text: 'hi' }))).not.toBeNull()
})
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `cd internal/web/_ui && npx vitest run src/state/run.test.ts -t "nothing in it"`

Expected: FAIL - a row with empty text is returned.

- [ ] **Step 3: Drop the empty message**

In `eventrow.ts`, the `AssistantMessage` case becomes:

```ts
    case EventKind.AssistantMessage:
      if (!e.text?.trim()) return null
      return { ...base, glyph: '', color: MUTED, text: e.text, prose: true }
```

- [ ] **Step 4: Run everything, deploy, look, commit, push**

Run: `make web-check`. Expected: clean.

Deploy. Headlessly open `/run/RUN-4` and count rows whose text cell is empty: zero.

```bash
git add internal/web/_ui/src
git commit -m "fix(web): leave an empty assistant message out of the transcript"
git push origin main
```

---

### Task 7: The phone drawer takes focus and gives it back

**Files:**
- Modify: `internal/web/_ui/src/App.tsx` (the drawer block at the `phone && navOpen` branch)
- Test: `internal/web/_ui/src/shell/layout.test.tsx`

- [ ] **Step 1: Write the failing test**

Append to `src/shell/layout.test.tsx`:

```ts
test('the drawer takes focus when it opens and gives it back when it closes', async () => {
  const user = userEvent.setup()
  await mountApp({ runs: [RUN] })
  act(() => setViewport(400))
  const menu = screen.getByRole('button', { name: 'Open navigation' })
  await user.click(menu)
  const nav = screen.getByRole('navigation', { name: 'Navigation' })
  await waitFor(() => expect(nav.contains(document.activeElement)).toBe(true))
  await user.keyboard('{Escape}')
  await waitFor(() => expect(document.activeElement).toBe(menu))
})
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `cd internal/web/_ui && npx vitest run src/shell/layout.test.tsx -t "takes focus"`

Expected: FAIL - focus stays on the menu button after opening.

- [ ] **Step 3: Move focus in an effect**

In `internal/web/_ui/src/App.tsx`, next to the other hooks, add:

```ts
  const drawer = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!phone) return
    if (navOpen) {
      drawer.current?.querySelector<HTMLElement>('button')?.focus()
      return
    }
    document.querySelector<HTMLElement>('[aria-label="Open navigation"]')?.focus()
  }, [phone, navOpen])
```

and put `ref={drawer}` on the drawer container, the `div` whose class list begins
`fixed inset-y-0 left-0 z-[65] flex w-[240px]`.
`useRef` is already imported. The effect's close branch also runs on the very first render at
phone width (navOpen false); focusing the menu button then is harmless, but if a test in
`layout.test.tsx` asserts the initial focus, guard with a `useRef(false)` "was open" flag and say
so in the commit body.

- [ ] **Step 4: Run everything, deploy, commit, push**

Run: `make web-check`. Expected: clean. Deploy.

```bash
git add internal/web/_ui/src
git commit -m "fix(web): give the phone drawer focus and hand it back"
git push origin main
```

---

### Task 8: Review cycle and changelog

- [ ] **Step 1: Two reviewers on the whole range**

Dispatch two review subagents with worktree isolation (they stash otherwise) over
`git diff <commit before Task 1>..HEAD`: one for code quality and simplicity, one for
functionality, security and tests. Ask the first whether `EventStream`'s prose branch and the
`Rows` component could share a Markdown-capable row, and whether `reveal()` belongs in
`usePublishInspector` instead (answer expected: no - publishing happens on mount too). Ask the
second to trace: a `turn_end` whose text is a 200 KB tool dump (does `Markdown` cope; is the
row height a scroll problem); `ReadArtifacts` on a journal directory a previous version of the
daemon wrote (no `artifacts.json`: must be `[]`, not 500); `putArtifacts` racing `Save` from two
turns; the deferred socket close when the handshake fails instead of opening (the socket must
not leak - `onclose` fires, nothing dials again because `stopped` is true).

- [ ] **Step 2: Apply what they find, rerun `make web-check` and `go test ./...`**

- [ ] **Step 3: Changelog**

In `CHANGELOG.md`, the `Added` bullet that begins `- The browser UI runs the slash commands:`
gets these sentences before its final period:

```
Prose in the transcript wraps and renders its markdown. A closed run still
lists what it changed, from a record its session keeps beside the journal. An
attached session with nothing in flight is "Idle". On a phone, choosing a row
opens the inspector.
```

Wrap at 80 characters like the surrounding bullets, hyphens only.

- [ ] **Step 4: Deploy, commit, push**

```bash
make web && make install && systemctl --user restart aigem-web
git add -A
git commit -m "fix(web): close what the review cycle found; note the QA fixes"
git push origin main
```

---

## Out of scope, on purpose

- **Paste with both text and an image drops the text.** Rare; needs caret insertion by hand.
- **`media_type` unvalidated in the daemon**, PNG transparency to black, the three names on the
  attach control: parked from the day-two review, unchanged here.
- **Projects phase two** and a **Stop run** button: need a spec.
