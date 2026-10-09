import { act, fireEvent, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
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

async function openTickets(tickets: Ticket[], routes: Record<string, () => Response | Promise<Response>> = {}) {
  const h = await mountApp({
    meta: META_TICKETS,
    projects: [DAEMON_PROJECT, PRJ],
    path: '/tickets',
    routes: { '/api/projects/PRJ-1/tickets': () => json(tickets), ...routes },
  })
  act(() => selectProject('PRJ-1'))
  await screen.findByRole('grid', { name: 'Tickets' })
  return h
}

const PLAN = [
  ticket('TCK-1', { title: 'Delete sessions', status: 'running', progress: { done: 1, total: 2 } }),
  ticket('TCK-2', { title: 'Journal helper', status: 'done', parent: 'TCK-1' }),
  ticket('TCK-3', { title: 'HTTP endpoint', status: 'ready', parent: 'TCK-1', dependsOn: ['TCK-4'] }),
  ticket('TCK-4', { title: 'Runner change', status: 'running' }),
]

test('the tree shows subtickets under their parent with progress and waits', async () => {
  await openTickets(PLAN)
  expect(await screen.findByText('Delete sessions')).toBeInTheDocument()
  expect(screen.getByText('1/2 done')).toBeInTheDocument()
  expect(screen.getByText(/waits for TCK-4/)).toBeInTheDocument()
  expect(screen.queryByText('Journal helper')).not.toBeInTheDocument()
  await userEvent.click(screen.getByRole('radio', { name: 'All' }))
  expect(screen.getByText('Journal helper')).toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'Collapse TCK-1' }))
  expect(screen.queryByText('HTTP endpoint')).not.toBeInTheDocument()
})

test('a row opens the ticket page', async () => {
  await openTickets(PLAN)
  await userEvent.click(await screen.findByText('Runner change'))
  expect(window.location.pathname).toBe('/task/TCK-4')
})

test('the new ticket dialog sends the form and shows a refusal', async () => {
  let created = 0
  const h = await openTickets(PLAN, {
    'POST /api/projects/PRJ-1/tickets': () => {
      created++
      return created === 1
        ? new Response('TCK-2 is a subticket and cannot have subtickets', { status: 409 })
        : new Response(JSON.stringify(ticket('TCK-5', { title: 'New one' })), { status: 201 })
    },
  })
  await userEvent.click(screen.getByRole('button', { name: 'New ticket' }))
  await userEvent.type(screen.getByLabelText('Title'), 'New one')
  fireEvent.change(screen.getByLabelText('Parent'), { target: { value: 'TCK-1' } })
  await userEvent.click(screen.getByRole('button', { name: 'Create ticket' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('cannot have subtickets')
  await userEvent.click(screen.getByRole('button', { name: 'Create ticket' }))
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  const post = h.sent.filter((s) => s.method === 'POST').pop()
  expect(JSON.parse(post!.body)).toMatchObject({ title: 'New one', parent: 'TCK-1' })
})

test('without a project the screen says to choose one', async () => {
  await mountApp({ meta: META_TICKETS, projects: [DAEMON_PROJECT, PRJ], path: '/tickets' })
  expect(await screen.findByText('Tickets need a project.')).toBeInTheDocument()
})
