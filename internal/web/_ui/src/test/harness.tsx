/**
 * A daemon the tests can drive.
 *
 * Everything the application does goes through `fetch` and `WebSocket`, so a
 * fake of both is the whole seam - and it is the right one: it exercises the
 * real api client, the real control stream, the real store and the real
 * components, and the only thing it replaces is the process on the other end.
 */

import { act, render, screen, waitFor } from '@testing-library/react'
import { vi } from 'vitest'
import App from '@/App'
import { initialState, store } from '@/state/app'
import { setInspectorContent } from '@/state/inspector'
import type { Activity, Command, Meta, Model, Run, RunEvent, Skills } from '@/lib/wire'
import { FakeSocket, installFakeSocket } from './fakesocket'

export type Daemon = {
  meta?: Partial<Meta>
  runs?: Run[]
  models?: Model[]
  skills?: Skills
  commands?: Command[]
  activity?: Activity[]
  events?: RunEvent[]
  /** Answers for routes a case wants to fail or shape by hand. */
  routes?: Record<string, () => Response>
}

export const META: Meta = {
  version: '1.2.3-test',
  defaultModel: 'anthropic/claude-opus-5',
  rev: 1,
  ui: true,
  features: {
    controlSocket: true,
    runs: true,
    models: true,
    skills: true,
    commands: true,
    usage: true,
    activity: true,
    providerLogin: true,
  },
}

export const RUN: Run = {
  id: 'r-1',
  sessionId: 's-1',
  mode: 'interactive',
  title: 'Rotate the signing keys',
  model: 'anthropic/claude-opus-5',
  root: '/home/dev/aigem',
  status: 'open',
  created: '2026-09-08T12:00:00Z',
  updated: '2026-09-08T12:02:00Z',
  live: true,
  seq: 4,
}

const ok = (body: unknown) => new Response(JSON.stringify(body), { status: 200 })

export type Harness = {
  /** Every path the page asked for, in order. */
  paths: string[]
  /** The bodies of every non-GET request, in order. */
  sent: { path: string; method: string; body: string }[]
  /** Deliver a control frame, as the daemon would. */
  publish: (type: string, rev: number, data?: unknown) => void
  /** Deliver a run event down the run socket. */
  emit: (event: RunEvent) => void
  runSocket: () => FakeSocket | undefined
}

/**
 * The viewport a case renders at. jsdom's default is 1024, which is below the
 * design's narrow breakpoint - so without this every test would run against the
 * layout that hides the inspector, and the ordinary one would go untested.
 */
export function setViewport(width: number) {
  Object.defineProperty(window, 'innerWidth', { value: width, configurable: true })
  window.dispatchEvent(new Event('resize'))
}

/** Install the fakes and answer the API from `daemon`. Call before rendering. */
export function installDaemon(daemon: Daemon = {}): Harness {
  const meta = { ...META, ...daemon.meta }
  const paths: string[] = []
  const sent: { path: string; method: string; body: string }[] = []

  installFakeSocket()
  window.history.replaceState(null, '', '/')
  Object.defineProperty(window, 'innerWidth', { value: 1440, configurable: true })
  store.set(initialState())
  setInspectorContent(null)

  vi.stubGlobal(
    'fetch',
    vi.fn((path: string, init?: RequestInit) => {
      paths.push(path)
      const method = init?.method ?? 'GET'
      if (method !== 'GET') {
        sent.push({ path, method, body: typeof init?.body === 'string' ? init.body : '' })
      }
      const custom = daemon.routes?.[`${method} ${path}`] ?? daemon.routes?.[path]
      if (custom) return Promise.resolve(custom())
      if (path === '/api/auth/session') return Promise.resolve(new Response(null, { status: 204 }))
      if (path === '/api/meta') return Promise.resolve(ok(meta))
      if (path === '/api/runs') return Promise.resolve(ok(daemon.runs ?? []))
      if (path === '/api/models') return Promise.resolve(ok(daemon.models ?? []))
      if (path === '/api/skills') return Promise.resolve(ok(daemon.skills ?? { items: [] }))
      if (path === '/api/commands') return Promise.resolve(ok(daemon.commands ?? []))
      if (path === '/api/usage') return Promise.resolve(ok([]))
      if (path.startsWith('/api/activity')) return Promise.resolve(ok(daemon.activity ?? []))
      if (path.includes('/events')) return Promise.resolve(ok(daemon.events ?? []))
      if (/^\/api\/runs\/[^/]+$/.test(path)) {
        const id = path.split('/')[3]
        const run = (daemon.runs ?? []).find((r) => r.id === id)
        return Promise.resolve(run ? ok(run) : new Response('no such run', { status: 404 }))
      }
      return Promise.resolve(new Response('not stubbed: ' + path, { status: 404 }))
    }),
  )

  const control = () => FakeSocket.instances.find((s) => s.url.includes('/api/socket'))
  const runSocket = () => FakeSocket.instances.filter((s) => s.url.includes('/socket?')).pop()

  return {
    paths,
    sent,
    // Wrapped in act: a frame off a socket is state arriving from outside
    // React, which is exactly what act exists to flush.
    publish: (type, rev, data) => act(() => control()?.deliver({ type, rev, data })),
    emit: (event) => act(() => runSocket()?.deliver(event)),
    runSocket,
  }
}

/** Render the application and wait until it is past the sign-in. */
export async function mountApp(daemon: Daemon = {}): Promise<Harness> {
  const harness = installDaemon(daemon)
  render(<App />)
  // The control socket is opened once the page has a cookie; hello is what
  // takes it off "connecting".
  await waitFor(() => {
    const socket = FakeSocket.instances.find((s) => s.url.includes('/api/socket'))
    if (!socket) throw new Error('the control socket was never opened')
    socket.open()
    socket.deliver({ type: 'hello', rev: 1, data: { ...META, ...daemon.meta } })
  })
  await screen.findByRole('banner')
  return harness
}

export { screen, waitFor }
