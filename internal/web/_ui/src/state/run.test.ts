import { expect, test } from 'vitest'
import { EventKind } from '@/lib/wire'
import type { Run, RunEvent } from '@/lib/wire'
import { agentTree, apply, applyAll, emptyRun, liveStatus, planRows } from './run'
import { toRow, visibleRows } from './eventrow'

const ev = (seq: number, kind: string, over: Partial<RunEvent> = {}): RunEvent =>
  ({ seq, time: '2026-09-08T12:00:00Z', kind, ...over }) as RunEvent

test('a turn in flight and a turn that ended', () => {
  const view = applyAll(emptyRun(), [ev(1, EventKind.TurnStart)])
  expect(view.running).toBe(true)
  expect(applyAll(view, [ev(2, EventKind.TurnEnd)]).running).toBe(false)
  expect(applyAll(view, [ev(2, EventKind.TurnEnd, { interrupted: true })]).interrupted).toBe(true)
})

// The daemon reports a failed turn on the event that ends it - a provider that
// could not be dialled, a budget that ran out - and not as a separate error
// event. Drawing every turn_end as a tick is a conversation that never happened
// reading as one that did.
test('a turn that failed is not drawn as one that finished', () => {
  const failed = ev(2, EventKind.TurnEnd, { error: 'dial tcp 127.0.0.1:9280: connection refused' })
  const row = toRow(failed)
  expect(row?.glyph).toBe('×')
  expect(row?.text).toContain('connection refused')
  expect(row?.text).not.toBe('Turn finished')

  expect(applyAll(emptyRun(), [ev(1, EventKind.TurnStart), failed]).error).toContain('refused')
})

test('an approval is held until the daemon says it was answered', () => {
  const asked = applyAll(emptyRun(), [
    ev(1, EventKind.ApprovalRequest, {
      id: 'a-1',
      approval: { kind: 'tool', tool: 'run_command', options: [] },
    }),
  ])
  expect(asked.pending.map((p) => p.id)).toEqual(['a-1'])

  // A resolution for something else must not clear it.
  expect(applyAll(asked, [ev(2, EventKind.ApprovalResolved, { id: 'other' })]).pending).toHaveLength(1)
  expect(applyAll(asked, [ev(2, EventKind.ApprovalResolved, { id: 'a-1' })]).pending).toHaveLength(0)
})

test('the context window and its usage come off the stream', () => {
  const view = applyAll(emptyRun(), [
    ev(1, EventKind.SessionMeta, { id: 's-1', text: 'A title', name: 'p/m', ctx: 200000 }),
    ev(2, EventKind.Usage, { tokens: 1234 }),
  ])
  expect(view.ctxSize).toBe(200000)
  expect(view.contextTokens).toBe(1234)
  expect(view.title).toBe('A title')
  expect(view.model).toBe('p/m')
  expect(view.sessionId).toBe('s-1')
})

// A file named twice is one file changed twice, not two rows.
test('a file is listed once however often it changes', () => {
  const view = applyAll(emptyRun(), [
    ev(1, EventKind.FileChanged, { path: 'a.go', created: true }),
    ev(2, EventKind.FileChanged, { path: 'a.go' }),
    ev(3, EventKind.FileChanged, { path: 'b.go' }),
  ])
  expect(view.files.map((f) => f.path)).toEqual(['a.go', 'b.go'])
  expect(view.files[0]?.created).toBe(true)
})

test('the agent tree is the conversation plus what it delegated to', () => {
  const view = applyAll(emptyRun(), [
    ev(1, EventKind.SessionMeta, { name: 'p/m' }),
    ev(2, EventKind.AgentStart, { id: 'a-1', agent: 'Research' }),
    ev(3, EventKind.AgentEnd, { id: 'a-1', tokens: 18400 }),
  ])
  const tree = agentTree(view)
  expect(tree[0]?.id).toBe('root')
  expect(tree[1]?.name).toBe('Research')
  expect(tree[1]?.running).toBe(false)
  expect(tree[1]?.tokens).toBe(18400)
})

// The events that drive state are not steps anybody reads in a timeline, and
// drawing them would put a presence change between two sentences.
test('the state-only events never become rows', () => {
  const view = applyAll(emptyRun(), [
    ev(1, EventKind.Presence, { clients: [] }),
    ev(2, EventKind.Usage, { tokens: 1 }),
    ev(3, EventKind.SessionMeta),
    ev(4, EventKind.ToolBatch),
    ev(5, EventKind.Content, { text: 'a delta' }),
    ev(6, EventKind.AssistantMessage, { text: 'the settled answer' }),
  ])
  expect(view.rows.map((r) => r.text)).toEqual(['the settled answer'])
})

