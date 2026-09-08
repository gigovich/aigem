import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { connectRun } from './socket'
import { EventKind } from './wire'
import type { RunEvent } from './wire'
import { FakeSocket, installFakeSocket } from '@/test/fakesocket'

const event = (seq: number, over: Partial<RunEvent> = {}): RunEvent => ({
  seq,
  time: '2026-09-08T12:00:00Z',
  kind: EventKind.Content,
  text: `e${seq}`,
  ...over,
})

/** The HTTP half: the catch-up page and the run record the socket asks about. */
function stubFetch(handler: (path: string) => Response) {
  const paths: string[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn((path: string) => {
      paths.push(path)
      return Promise.resolve(handler(path))
    }),
  )
  return paths
}

const ok = (body: unknown) => new Response(JSON.stringify(body), { status: 200 })

function attach(since = 0) {
  const events: RunEvent[] = []
  // Batches, not just their contents: the empty batch IS the message - it is
  // how the stream says "drop what you were holding" - and an assertion on a
  // flat accumulator cannot tell it from no call at all.
  const batches: RunEvent[][] = []
  const refusals: string[] = []
  const states: [string, string][] = []
  const conn = connectRun(
    'r1',
    {
      onEvents: (batch) => {
        batches.push(batch)
        events.push(...batch)
      },
      onRefusal: (e) => refusals.push(e.error),
      onStatus: (s, why) => states.push([s, why ?? '']),
    },
    { since, random: () => 0 },
  )
  return { events, batches, refusals, states, conn }
}

beforeEach(() => {
  installFakeSocket()
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

test('catches up over HTTP before it dials, and resumes from what it holds', async () => {
  const paths = stubFetch(() => ok([event(4), event(5)]))
  const { events, conn } = attach(3)
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1))

  expect(paths[0]).toBe('/api/runs/r1/events?since=3')
  expect(events.map((e) => e.seq)).toEqual([4, 5])
  // The socket resumes from the last event the catch-up delivered, not from the
  // cursor the caller started with: everything in between is already applied.
  expect(FakeSocket.last.url).toContain('/api/runs/r1/socket?since=5')
  conn.close()
})

test('delivers live events in order and tracks the sequence', async () => {
  stubFetch(() => ok([]))
  const { events, conn } = attach(0)
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
  FakeSocket.last.open()
  FakeSocket.last.deliver(event(1))
  FakeSocket.last.deliver(event(2))

  expect(events.map((e) => e.seq)).toEqual([1, 2])
  conn.close()
})

// desync is not an event to draw: it says this client fell behind and the
// socket is about to end. Recovery is a refetch from the sequence it names -
// the last one that did arrive - and then a fresh dial.
test('recovers from a desync at the sequence it names', async () => {
  vi.useFakeTimers()
  let page: RunEvent[] = []
  const paths = stubFetch(() => ok(page))
  const { events, conn } = attach(0)
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
  FakeSocket.last.open()
  FakeSocket.last.deliver(event(1))

  page = [event(9)]
  FakeSocket.last.deliver({ seq: 0, time: '', kind: EventKind.Desync, from: 8 })
  FakeSocket.last.drop()

  await vi.advanceTimersByTimeAsync(2000)
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(2))
  // The refetch starts at 8, not at 1: the events between them reached this
  // client and were applied, and asking from 1 would deliver them twice.
  expect(paths.some((p) => p === '/api/runs/r1/events?since=8')).toBe(true)
  expect(events.map((e) => e.seq)).toEqual([1, 9])
  // Never handed up as an event of its own.
  expect(events.some((e) => e.kind === EventKind.Desync)).toBe(false)
  conn.close()
})

// A refused operation is not something that happened in the conversation. It
// must not be folded into the timeline as an error event.
test('hands a refusal up without putting it in the timeline', async () => {
  stubFetch(() => ok([]))
  const { events, refusals, conn } = attach(0)
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
  FakeSocket.last.open()
  FakeSocket.last.deliver({ kind: 'client_error', op: 'resolve', error: 'approval already decided' })

  expect(refusals).toEqual(['approval already decided'])
  expect(events).toHaveLength(0)
  conn.close()
})

test('sends an operation only while the socket is open', async () => {
  stubFetch(() => ok([]))
  const { conn } = attach(0)
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
  expect(conn.send({ op: 'interrupt' })).toBe(false)

  FakeSocket.last.open()
  expect(conn.send({ op: 'interrupt' })).toBe(true)
  expect(FakeSocket.last.sent).toEqual(['{"op":"interrupt"}'])
  conn.close()
})

// A browser cannot read the status of a failed handshake, so the only way to
// tell "this run is gone" from "the daemon restarted" is to ask over HTTP -
// and a page that kept dialling a deleted run would never stop.
test('stops dialling a run the daemon says is gone', async () => {
  vi.useFakeTimers()
  stubFetch((path) =>
    path.includes('/events') ? ok([]) : new Response('no such run', { status: 404 }),
  )
  const { states, conn } = attach(0)
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
  FakeSocket.last.open()
  FakeSocket.last.drop()

  await vi.waitFor(() => expect(states.some(([s]) => s === 'gone')).toBe(true))
  await vi.advanceTimersByTimeAsync(60_000)
  expect(FakeSocket.instances).toHaveLength(1)
  conn.close()
})

