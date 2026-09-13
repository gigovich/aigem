# Web UI Day Two Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the three remaining gaps in the browser UI that need no design spec: a test
for the skill command family, a plan panel for the agent's todo list, and image attachments
in the composer.

**Architecture:** The daemon already folds todo events and already accepts `images` on the
run socket's `submit` op; only the websocket frame cap and the client are missing. The client
keeps its layering (`lib -> state -> ui -> shell -> screens`): image reading goes in `lib`,
the composer change in the chat screen, the plan panel in the chat screen's inspector content.

**Tech Stack:** Go 1.22+ (`internal/web`, `internal/runner`), React 19 + TypeScript + Vite +
Vitest + Testing Library under `internal/web/_ui`.

**Spec:** No written spec. This plan follows the day-one batch merged to `main` on
2026-09-13 (commits `a06480a..b5fd212`) and the audit that produced it. Projects phase two
(tickets, worktrees, tasks) is deliberately NOT here: it is architectural and needs a
brainstormed spec first.

## Global Constraints

- No code comments unless they explain hidden logic; then one or two lines.
- Line length: Go and TS up to 120 characters; Markdown up to 100 with hard breaks.
- Hyphens only, never em dashes, in code, docs and commit messages.
- After UI edits run `make web-check` (eslint, tsc, vitest). Never run `npx prettier` in
  `internal/web/_ui`: there is no prettier config and its defaults rewrite every file.
- After Go edits run `gofmt -l internal cmd`, `go test ./...`, `golangci-lint run`.
- Every task ends deployed and checked in a headless browser against the live daemon:
  `make web && make install && systemctl --user restart aigem-web`, then drive
  `https://tba.tail74d52.ts.net` with the playwright scripts described in Task 3 Step 12.
- Commit on `main` and `git push origin main` at the end of every task.
- `src/state/run.ts` appends to arrays in place; never memoise on the identity of
  `view.events`, `view.rows`, `view.todos` or `view.files` - memoise on `seq`.
- `src/ui/Markdown.tsx` never renders HTML; do not introduce `dangerouslySetInnerHTML`.

---

### Task 1: Prove the skill command family resolves a real skill

**Files:**
- Test: `internal/runner/commands_test.go`
- Read only: `internal/runner/commands.go`, `internal/runner/env_test.go:46-55` (the
  `writeSkill` helper), `internal/runner/env_test.go:238` (how a test loads an environment
  with `TrustProjectSkills: true`).

**Interfaces:**
- Consumes: `(*Session).HandleCommands(skills *skill.Registry, m *mcp.Manager)` from
  `internal/runner/commands.go`; `(*uisession.Local).Command(name, args string) error`;
  `(*uisession.Local).Replay(since uint64) ([]uisession.Event, error)`.
- Produces: nothing new. This task is coverage the second review named as missing.

- [ ] **Step 1: Write the failing test**

Append to `internal/runner/commands_test.go`:

```go
// "skill:greet" is not registered by name; it reaches the family handler,
// which looks the skill up when invoked and starts a turn shown as the
// command the person typed.
func TestASkillCommandStartsATurnUnderItsOwnName(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: greet\n---\nsay hello\n")
	env, _ := load(t, runner.Options{Cwd: cwd, TrustProjectSkills: true})
	reg, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	s := runner.NewSession(runner.Spec{Tools: reg, Backend: deadBackend(), Skills: env.Skills})
	t.Cleanup(s.Local.Close)
	s.HandleCommands(env.Skills, env.MCP)

	if err := s.Local.Command("skill:greet", "politely"); err != nil {
		t.Fatalf("skill:greet: %v", err)
	}
	evs, err := s.Local.Replay(0)
	if err != nil {
		t.Fatal(err)
	}
	var shown bool
	for _, ev := range evs {
		if ev.Kind == uisession.KindUserMessage && ev.Text == "/skill:greet politely" {
			shown = true
		}
	}
	if !shown {
		t.Errorf("no user line carrying the command, events: %+v", evs)
	}
}

// A skill only the model may invoke is not a command a person can run.
func TestAModelOnlySkillIsNotACommand(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "quiet",
		"---\nname: quiet\ndescription: quiet\nuser-invocable: false\n---\nnothing\n")
	env, _ := load(t, runner.Options{Cwd: cwd, TrustProjectSkills: true})
	reg, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	s := runner.NewSession(runner.Spec{Tools: reg, Backend: deadBackend(), Skills: env.Skills})
	t.Cleanup(s.Local.Close)
	s.HandleCommands(env.Skills, env.MCP)

	err = s.Local.Command("skill:quiet", "")
	if err == nil || !strings.Contains(err.Error(), "no such skill") {
		t.Errorf("a model-only skill = %v, want to be refused as no such skill", err)
	}
}
```