// The detail toggle keeps the conversation's own steps and drops what happened
// under them.
test('phases only drops the nested rows', () => {
  const { rows } = applyAll(emptyRun(), [
    ev(1, EventKind.TurnStart),
    ev(2, EventKind.ToolStart, { name: 'read_file' }),
    ev(3, EventKind.TurnEnd),
  ])
  expect(visibleRows(rows, true)).toHaveLength(3)
  expect(visibleRows(rows, false).map((r) => r.text)).toEqual(['Agent started', 'Turn finished'])
})

test('an unknown kind is still a row rather than a hole', () => {
  const row = apply(emptyRun(), ev(1, 'something_new', { text: 'a thing' }))
  expect(row.events).toHaveLength(1)
  expect(toRow(ev(1, 'something_new', { text: 'a thing' }))?.text).toBe('a thing')
})

// Copying the timeline per event is quadratic - measured at 74 seconds of pure
// copying for forty thousand events. The arrays are appended to in place and
// the view object around them is replaced, which is what React actually reads.
test('folding an event does not copy the timeline', () => {
  const before = applyAll(emptyRun(), [ev(1, EventKind.TurnStart)])
  const after = apply(before, ev(2, EventKind.TurnEnd))
  expect(after).not.toBe(before)
  expect(after.events).toBe(before.events)
  expect(after.events).toHaveLength(2)
  expect(after.rows).toBe(before.rows)
})

// One shared empty view would leak the first conversation's timeline into the
// second's, because the arrays are appended to.
test('two conversations do not share a timeline', () => {
  const a = apply(emptyRun(), ev(1, EventKind.UserMessage, { text: 'first' }))
  const b = apply(emptyRun(), ev(1, EventKind.UserMessage, { text: 'second' }))
  expect(a.events).toHaveLength(1)
  expect(b.events).toHaveLength(1)
  expect(a.rows[0]?.text).toBe('first')
})

// An error event is a failure the conversation reported on its own, distinct
// from a turn that ended badly. Both feed the same field.
test('an error event is recorded', () => {
  const view = applyAll(emptyRun(), [ev(1, EventKind.Error, { error: 'the tool exploded' })])
  expect(view.error).toBe('the tool exploded')
  expect(toRow(ev(1, EventKind.Error, { error: 'the tool exploded' }))?.glyph).toBe('×')
})

// A turn stopped by the budget has stopped. Leaving `running` set keeps the
// spinner and the Interrupt button up on a conversation that is over.
test('a budget that ran out ends the turn', () => {
  const view = applyAll(emptyRun(), [
    ev(1, EventKind.TurnStart),
    ev(2, EventKind.BudgetExhausted, { text: 'the turn budget ran out' }),
  ])
  expect(view.running).toBe(false)
  expect(view.rows[view.rows.length - 1]?.text).toBe('the turn budget ran out')
})

// A subagent that delegated further is drawn one level deeper. Flattening the
// tree loses which agent asked for what.
test('a nested subagent is drawn under the one that spawned it', () => {
  const view = applyAll(emptyRun(), [
    ev(1, EventKind.AgentStart, { id: 'a-1', agent: 'Research' }),
    ev(2, EventKind.AgentStart, { id: 'a-2', agent: 'Reader', run_id: 'a-1' }),
  ])
  const tree = agentTree(view)
  expect(tree.map((n) => n.level)).toEqual([0, 1, 2])
})

// A turn that was interrupted is not one that failed, and not one that
// finished: the design draws it with its own glyph.
test('an interrupted turn is drawn as neither a success nor a failure', () => {
  const row = toRow(ev(2, EventKind.TurnEnd, { interrupted: true }))
  expect(row?.glyph).toBe('■')
  expect(row?.text).toBe('Interrupted')
})

// The line that ends a turn is where the model's own answer becomes visible,
// rather than a fixed word that says nothing about what happened.
test('a finished turn carries the model\'s answer, or falls back with none', () => {
  const withText = toRow(ev(2, EventKind.TurnEnd, { text: 'Done: two files listed.' }))
  expect(withText?.text).toBe('Done: two files listed.')

  const withoutText = toRow(ev(2, EventKind.TurnEnd))
  expect(withoutText?.text).toBe('Turn finished')
})

