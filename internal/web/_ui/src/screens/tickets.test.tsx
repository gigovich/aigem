import { act } from '@testing-library/react'
import { expect, test } from 'vitest'
import type { Ticket } from '@/lib/wire'
import { selectProject, store } from '@/state/app'
import { DAEMON_PROJECT, mountApp, waitFor } from '@/test/harness'

const PRJ = { id: 'PRJ-1', name: 'work', dir: '/home/dev/work' }
const META_TICKETS = {
  features: {
    controlSocket: true, runs: true, models: true, skills: true, commands: true, usage: true,
    activity: true, providerLogin: true, projects: true, tickets: true,
  },
}
const ticket = (id: string, extra: Partial<Ticket> = {}): Ticket => ({
  id, repo: '', title: `Title ${id}`, body: '', status: 'open', dependsOn: [], by: 'you',
  comments: [], runs: [], runnable: false, ...extra,
})
const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200 })

test('tickets load for the selected project and follow a switch', async () => {
  const other = { id: 'PRJ-2', name: 'other', dir: '/home/dev/other' }
  await mountApp({
    meta: META_TICKETS,
    projects: [DAEMON_PROJECT, PRJ, other],
    routes: {
      '/api/projects/PRJ-1/tickets': () => json([ticket('TCK-1')]),
      '/api/projects/PRJ-2/tickets': () => json([ticket('TCK-7')]),
    },
  })
  expect(store.get().tickets).toEqual([])
  act(() => selectProject('PRJ-1'))
  await waitFor(() => expect(store.get().tickets.map((t) => t.id)).toEqual(['TCK-1']))
  act(() => selectProject('PRJ-2'))
  expect(store.get().tickets).toEqual([])
  await waitFor(() => expect(store.get().tickets.map((t) => t.id)).toEqual(['TCK-7']))
})

test('ticket.updated rereads, and an older answer does not win over a newer one', async () => {
  let answer: (r: Response) => void = () => {}
  let calls = 0
  const h = await mountApp({
    meta: META_TICKETS,
    projects: [DAEMON_PROJECT, PRJ],
    routes: {
      '/api/projects/PRJ-1/tickets': () => {
        calls++
        if (calls === 2) return new Promise<Response>((r) => (answer = r))
        return json(calls === 1 ? [ticket('TCK-1')] : [ticket('TCK-1'), ticket('TCK-2')])
      },
    },
  })
  act(() => selectProject('PRJ-1'))
  await waitFor(() => expect(store.get().tickets).toHaveLength(1))
  h.publish('ticket.updated', 2, { projectId: 'PRJ-1', id: 'TCK-2' })
  h.publish('ticket.updated', 3, { projectId: 'PRJ-1', id: 'TCK-2' })
  await waitFor(() => expect(store.get().tickets).toHaveLength(2))
  await act(() => Promise.resolve(answer(json([]))))
  expect(store.get().tickets).toHaveLength(2)
})