- [ ] **Step 2: Run the tests to see whether they pass or fail**

Run: `go test ./internal/runner/ -run 'TestASkillCommand|TestAModelOnlySkill' -count=1 -v`

Expected: both PASS. These are coverage tests for code that exists; if the first FAILS with
"no such skill: greet", the project skill was not trusted - check that `load` was given
`TrustProjectSkills: true` and that `writeSkill` wrote under `.skills/greet/SKILL.md`. If the
user line is missing, the turn started with a different display string - read
`internal/runner/commands.go` and match the test to the display it builds
(`"/skill:" + name + " " + args`).

- [ ] **Step 3: Lint and the whole suite**

Run:

```sh
gofmt -l internal cmd && golangci-lint run && go test ./internal/runner/ ./internal/uisession/
```

Expected: no output from gofmt, `0 issues.`, `ok` for both packages.

- [ ] **Step 4: Commit and push**

```bash
git add internal/runner/commands_test.go
git commit -m "test(runner): prove the skill command family resolves a real skill"
git push origin main
```

---

### Task 2: The plan panel

The daemon publishes a `todo` event carrying the agent's whole working plan every time it
changes, and `src/state/run.ts:141` already folds it into `view.todos`. Nothing draws it: the
transcript shows one line "Plan updated - N items". This task draws the plan in the chat
screen's inspector, above the files the run touched.

**Files:**
- Modify: `internal/web/_ui/src/state/inspector.ts` (the `InspectorContent` type: add a
  second optional list so a screen can publish a plan and a file list)
- Modify: `internal/web/_ui/src/shell/Inspector.tsx` (draw the second list)
- Modify: `internal/web/_ui/src/screens/Chat.tsx:60-100` (the `panel` memo)
- Modify: `internal/web/_ui/src/screens/Run.tsx` (the panel memo built for narrow widths,
  and the permanent right column: add a "Plan" section under the agent tree)
- Test: `internal/web/_ui/src/screens/chat.test.tsx`,
  `internal/web/_ui/src/screens/screens.test.tsx`

**Interfaces:**
- Consumes: `RunView.todos: TodoItem[]` where `TodoItem = { text: string; status?: string }`
  (`src/lib/wire.ts:196`); statuses are `pending`, `in_progress`, `completed`
  (`internal/agent/todo.go:12-14`).
- Produces: `InspectorContent.plan?: InspectorRow[]` and a helper
  `planRows(todos: TodoItem[]): InspectorRow[]` exported from `src/state/run.ts`.

- [ ] **Step 1: Write the failing fold test**

Append to `internal/web/_ui/src/state/run.test.ts`:

```ts
import { planRows } from './run'

test('a plan row says its state without relying on colour', () => {
  const rows = planRows([
    { text: 'read the spec', status: 'completed' },
    { text: 'write the test', status: 'in_progress' },
    { text: 'commit', status: 'pending' },
    { text: 'unknown', status: 'later' },
  ])
  expect(rows.map((r) => r.icon)).toEqual(['✓', '●', '·', '·'])
  expect(rows.map((r) => r.meta)).toEqual(['done', 'doing', '', ''])
  expect(rows[0]?.color).toBe('var(--success)')
  expect(rows[1]?.color).toBe('var(--running)')
})
```

Put the `import { planRows } from './run'` line with the other imports at the top of the
file, not inline.

- [ ] **Step 2: Run it to make sure it fails**

Run: `cd internal/web/_ui && npx vitest run src/state/run.test.ts`

Expected: FAIL - `planRows` is not exported.

- [ ] **Step 3: Implement `planRows`**

In `internal/web/_ui/src/state/run.ts`, after the `liveStatus` function (search for
`export function liveStatus`), add:

```ts
/** A plan item as the inspector draws it. The glyph carries the state; the meta repeats it in words. */
export function planRows(todos: TodoItem[]): InspectorRow[] {
  return todos.map((t) => {
    switch (t.status) {
      case 'completed':
        return { icon: '✓', color: 'var(--success)', text: t.text, meta: 'done' }
      case 'in_progress':
        return { icon: '●', color: 'var(--running)', text: t.text, meta: 'doing' }
      default:
        return { icon: '·', text: t.text, meta: '' }
    }
  })
}
```

