import { act, fireEvent, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { expect, test } from 'vitest'
import { navigate } from '@/lib/route'
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

test('the collapse button is reached with Tab and works from the keyboard', async () => {
  await openTickets(PLAN)
  const row = screen.getByText('Delete sessions').closest<HTMLElement>('[role="row"]')!
  act(() => row.focus())
  await userEvent.tab()
  expect(screen.getByRole('button', { name: 'Collapse TCK-1' })).toHaveFocus()
  await userEvent.keyboard('{Enter}')
  expect(screen.getByRole('button', { name: 'Expand TCK-1' })).toBeInTheDocument()
  expect(screen.queryByText('HTTP endpoint')).not.toBeInTheDocument()
  expect(window.location.pathname).toBe('/tickets')
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

async function openTask(id: string, tickets: Ticket[], routes: Record<string, () => Response | Promise<Response>> = {}) {
  const h = await mountApp({
    meta: META_TICKETS,
    projects: [DAEMON_PROJECT, PRJ],
    path: `/task/${id}`,
    routes: { '/api/projects/PRJ-1/tickets': () => json(tickets), ...routes },
  })
  act(() => selectProject('PRJ-1'))
  await screen.findByRole('heading', { level: 1, name: tickets.find((t) => t.id === id)!.title })
  return h
}

test('a subticket page shows its parent, waits, blocks and only allowed moves', async () => {
  const tickets = [
    ...PLAN,
    ticket('TCK-5', { title: 'Trash button', status: 'ready', parent: 'TCK-1', dependsOn: ['TCK-3'] }),
  ]
  const h = await openTask('TCK-3', tickets, {
    'PATCH /api/projects/PRJ-1/tickets/TCK-3': () => json(ticket('TCK-3', { status: 'open' })),
  })
  expect(screen.getByRole('button', { name: '‹ TCK-1' })).toBeInTheDocument()
  expect(screen.getByText(/waits for TCK-4/)).toBeInTheDocument()
  expect(screen.getByRole('link', { name: 'TCK-5' })).toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Back to open' })).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'Reopen' })).not.toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'Back to open' }))
  const patchReq = h.sent.find((s) => s.method === 'PATCH')
  expect(JSON.parse(patchReq!.body)).toEqual({ status: 'open' })
})

test('adding a dependency that makes a cycle shows the daemon sentence', async () => {
  await openTask('TCK-4', PLAN, {
    'PATCH /api/projects/PRJ-1/tickets/TCK-4': () =>
      new Response('that makes a cycle: TCK-4 -> TCK-3 -> TCK-4', { status: 409 }),
  })
  fireEvent.change(screen.getByLabelText('Add dependency'), { target: { value: 'TCK-3' } })
  expect(await screen.findByRole('alert')).toHaveTextContent('makes a cycle')
})

test('a parent page lists its subtickets and has no status buttons', async () => {
  await openTask('TCK-1', PLAN)
  expect(screen.getByText('1/2 done')).toBeInTheDocument()
  expect(screen.getByText('HTTP endpoint')).toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Add subticket' })).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'Close' })).not.toBeInTheDocument()
  expect(screen.getByText('1 of 2 done')).toBeInTheDocument()
})

test('a subticket shows its parent progress and cannot be split; a top-level ticket can', async () => {
  await openTask('TCK-3', PLAN)
  expect(screen.getByText('1 of 2 done')).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'Add subticket' })).not.toBeInTheDocument()
  act(() => navigate({ screen: 'task', id: 'TCK-4' }))
  expect(await screen.findByRole('button', { name: 'Add subticket' })).toBeInTheDocument()
})

test('a draft comment does not follow to another ticket', async () => {
  await openTask('TCK-3', PLAN)
  await userEvent.click(screen.getByRole('radio', { name: /Discussion/ }))
  await userEvent.type(screen.getByLabelText('Comment'), 'half typed')
  await userEvent.click(screen.getByRole('link', { name: 'TCK-4' }))
  await screen.findByRole('heading', { level: 1, name: 'Runner change' })
  await userEvent.click(screen.getByRole('radio', { name: /Discussion/ }))
  expect(screen.getByLabelText('Comment')).toHaveValue('')
})

test('a comment is sent and the discussion shows who wrote what', async () => {
  const withComments = PLAN.map((t) =>
    t.id === 'TCK-4'
      ? { ...t, by: 'run RUN-12', comments: [{ at: '2026-10-09T10:00:00Z', by: 'run RUN-12', text: 'Split from TCK-1.' }] }
      : t,
  )
  const h = await openTask('TCK-4', withComments, {
    'POST /api/projects/PRJ-1/tickets/TCK-4/comments': () =>
      new Response(JSON.stringify(withComments[3]), { status: 201 }),
  })
  expect(screen.getByText(/created by run RUN-12/)).toBeInTheDocument()
  await userEvent.click(screen.getByRole('radio', { name: /Discussion/ }))
  expect(screen.getByText('Split from TCK-1.')).toBeInTheDocument()
  await userEvent.type(screen.getByLabelText('Comment'), 'Keep the 404')
  await userEvent.click(screen.getByRole('button', { name: 'Send comment' }))
  await waitFor(() => expect(h.sent.some((s) => s.path.endsWith('/comments'))).toBe(true))
})

test('an unknown ticket says so', async () => {
  await mountApp({ meta: META_TICKETS, projects: [DAEMON_PROJECT, PRJ], path: '/task/TCK-99' })
  act(() => selectProject('PRJ-1'))
  expect(await screen.findByText('No such ticket.')).toBeInTheDocument()
})