// The record's booleans are whatever was true when the daemon last announced
// one. For the conversation this tab is attached to, the stream has said
// everything the record could and said it sooner.
test('the stream outranks the record for the run being watched', () => {
  const record: Run = {
    id: 'r1',
    mode: 'interactive',
    status: 'open',
    created: '',
    updated: '',
    live: true,
    running: true,
  }
  const finished = applyAll(emptyRun(), [ev(1, EventKind.TurnStart), ev(2, EventKind.TurnEnd)])
  expect(liveStatus(record, finished)).toBe('waiting')

  // With nothing read yet there is only the record to go on.
  expect(liveStatus(record, emptyRun())).toBe('running')

  // An unanswered approval outranks both.
  const asked = applyAll(finished, [
    ev(3, EventKind.ApprovalRequest, {
      id: 'a-1',
      approval: { kind: 'tool', tool: 'bash', options: [] },
    }),
  ])
  expect(liveStatus(record, asked)).toBe('attention')
})

// A subagent's tool calls sit one level deeper than the conversation's, which
// is how a transcript says who ran what. Flattening them loses that.
test('a subagent tool call is drawn deeper than the conversation it serves', () => {
  const own = toRow(ev(1, EventKind.ToolStart, { name: 'read_file' }))
  const sub = toRow(ev(2, EventKind.SubToolStart, { name: 'read_file', run_id: 'a-1' }))
  expect(own?.level).toBe(1)
  expect(sub?.level).toBe(2)
  expect(toRow(ev(3, EventKind.SubToolEnd, { name: 'read_file', run_id: 'a-1' }))?.level).toBe(2)
})

// A tool row shows the argument that says what it did. Preferring the wrong
// field shows a command's `query` where its command belongs.
test('a tool row shows the argument that names what it touched', () => {
  const row = toRow(
    ev(1, EventKind.ToolStart, {
      name: 'run_command',
      args: { query: 'unused', command: 'go test ./...' },
    }),
  )
  expect(row?.text).toBe('run_command go test ./...')
  expect(toRow(ev(2, EventKind.ToolStart, { name: 'read_file', args: { path: 'a.go' } }))?.text)
    .toBe('read_file a.go')
})

// The timeline is where a person reads back what the agent actually did, so a
// path that displays as one name and is another is neutralised there too.
test('a tool argument that lies about its name is shown as what it is', () => {
  const row = toRow(
    ev(1, EventKind.ToolStart, { name: 'write_file', args: { path: 'report‮hs.txt' } }),
  )
  expect(row?.text).toContain('\\u202e')
  expect(row?.text).not.toContain('‮')
})

// A trimmed tool result offers its whole body only when the daemon kept one:
// `blob` is set from the write that kept it and never ahead of it.
test('a row offers a body only when there is one', () => {
  expect(toRow(ev(4, EventKind.ToolEnd, { name: 't', bytes: 9000, blob: true }))?.blob).toBe(4)
  expect(toRow(ev(4, EventKind.ToolEnd, { name: 't', bytes: 9000 }))?.blob).toBeUndefined()
})

// `files` counts paths and a conversation can write one file over and over.
// What says the working tree is worth reading again is the number of writes.
test('a file written twice counts twice, and is listed once', () => {
  const view = applyAll(emptyRun(), [
    ev(1, EventKind.FileChanged, { path: 'notes.md' }),
    ev(2, EventKind.FileChanged, { path: 'notes.md' }),
    ev(3, EventKind.FileChanged, { path: 'other.md' }),
  ])
  expect(view.files.map((f) => f.path)).toEqual(['notes.md', 'other.md'])
  expect(view.writes).toBe(3)
})

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

test('the rows a person reads as prose say so; tool rows do not', () => {
  expect(toRow(ev(1, EventKind.UserMessage, { text: 'hello' }))?.prose).toBe(true)
  expect(toRow(ev(2, EventKind.AssistantMessage, { text: 'hi' }))?.prose).toBe(true)
  expect(toRow(ev(3, EventKind.TurnEnd, { text: 'Done: **two** files.' }))?.prose).toBe(true)
  expect(toRow(ev(4, EventKind.TurnEnd))?.prose).toBe(false)
  expect(toRow(ev(5, EventKind.ToolStart, { name: 'read_file' }))?.prose).toBe(false)
  expect(toRow(ev(6, EventKind.Reasoning, { text: 'thinking' }))?.prose).toBe(false)
})
