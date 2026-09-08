import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { backoff, connectControl } from './control'
import type { ControlState } from './control'
import type { Meta } from './wire'
import { FakeSocket, installFakeSocket } from '@/test/fakesocket'

const meta: Meta = { version: '1.0.0', defaultModel: 'a/b', rev: 12, ui: true, features: {} }

type Handlers = {
  hellos: Meta[]
  deltas: { kind: string; data: unknown }[]
  gaps: number
  states: ControlState[]
}

function connect() {
  const seen: Handlers = { hellos: [], deltas: [], gaps: 0, states: [] }
  const conn = connectControl({
    onHello: (m) => seen.hellos.push(m),
    onDelta: (kind, data) => seen.deltas.push({ kind, data }),
    onGap: () => seen.gaps++,
    onStatus: (s) => seen.states.push(s),
  })
  return { seen, conn }
}

beforeEach(() => {
  installFakeSocket()
  vi.useFakeTimers()
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

test('takes its base from hello and applies the deltas that follow', () => {
  const { seen, conn } = connect()
  FakeSocket.last.open()
  FakeSocket.last.deliver({ type: 'hello', rev: 12, data: meta })
  FakeSocket.last.deliver({ type: 'run.updated', rev: 13, data: { id: 'r1' } })

  expect(seen.hellos).toEqual([meta])
  expect(seen.deltas).toEqual([{ kind: 'run.updated', data: { id: 'r1' } }])
  expect(seen.gaps).toBe(0)
  conn.close()
})

// The whole point of the revision counter. A page that missed a message has no
// way to be told what was in it, so noticing the hole is what makes it refetch.
test('reports a gap when a revision skips', () => {
  const { seen, conn } = connect()
  FakeSocket.last.open()
  FakeSocket.last.deliver({ type: 'hello', rev: 12, data: meta })
  FakeSocket.last.deliver({ type: 'run.updated', rev: 15, data: { id: 'r1' } })

  expect(seen.gaps).toBe(1)
  // Still applied: the delta that did arrive is real, and the refetch the gap
  // triggers is what covers the ones that did not.
  expect(seen.deltas).toHaveLength(1)
  conn.close()
})

// A frame that is not a mutation repeats the revision the client is already at.
// Reading that as a gap would make every one of the page's own mistakes - a
// refused op, a ping answered - cost a full refetch of every collection.
test('a repeated revision is not a gap', () => {
  const { seen, conn } = connect()
  FakeSocket.last.open()
  FakeSocket.last.deliver({ type: 'hello', rev: 12, data: meta })
  FakeSocket.last.deliver({ type: 'client_error', rev: 12, data: { error: 'unknown op' } })

  expect(seen.gaps).toBe(0)
  conn.close()
})

// "A delta that arrives with no data carries nothing the client can apply" -
// the daemon had nothing to say or could not encode it, so the page reads the
// collections again rather than applying an empty change.
test('a delta with no payload is treated as a gap', () => {
  const { seen, conn } = connect()
  FakeSocket.last.open()
  FakeSocket.last.deliver({ type: 'hello', rev: 12, data: meta })
  FakeSocket.last.deliver({ type: 'skills.updated', rev: 13 })

  expect(seen.gaps).toBe(1)
  expect(seen.deltas).toHaveLength(0)
  conn.close()
})

// hello re-bases. A reconnection's numbers are not continuous with the ones
// before it, and comparing across the break reports a gap the reconnection has
// already recovered from - or, worse, hides a real one behind a lower number.
test('re-bases on the hello of the next connection', () => {
  const { seen, conn } = connect()
  FakeSocket.last.open()
  FakeSocket.last.deliver({ type: 'hello', rev: 400, data: meta })
  FakeSocket.last.drop()

  vi.advanceTimersByTime(20_000)
  expect(FakeSocket.instances).toHaveLength(2)
  FakeSocket.last.open()
  FakeSocket.last.deliver({ type: 'hello', rev: 7, data: { ...meta, rev: 7 } })
  FakeSocket.last.deliver({ type: 'run.updated', rev: 8, data: { id: 'r1' } })

  expect(seen.gaps).toBe(0)
  expect(seen.hellos).toHaveLength(2)
  conn.close()
})

// Before hello there is no base to compare against, and the daemon always sends
// it first. A frame that somehow arrived earlier must not be read as a gap from
// revision zero.
test('ignores a frame that arrives before the base', () => {
  const { seen, conn } = connect()
  FakeSocket.last.open()
  FakeSocket.last.deliver({ type: 'run.updated', rev: 900, data: { id: 'r1' } })

  expect(seen.gaps).toBe(0)
  expect(seen.deltas).toHaveLength(0)
  conn.close()
})

test('closing stops it reconnecting', () => {
  const { conn } = connect()
  FakeSocket.last.open()
  conn.close()
  vi.advanceTimersByTime(60_000)
  expect(FakeSocket.instances).toHaveLength(1)
})

// Full jitter over an exponential ceiling: a daemon that restarts has every tab
// dialling it, and a fixed delay makes them all arrive together.
test('backs off exponentially and never below the floor or above the ceiling', () => {
  expect(backoff(0, () => 0)).toBe(500)
  expect(backoff(0, () => 1)).toBe(500)
  expect(backoff(3, () => 1)).toBe(4000)
  expect(backoff(30, () => 1)).toBe(15_000)
  expect(backoff(30, () => 0)).toBe(500)
  // Jitter, not a constant: two draws from the same attempt differ.
  expect(backoff(5, () => 0.25)).not.toBe(backoff(5, () => 0.75))
})

// The refusal frame is this connection's own mistake coming back, not a state
// change. It is a delta with a payload, so without a case of its own the page
// would refetch every collection over it.
test('the refusal frame carries a payload and is still not a mutation', () => {
  const { seen, conn } = connect()
  FakeSocket.last.open()
  FakeSocket.last.deliver({ type: 'hello', rev: 12, data: meta })
  FakeSocket.last.deliver({
    type: 'client_error',
    rev: 12,
    data: { op: 'subscribe', error: 'the control stream is read-only' },
  })

  expect(seen.gaps).toBe(0)
  expect(seen.deltas).toEqual([
    { kind: 'client_error', data: { op: 'subscribe', error: 'the control stream is read-only' } },
  ])
  conn.close()
})

// A frame the page cannot decode is a frame whose meaning it missed. Going
// quietly stale over one is the failure the revision counter exists to prevent.
test('a frame it cannot parse is a gap', () => {
  const { seen, conn } = connect()
  FakeSocket.last.open()
  FakeSocket.last.deliver({ type: 'hello', rev: 12, data: meta })
  FakeSocket.last.deliver('{ not json')

  expect(seen.gaps).toBe(1)
  conn.close()
})

// One frame arriving out of order must not lower the watermark: every frame
// after it would then be judged against a number the client has passed, which
// is either a refetch storm or a real gap swallowed.
test('the revision it holds never goes backwards', () => {
  const { seen, conn } = connect()
  FakeSocket.last.open()
  FakeSocket.last.deliver({ type: 'hello', rev: 12, data: meta })
  FakeSocket.last.deliver({ type: 'run.updated', rev: 13, data: { id: 'r1' } })
  FakeSocket.last.deliver({ type: 'run.updated', rev: 11, data: { id: 'r1' } })
  FakeSocket.last.deliver({ type: 'run.updated', rev: 14, data: { id: 'r1' } })

  expect(seen.gaps).toBe(0)
  conn.close()
})

// The backoff has to come back down, or a daemon that blipped three times early
// makes every later reconnection wait the full fifteen seconds for the life of
// the tab.
test('a successful connection resets the backoff', () => {
  const { conn } = connect()
  for (let i = 0; i < 4; i++) {
    FakeSocket.last.open()
    FakeSocket.last.deliver({ type: 'hello', rev: 1, data: meta })
    FakeSocket.last.drop()
    // Well under the ceiling: if the attempt counter had kept climbing, the
    // next socket would not exist yet.
    vi.advanceTimersByTime(1000)
    expect(FakeSocket.instances).toHaveLength(i + 2)
  }
  conn.close()
})

// The status bar reads this. A stream that never reports its own disconnection
// leaves the page looking live while it has stopped hearing anything.
test('reports that it disconnected', () => {
  const { seen, conn } = connect()
  FakeSocket.last.open()
  expect(seen.states).toContain('open')

  FakeSocket.last.drop()
  expect(seen.states).toContain('closed')
  conn.close()
})