Add `import type { InspectorRow } from './inspector'` to the imports of `run.ts`. Both files
are in `src/state/`, so the layering rule holds.

- [ ] **Step 4: Run the fold test**

Run: `cd internal/web/_ui && npx vitest run src/state/run.test.ts`

Expected: PASS.

- [ ] **Step 5: Extend the inspector contract**

In `internal/web/_ui/src/state/inspector.ts`, inside `InspectorContent`, after
`list?: InspectorRow[]`, add:

```ts
  /** The agent's working plan, drawn above `list` when present. */
  plan?: InspectorRow[]
```

In `internal/web/_ui/src/shell/Inspector.tsx`, the block that draws `content.list` starts with
`{content.list && content.list.length > 0 && (`. Move its markup into one local component at
the bottom of the file, so the plan and the list share it:

```tsx
function Rows({ title, rows }: { title?: string; rows: InspectorRow[] }) {
  return (
    <>
      <Rule />
      <div className="p-3">
        <h3 className="mb-[7px] text-[10px] font-semibold tracking-[.07em] text-fg-subtle uppercase">
          {title}
        </h3>
        {rows.map((row) => (
          <div key={row.text} className="flex min-h-[26px] items-center gap-2">
            <span
              aria-hidden="true"
              className="text-[10px]"
              style={{ color: row.color ?? 'var(--fg-subtle)' }}
            >
              {row.icon}
            </span>
            <span className="overflow-hidden text-[11.5px] text-ellipsis whitespace-nowrap text-fg-muted">
              {row.text}
            </span>
            {row.meta && (
              <span className="ml-auto font-mono text-[10px] text-fg-subtle">{row.meta}</span>
            )}
          </div>
        ))}
      </div>
    </>
  )
}
```

and replace the list block with:

```tsx
      {content.plan && content.plan.length > 0 && <Rows title="Plan" rows={content.plan} />}
      {content.list && content.list.length > 0 && (
        <Rows title={content.listTitle} rows={content.list} />
      )}
```

Import the type: `import type { InspectorContent, InspectorRow } from '@/state/inspector'`.

- [ ] **Step 6: Write the failing screen test**

Append to `internal/web/_ui/src/screens/chat.test.tsx`:

```ts
// The daemon sends the whole plan on every change; the inspector shows the
// current one, with each item's state readable without colour.
test('the inspector shows the agent\'s plan as it changes', async () => {
  const h = await attached()
  h.emit({
    seq: 1,
    time: '2026-09-08T12:00:01Z',
    kind: EventKind.Todo,
    todos: [
      { text: 'read the spec', status: 'completed' },
      { text: 'write the test', status: 'in_progress' },
    ],
  })
  const aside = screen.getByRole('complementary', { name: 'Inspector' })
  await waitFor(() => expect(within(aside).getByText('Plan')).toBeInTheDocument())
  expect(within(aside).getByText('write the test')).toBeInTheDocument()
  expect(within(aside).getByText('doing')).toBeInTheDocument()

  h.emit({
    seq: 2,
    time: '2026-09-08T12:00:02Z',
    kind: EventKind.Todo,
    todos: [{ text: 'commit', status: 'pending' }],
  })
  await waitFor(() => expect(within(aside).queryByText('read the spec')).not.toBeInTheDocument())
  expect(within(aside).getByText('commit')).toBeInTheDocument()
})
```

- [ ] **Step 7: Run it to make sure it fails**

Run: `cd internal/web/_ui && npx vitest run src/screens/chat.test.tsx -t "plan"`

Expected: FAIL - "Plan" is not in the inspector.

- [ ] **Step 8: Publish the plan from the chat screen**

In `internal/web/_ui/src/screens/Chat.tsx`, in the `panel` memo (search for
`listTitle: run.files.length > 0 ? 'Files touched' : undefined,`), add above that line:

```ts
      plan: planRows(run.todos),
```

and import it: change `import { liveStatus } from '@/state/run'` to
`import { liveStatus, planRows } from '@/state/run'`.

The memo depends on `[record, run]`. `run` is the same object across events (the fold appends
in place) and `run.seq` changes per event; the memo already re-runs because the hook hands
React a new `view` reference per event batch (`hooks/useRunEvents.ts`, `onEvents`). Do not add
`run.todos` to the dependency array.

- [ ] **Step 9: Run the screen test**

Run: `cd internal/web/_ui && npx vitest run src/screens/chat.test.tsx -t "plan"`

Expected: PASS.

- [ ] **Step 10: The run screen's column gets the plan too**