test('reconnects when the daemon simply hangs up', async () => {
  vi.useFakeTimers()
  stubFetch((path) => (path.includes('/events') ? ok([]) : ok({ id: 'r1', live: true })))
  const { conn } = attach(0)
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
  FakeSocket.last.open()
  FakeSocket.last.drop()

  await vi.advanceTimersByTimeAsync(2000)
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(2))
  conn.close()
})

// 410 means the history no longer reaches the point asked for. Retrying the
// same cursor answers 410 forever; the fix is to read the timeline again from
// the start, and to tell the page to throw away what it had.
test('reloads the timeline from the start when the history is gone', async () => {
  let gone = true
  const paths = stubFetch((path) => {
    if (!path.includes('/events')) return ok({ id: 'r1', live: true })
    if (gone && path.endsWith('since=90')) {
      gone = false
      return new Response('the history no longer reaches that point', { status: 410 })
    }
    return ok([])
  })
  const { batches, conn } = attach(90)
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1))

  expect(paths[0]).toBe('/api/runs/r1/events?since=90')
  // The empty batch is the instruction to drop what was held; without it the
  // reloaded timeline is appended to the one that is already there.
  expect(batches).toEqual([[]])
  expect(FakeSocket.last.url).toContain('since=0')
  conn.close()
})

// A live event has to move the cursor, or every reconnect asks the daemon to
// replay a tail this client already applied and the transcript doubles.
test('a live event moves the cursor a reconnection resumes from', async () => {
  vi.useFakeTimers()
  stubFetch((path) => (path.includes('/events') ? ok([]) : ok({ id: 'r1', live: true })))
  const { conn } = attach(0)
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
  FakeSocket.last.open()
  FakeSocket.last.deliver(event(7))
  FakeSocket.last.drop()

  await vi.advanceTimersByTimeAsync(2000)
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(2))
  expect(FakeSocket.last.url).toContain('since=7')
  conn.close()
})

// A page that came back full is the signal to ask again; this API has no "more"
// marker. Stopping at the first page loses the middle of a long conversation
// and then dials the socket from a stale cursor.
test('pages until the catch-up runs out', async () => {
  const full = Array.from({ length: 2000 }, (_, i) => event(i + 1))
  const paths = stubFetch((path) => {
    if (!path.includes('/events')) return ok({ id: 'r1', live: true })
    return ok(path.endsWith('since=0') ? full : [event(2001)])
  })
  const { events, conn } = attach(0)
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1))

  expect(paths.filter((p) => p.includes('/events'))).toEqual([
    '/api/runs/r1/events?since=0',
    '/api/runs/r1/events?since=2000',
  ])
  expect(events).toHaveLength(2001)
  expect(FakeSocket.last.url).toContain('since=2001')
  conn.close()
})

// The catch-up learns the run is gone before the socket ever gets a chance to
// fail, and must not go on to open one.
test('does not dial a run the catch-up already found gone', async () => {
  vi.useFakeTimers()
  stubFetch((path) =>
    path.includes('/events') ? new Response('no such run', { status: 404 }) : ok({ live: true }),
  )
  const { states, conn } = attach(0)
  await vi.waitFor(() => expect(states.some(([s]) => s === 'gone')).toBe(true))
  await vi.advanceTimersByTimeAsync(60_000)
  expect(FakeSocket.instances).toHaveLength(0)
  conn.close()
})

test('closing stops it reconnecting', async () => {
  vi.useFakeTimers()
  stubFetch((path) => (path.includes('/events') ? ok([]) : ok({ id: 'r1', live: true })))
  const { conn } = attach(0)
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
  FakeSocket.last.open()
  conn.close()
  await vi.advanceTimersByTimeAsync(60_000)
  expect(FakeSocket.instances).toHaveLength(1)
})

// The record's `live` is the terminal signal, not a status code: GET
// /api/runs/{id} describes a closed run perfectly well and answers 200 for it.
// Waiting for a 409 there is waiting for something the route never sends - and
// every run is closed after a daemon restart, so a tab would redial a closed
// conversation for as long as it stayed open.
test('stops dialling a conversation the record says is closed', async () => {
  vi.useFakeTimers()
  stubFetch((path) =>
    path.includes('/events') ? ok([]) : ok({ id: 'r1', live: false, status: 'closed' }),
  )
  const { states, conn } = attach(0)
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
  FakeSocket.last.open()
  FakeSocket.last.drop()

  await vi.waitFor(() => expect(states.some(([s]) => s === 'gone')).toBe(true))
  expect(states.find(([s]) => s === 'gone')?.[1]).toContain('closed')
  await vi.advanceTimersByTimeAsync(60_000)
  expect(FakeSocket.instances).toHaveLength(1)
  conn.close()
})

// The presence event is what shows the other tabs who is attached; a socket
// that names nobody makes it useless.
test('names itself on the socket so the other clients can see it', async () => {
  stubFetch(() => ok([]))
  const conn = connectRun(
    'r1',
    { onEvents: () => undefined, onRefusal: () => undefined },
    { label: 'a browser' },
  )
  await vi.waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
  expect(FakeSocket.last.url).toContain('label=a+browser')
  conn.close()
})
