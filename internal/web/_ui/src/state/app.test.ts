import { afterEach, expect, test, vi } from 'vitest'
import { resync } from '@/lib/route'
import { compose, initialState, latestRunId, runCounts, store } from './app'
import type { Run } from '@/lib/wire'

afterEach(() => {
  vi.unstubAllGlobals()
  store.set(initialState())
})

const run = (over: Partial<Run>): Run => ({
  id: 'r',
  mode: 'interactive',
  status: 'open',
  created: '',
  updated: '',
  live: true,
  ...over,
})

// Three separate flags, counted five places before this existed. The status bar
// says "N running" and "N needs attention", and swapping them tells a person
// that nothing wants them when something does.
test('counts the three run states apart', () => {
  store.set((s) => ({
    ...s,
    // Deliberately different counts: with one of each, swapping two of the
    // three flags is a mutation the assertion cannot see.
    runs: [
      run({ id: 'a', running: true }),
      run({ id: 'b', running: true }),
      run({ id: 'c', waiting: true }),
      run({ id: 'd' }),
      run({ id: 'e' }),
      run({ id: 'f', live: false, status: 'closed' }),
    ],
  }))
  expect(runCounts(store.get())).toEqual({ live: 5, running: 2, waiting: 1 })
})

// The one fallback App.tsx's `runId` and `compose()` both read: prefer the
// run the tab is already attached to, else the newest, preferring a live one.
test('the run the tab is attached to wins, then a live run, then the newest', () => {
  const runs = [run({ id: 'a', live: true }), run({ id: 'b', live: false, status: 'closed' })]
  expect(latestRunId(runs, '')).toBe('a')
  expect(latestRunId(runs, 'b')).toBe('b')
  expect(latestRunId([], '')).toBeUndefined()
})

// `compose` puts a command in the address bar as well as the store, and the
// id it picks has to be the same conversation App.tsx's own fallback would
// land on - otherwise the composer it navigates to is not the one the person
// is actually looking at.
test('compose lands on the conversation the address bar can name', () => {
  // Off the chat screen: composing from the same route it lands on would
  // navigate nowhere and leave the address bar exactly as this left it.
  const reset = () => {
    window.history.replaceState(null, '', '/models')
    resync()
  }

  reset()
  store.set((s) => ({ ...s, runs: [run({ id: 'a', live: true })] }))
  compose('/x ')
  expect(window.location.pathname).toBe('/chat/a')

  reset()
  store.set((s) => ({ ...s, runs: [], activeRun: '' }))
  compose('/x ')
  expect(window.location.pathname).toBe('/chat')
})

// A run screen names a conversation too, and it can be a closed one a live run
// would otherwise outrank - the address bar says which tab this is, and
// compose has to honour it rather than the fallback that ignores it.
test('compose honours the run the address bar names, even over a live one', () => {
  window.history.replaceState(null, '', '/run/r-9')
  resync()
  store.set((s) => ({
    ...s,
    runs: [run({ id: 'r-9', live: false, status: 'closed' }), run({ id: 'r-1', live: true })],
  }))
  compose('/x ')
  expect(window.location.pathname).toBe('/chat/r-9')
})

/**
 * The activity feed is read from its end, and `since` is a cursor: `limit`
 * takes the first N *after* it. The page has to walk to the end, and the walk
 * has to terminate on every answer the daemon can give.
 */
function feed(total: number) {
  const paths: string[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn((path: string) => {
      paths.push(path)
      if (path === '/api/auth/session') return Promise.resolve(new Response(null, { status: 204 }))
      if (path.startsWith('/api/activity')) {
        const since = Number(new URL(path, 'http://x').searchParams.get('since') ?? 0)
        const page = Array.from({ length: Math.min(1000, Math.max(0, total - since)) }, (_, i) => ({
          seq: since + i + 1,
          at: '2026-09-08T12:00:00Z',
          kind: 'run.opened',
          text: `entry ${since + i + 1}`,
        }))
        return Promise.resolve(new Response(JSON.stringify(page), { status: 200 }))
      }
      return Promise.resolve(new Response('[]', { status: 200 }))
    }),
  )
  return paths
}

test('reads the end of a long feed, newest first', async () => {
  feed(2500)
  store.set((s) => ({
    ...s,
    meta: { version: '', defaultModel: '', rev: 0, ui: true, features: { activity: true } },
  }))
  const { refresh } = await import('./app')
  await refresh.activity()

  const held = store.get().activity
  expect(held).toHaveLength(200)
  expect(held[0]?.seq).toBe(2500)
  expect(held[held.length - 1]?.seq).toBe(2301)
})

// A daemon that answers a full page whose last cursor does not advance would
// otherwise spin the loop forever and freeze the tab. It has to be a full page:
// a short one ends the walk for its own reason.
test('gives up on a cursor that does not advance', async () => {
  const stuck = Array.from({ length: 1000 }, () => ({ seq: 1, kind: 'x', text: 'stuck' }))
  const asked: string[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn((path: string) => {
      if (path.startsWith('/api/activity')) asked.push(path)
      return Promise.resolve(
        path.startsWith('/api/activity')
          ? new Response(JSON.stringify(stuck), { status: 200 })
          : new Response('[]', { status: 200 }),
      )
    }),
  )
  store.set((s) => ({
    ...s,
    meta: { version: '', defaultModel: '', rev: 0, ui: true, features: { activity: true } },
  }))
  const { refresh } = await import('./app')
  await refresh.activity()
  // Two requests, not fifty: the walk ends because the cursor did not move,
  // not because it ran out of patience.
  expect(asked).toHaveLength(2)
  expect(store.get().activity).toHaveLength(200)
})