In `internal/web/_ui/src/screens/Run.tsx`, the narrow `panel` memo (search for
`kind: 'run',`) gets `plan: planRows(run.todos),` after the `fields:` line, and the import
`import { agentTree, liveStatus, planRows } from '@/state/run'` (merge with the existing
imports from `@/state/run`; there are two today - `liveStatus` on one line and `agentTree` on
another - make it one line).

In the permanent right column, directly after the line
`<AgentTree nodes={tree} selected={agent} onSelect={setAgent} />`,
add:

```tsx
            {run.todos.length > 0 && (
              <>
                <div aria-hidden="true" className="h-px bg-line" />
                <div className="p-3">
                  <h2 className="mb-2 text-[10px] font-semibold tracking-[.07em] text-fg-subtle uppercase">
                    Plan
                  </h2>
                  {planRows(run.todos).map((row) => (
                    <div key={row.text} className="flex h-6 items-center gap-2 font-mono text-[11px]">
                      <span aria-hidden="true" className="w-2" style={{ color: row.color }}>
                        {row.icon}
                      </span>
                      <span className="sr-only">{row.meta || 'pending'}</span>
                      <span className="overflow-hidden text-ellipsis whitespace-nowrap text-fg-muted">
                        {row.text}
                      </span>
                    </div>
                  ))}
                </div>
              </>
            )}
```

Append to `internal/web/_ui/src/screens/screens.test.tsx`:

```ts
test('the run screen lists the plan beside the agent tree', async () => {
  const h = await mountApp({ runs: [RUN] })
  go('run')
  act(() => navigate({ screen: 'run', id: 'r-1' }))
  await waitFor(() => expect(h.runSocket()).toBeTruthy())
  h.runSocket()?.open()
  h.emit({
    seq: 1,
    time: '2026-09-08T12:00:01Z',
    kind: EventKind.Todo,
    todos: [{ text: 'rotate the keys', status: 'in_progress' }],
  })
  expect(await screen.findByRole('heading', { name: 'Plan' })).toBeInTheDocument()
  expect(screen.getByText('rotate the keys')).toBeInTheDocument()
})
```

Run: `cd internal/web/_ui && npx vitest run src/screens/screens.test.tsx -t "plan beside"`

Expected: PASS.

- [ ] **Step 11: Full check, deploy, look**

Run: `make web-check`

Expected: eslint clean, tsc clean, all test files pass.

Deploy:

```sh
make web && make install && systemctl --user restart aigem-web
curl -s https://tba.tail74d52.ts.net/healthz
```

Expected: `{"ok":true}`.

Headless check: open a chat that has a plan. Start a new session and send
`Make a three step plan to list this directory, then do it.` through the composer with the
playwright helper from Task 3 Step 12 (it exists in the session scratchpad as `shot.mjs`; if it
does not, recreate it from that step), wait for the turn, and screenshot the inspector. The
plan must appear with ✓ / ● / · glyphs. This costs one short model turn.

- [ ] **Step 12: Commit and push**

```bash
git add internal/web/_ui/src
git commit -m "feat(web): draw the agent's plan in the inspector and the run column"
git push origin main
```

---

### Task 3: Image attachments

The `submit` op already carries `images: [{media_type, data}]` end to end
(`src/lib/wire.ts:235`, `internal/web/backend.go:256`, `cmd/aigem/webbackend.go:327`,
`internal/uisession/local.go:604`). Two things stop it: the websocket frame cap of 64 KiB
(`internal/web/ws.go:57`), and a composer with no way to attach.

Decision taken here: raise the frame cap to 1 MiB and the message cap to 4 MiB, and have the
client shrink an image to fit. Worst case memory is 64 sockets x 4 MiB = 256 MiB, on a daemon
that serves one signed-in person. The client scales any image to at most 1568 px on its long
edge and re-encodes as JPEG at quality 0.85 when the file is over 700 KiB, which lands a
screenshot well under the frame cap.

**Files:**
- Modify: `internal/web/ws.go:47-58` (the two caps and their comment)
- Modify: `docs/web.md:286-289` and `docs/web.md:470-476` (the caps and the "not yet" sentence)
- Create: `internal/web/_ui/src/lib/image.ts`
- Test: `internal/web/_ui/src/lib/image.test.ts`
- Modify: `internal/web/_ui/src/screens/Chat.tsx:121-126` (submit) and `:350-380` (composer)
- Test: `internal/web/_ui/src/screens/chat.test.tsx`
- Test: `internal/web/runsocket_test.go:418` (uses `wsMaxFrame`; it keeps passing)

**Interfaces:**
- Consumes: the `RunOp` variant
  `{ op: 'submit'; text: string; images?: { media_type: string; data: string }[] }`.
- Produces: `readImage(file: Blob): Promise<Attachment>` and
  `type Attachment = { name: string; media_type: string; data: string; bytes: number }`
  from `src/lib/image.ts`; `IMAGE_LIMIT = 700 * 1024` exported from the same file.

- [ ] **Step 1: Raise the caps in the daemon**

In `internal/web/ws.go`, replace the two constants and the last paragraph of their comment:

```go
	// wsMaxFrame and wsMaxMessage bound what one client can make the daemon
	// hold. Everything a client sends up any of these streams is a small op
	// envelope, and without a bound a single unterminated message is an
	// out-of-memory the sender pays nothing for.
	//
	// The frame bound is the operative one, and it bounds a browser: a page
	// sends a message as one frame rather than fragmenting it, so this is what
	// a submit may carry. A megabyte holds typed text and a screenshot the page
	// has scaled down, and 64 sockets of it is a quarter of a gigabyte on a
	// daemon that serves one signed-in person.
	wsMaxFrame   = 1 << 20
	wsMaxMessage = 4 << 20
```

- [ ] **Step 2: Run the socket tests**

Run: `go test ./internal/web/ -run 'FrameBound|Malformed' -count=1`

Expected: `ok`. `TestASubmitPastTheFrameBoundEndsTheSocket` sends `wsMaxFrame/2` and
`wsMaxFrame` bytes, so it follows the constant.

- [ ] **Step 3: Update the protocol document**

In `docs/web.md`, the sentence at line 287-288 reads
`... and caps one frame at 64 KiB and one message at 256 KiB.` Change it to
`... and caps one frame at 1 MiB and one message at 4 MiB.`

The paragraph at lines 470-476 begins `The connection's own contract - the pings, the timeouts,
the frame and message caps, the 64-socket ceiling - is the control stream's, above.` Replace
the rest of that paragraph with:

```
The frame cap is the one that bites here: a browser sends a message as a single
frame, so a submit may carry 1 MiB. `images` is an array of `{"media_type":
"image/png", "data": "<base64>"}`, and the page scales what it attaches so a
message stays inside the cap; a submit past it ends the socket, as any
oversized frame does.
```

- [ ] **Step 4: Go lint and tests, commit the daemon half**

Run: `gofmt -l internal && golangci-lint run && go test ./internal/web/`

Expected: clean, `0 issues.`, `ok`.

```bash
git add internal/web/ws.go docs/web.md
git commit -m "feat(web): let a submit carry a scaled screenshot"
```

- [ ] **Step 5: Write the failing image test**

Create `internal/web/_ui/src/lib/image.test.ts`:

```ts
import { expect, test } from 'vitest'
import { IMAGE_LIMIT, readImage } from './image'

// jsdom has no canvas, so a small file goes through untouched: the base64 of
// its bytes, its type, its size.
test('a small image is sent as it is', async () => {
  const bytes = new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10])
  const file = new File([bytes], 'shot.png', { type: 'image/png' })
  const out = await readImage(file)
  expect(out).toEqual({
    name: 'shot.png',
    media_type: 'image/png',
    data: 'iVBORw0KGgo=',
    bytes: 8,
  })
})

test('a file that is not an image is refused', async () => {
  const file = new File(['hello'], 'notes.txt', { type: 'text/plain' })
  await expect(readImage(file)).rejects.toThrow('not an image')
})

// Without a canvas nothing can be scaled, so a large file is refused rather
// than sent past the frame cap.
test('a large image is refused where it cannot be scaled', async () => {
  const file = new File([new Uint8Array(IMAGE_LIMIT + 1)], 'big.png', { type: 'image/png' })
  await expect(readImage(file)).rejects.toThrow('too large')
})
```

- [ ] **Step 6: Run it to make sure it fails**

Run: `cd internal/web/_ui && npx vitest run src/lib/image.test.ts`

Expected: FAIL - cannot resolve `./image`.

- [ ] **Step 7: Implement `readImage`**

Create `internal/web/_ui/src/lib/image.ts`:

```ts
/**
 * An image for the composer: base64, inside the daemon's frame cap.
 *
 * A screenshot is several megabytes and the run socket takes one megabyte per
 * frame, so a large image is drawn onto a canvas at most 1568 px on its long
 * edge and re-encoded as JPEG. A browser without a canvas - or a test runner -
 * cannot do that, and refuses the image rather than sending one that would end
 * the socket.
 */

export type Attachment = { name: string; media_type: string; data: string; bytes: number }

export const IMAGE_LIMIT = 700 * 1024
const LONG_EDGE = 1568
const TYPES = new Set(['image/png', 'image/jpeg', 'image/webp', 'image/gif'])

export async function readImage(file: Blob & { name?: string }): Promise<Attachment> {
  if (!TYPES.has(file.type)) throw new Error(`${file.name ?? 'the clipboard'} is not an image`)
  const name = file.name ?? 'pasted image'
  if (file.size <= IMAGE_LIMIT) {
    return { name, media_type: file.type, data: await base64(file), bytes: file.size }
  }
  const scaled = await shrink(file)
  if (!scaled) throw new Error(`${name} is too large to send and cannot be scaled here`)
  return { name, media_type: 'image/jpeg', data: await base64(scaled), bytes: scaled.size }
}

async function shrink(file: Blob): Promise<Blob | null> {
  if (typeof createImageBitmap !== 'function') return null
  const canvas = document.createElement('canvas')
  const ctx = canvas.getContext('2d')
  if (!ctx) return null
  const bitmap = await createImageBitmap(file)
  const scale = Math.min(1, LONG_EDGE / Math.max(bitmap.width, bitmap.height))
  canvas.width = Math.round(bitmap.width * scale)
  canvas.height = Math.round(bitmap.height * scale)
  ctx.drawImage(bitmap, 0, 0, canvas.width, canvas.height)
  bitmap.close()
  const out = await new Promise<Blob | null>((r) => canvas.toBlob(r, 'image/jpeg', 0.85))
  return out && out.size <= IMAGE_LIMIT ? out : null
}

function base64(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onerror = () => reject(reader.error ?? new Error('could not read the image'))
    reader.onload = () => resolve(String(reader.result).split(',', 2)[1] ?? '')
    reader.readAsDataURL(blob)
  })
}
```

- [ ] **Step 8: Run the image tests**

Run: `cd internal/web/_ui && npx vitest run src/lib/image.test.ts`

Expected: PASS (3 tests).

- [ ] **Step 9: Write the failing composer test**

Append to `internal/web/_ui/src/screens/chat.test.tsx`:

```ts
// A pasted screenshot is the ordinary attachment on a phone and a laptop
// alike; it rides on the same submit as the text.
test('a pasted image is attached and sent with the message', async () => {
  const user = userEvent.setup()
  const h = await attached()
  const box = screen.getByRole('textbox', { name: 'Message' })
  const png = new File([new Uint8Array([137, 80, 78, 71])], 'shot.png', { type: 'image/png' })
  await user.click(box)
  await user.paste({ getData: () => '', files: [png], items: [], types: ['Files'] } as never)

  expect(await screen.findByText('shot.png')).toBeInTheDocument()
  await user.type(box, 'what is this?')
  await user.keyboard('{Control>}{Enter}{/Control}')

  await waitFor(() => expect(h.runSocket()?.sent).toHaveLength(1))
  expect(JSON.parse(h.runSocket()?.sent[0] ?? '{}')).toEqual({
    op: 'submit',
    text: 'what is this?',
    images: [{ media_type: 'image/png', data: 'iVBORw==' }],
  })
  // Sent means gone: the chip and the text both clear.
  expect(screen.queryByText('shot.png')).not.toBeInTheDocument()
})

test('an attachment can be removed before sending, and an image alone can be sent', async () => {
  const user = userEvent.setup()
  const h = await attached()
  const png = new File([new Uint8Array([137, 80, 78, 71])], 'shot.png', { type: 'image/png' })
  const input = screen.getByLabelText('Attach an image')
  await user.upload(input, png)
  expect(await screen.findByText('shot.png')).toBeInTheDocument()

  await user.click(screen.getByRole('button', { name: 'Remove shot.png' }))
  expect(screen.queryByText('shot.png')).not.toBeInTheDocument()

  await user.upload(input, png)
  await screen.findByText('shot.png')
  await user.click(screen.getByRole('button', { name: 'Send' }))
  await waitFor(() => expect(h.runSocket()?.sent).toHaveLength(1))
  expect(JSON.parse(h.runSocket()?.sent[0] ?? '{}')).toEqual({
    op: 'submit',
    text: '',
    images: [{ media_type: 'image/png', data: 'iVBORw==' }],
  })
})
```

- [ ] **Step 10: Run them to make sure they fail**

Run: `cd internal/web/_ui && npx vitest run src/screens/chat.test.tsx -t "image"`

Expected: FAIL - no element with text `shot.png`, no label `Attach an image`.

- [ ] **Step 11: Implement the composer half**

In `internal/web/_ui/src/screens/Chat.tsx`:

1. Imports: add `import { readImage } from '@/lib/image'` and
   `import type { Attachment } from '@/lib/image'`, and `useRef` to the React import.

2. State, next to `const [text, setText] = useState(pendingCommand.text)`:

```ts
  const [images, setImages] = useState<Attachment[]>([])
  const picker = useRef<HTMLInputElement>(null)

  const attach = async (files: Iterable<File>) => {
    for (const file of files) {
      try {
        const image = await readImage(file)
        setImages((list) => [...list, image])
      } catch (err) {
        setBanner(explain(err))
      }
    }
  }
```

   Import `explain` and `setBanner` from `@/state/app` (extend the existing import line).

3. Replace the `submit` function:

```ts
  const submit = () => {
    const value = text.trim()
    if (!value && images.length === 0) return
    let sent: boolean
    if (value.startsWith('/')) {
      sent = slash(value, runId, send)
    } else {
      sent = send({
        op: 'submit',
        text: value,
        ...(images.length > 0
          ? { images: images.map(({ media_type, data }) => ({ media_type, data })) }
          : {}),
      })
    }
    if (sent) {
      setText('')
      setImages([])
    }
  }
```

4. The textarea gets a paste handler, after `onKeyDown={...}`:

```tsx
                      onPaste={(e) => {
                        const files = Array.from(e.clipboardData?.files ?? []).filter((f) =>
                          f.type.startsWith('image/'),
                        )
                        if (files.length === 0) return
                        e.preventDefault()
                        void attach(files)
                      }}
```

5. Between the `</textarea ... />` and the Send button, an attach button and a hidden file
   input:

```tsx
                    <input
                      ref={picker}
                      type="file"
                      accept="image/*"
                      multiple
                      aria-label="Attach an image"
                      className="hidden"
                      onChange={(e) => {
                        void attach(e.target.files ?? [])
                        e.target.value = ''
                      }}
                    />
                    <button
                      type="button"
                      onClick={() => picker.current?.click()}
                      disabled={state !== 'open'}
                      aria-label="Attach"
                      title="Attach an image"
                      className="flex-none self-stretch rounded-md border border-line px-[10px] text-[14px] text-fg-muted enabled:cursor-pointer enabled:hover:border-line-strong enabled:hover:text-fg disabled:opacity-50"
                    >
                      <span aria-hidden="true">⊕</span>
                    </button>
```

   Note `className="hidden"`: Tailwind's `hidden` is `display: none`, and Testing Library's
   `getByLabelText` still finds it; `user.upload` works on a hidden input.

6. The Send button's `disabled` becomes
   `disabled={(!text.trim() && images.length === 0) || state !== 'open'}`.

7. Above the `<div className="flex gap-2">` that holds the textarea, the chips:

```tsx
                  {images.length > 0 && (
                    <ul aria-label="Attachments" className="m-0 mb-2 flex list-none flex-wrap gap-[6px] p-0">
                      {images.map((im, i) => (
                        <li
                          key={`${im.name}-${i}`}
                          className="flex h-[22px] items-center gap-[6px] rounded-[5px] border border-line bg-bg px-2 font-mono text-[10.5px] text-fg-muted"
                        >
                          <span>{im.name}</span>
                          <span className="text-fg-subtle">{Math.ceil(im.bytes / 1024)}K</span>
                          <button
                            type="button"
                            onClick={() => setImages((list) => list.filter((_, j) => j !== i))}
                            aria-label={`Remove ${im.name}`}
                            className="cursor-pointer text-fg-subtle hover:text-fg"
                          >
                            <span aria-hidden="true">×</span>
                          </button>
                        </li>
                      ))}
                    </ul>
                  )}
```

- [ ] **Step 12: Run the composer tests, then everything**

Run: `cd internal/web/_ui && npx vitest run src/screens/chat.test.tsx -t "image"`

Expected: PASS (2 tests). If the paste test cannot deliver files through `user.paste`, fire
the event by hand instead:

```ts
  fireEvent.paste(box, { clipboardData: { files: [png], getData: () => '' } })
```

with `fireEvent` imported from `@testing-library/react`.

Run: `make web-check`

Expected: eslint clean, tsc clean, every test file passes. The a11y test
`every control on the shell has a name that can be read out` must still pass: both new
buttons carry `aria-label`.

- [ ] **Step 13: Deploy and check with a real screenshot**

Deploy:

```sh
make web && make install && systemctl --user restart aigem-web
curl -s https://tba.tail74d52.ts.net/healthz
```

Headless check. The session scratchpad holds `pw/` with playwright installed and a signed-in
`state.json`; if it is gone, recreate it: `npm i playwright` in a scratch directory, launch
with `chromium.launch({ channel: 'chrome' })` (the playwright CDN is unreachable here and the
system Chrome works), open `https://tba.tail74d52.ts.net/?token=<token from
journalctl --user -u aigem-web | grep -o 'token=[^ ]*'>` once and save `storageState`.

Then this script, run with `node attach.mjs`:

```js
import { chromium } from 'playwright'
const base = 'https://tba.tail74d52.ts.net'
const browser = await chromium.launch({ channel: 'chrome' })
const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, storageState: 'state.json' })
const page = await ctx.newPage()
const problems = []
page.on('console', (m) => { if (m.type() === 'error') problems.push(m.text()) })
page.on('response', (r) => { if (r.status() >= 400) problems.push(`HTTP ${r.status()} ${r.url()}`) })
await page.goto(base + '/chat', { waitUntil: 'networkidle' })
await page.getByRole('button', { name: 'New' }).click()
await page.waitForURL(/\/chat\/RUN-/)
await page.screenshot({ path: 'before.png', fullPage: true })
await page.getByLabel('Attach an image').setInputFiles('before.png')
await page.waitForTimeout(500)
console.log('chip:', await page.getByRole('list', { name: 'Attachments' }).textContent())
await page.getByRole('textbox', { name: 'Message' }).fill('In one sentence, what is in this screenshot?')
await page.getByRole('button', { name: 'Send' }).click()
await page.waitForTimeout(20000)
console.log('transcript:', (await page.getByRole('log', { name: 'Transcript' }).textContent()).slice(0, 600))
await page.screenshot({ path: 'after.png' })
console.log('problems:', problems.length ? problems.join('\n') : 'none')
await browser.close()
```

Expected: the chip lists `before.png` with a size under 700K, the transcript shows the user
line with `1 image`, an agent answer that describes the page, and `problems: none`. A refused
frame would show as the socket dropping and `stream ended` in the header: that means the
scaled image was still over the cap, and `IMAGE_LIMIT` or the JPEG quality must come down.
This costs one model turn with an image.

- [ ] **Step 14: Commit and push**

```bash
git add internal/web/_ui/src
git commit -m "feat(web): attach images to a message"
git push origin main
```

---

### Task 4: Review cycle and changelog

**Files:**
- Modify: `CHANGELOG.md` (the `[Unreleased]` / `Added` list, the browser UI bullet)

- [ ] **Step 1: Two reviewers on the whole range**

Dispatch two review subagents with worktree isolation (they stash otherwise) over
`git diff b5fd212..HEAD`: one for code quality and simplicity, one for functionality, security
and tests. Ask the first specifically whether `Rows` in `Inspector.tsx` and the run column's
plan markup should be one component, and whether `readImage` can lose the `name` field. Ask
the second to trace a paste of a non-image, a 5 MB PNG in a real browser, and an image sent
while the socket is closed.

- [ ] **Step 2: Apply what they find, rerun `make web-check` and `go test ./...`**

- [ ] **Step 3: Changelog**

In `CHANGELOG.md`, the `Added` bullet that begins `- The browser UI runs the slash commands:`
gets two sentences appended before its final period:

```
The inspector and the run column show the agent's plan. A message can carry
images, pasted or attached; the page scales a screenshot to fit the socket's
1 MiB frame.
```

- [ ] **Step 4: Deploy, commit, push**

```bash
make web && make install && systemctl --user restart aigem-web
git add -A
git commit -m "fix(web): close what the review cycle found; note the plan panel and images"
git push origin main
```

---

## Out of scope, on purpose

- **Projects phase two** (Tickets, Worktrees, Task screens, the New project button). It is a
  new subsystem: a project model, repositories, per-run worktrees, a ticket store, and the
  `stop` op that discards a worktree. Run `superpowers:brainstorming` for it and write a spec
  under `docs/superpowers/specs/` before any plan.
- **A Stop run button**: bound to the worktree design above (`src/screens/Run.tsx:36-38`).
- **A usage screen**: `/api/usage` is shown inside the Models inspector; a screen of its own
  is a design question for the canvas, not a gap.
- **Gating the Tickets and Worktrees rows**: the canvas puts them in the navigation and the
  placeholder screens say what they wait for. Leave them until phase two replaces them.
