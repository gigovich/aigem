import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'
import type { Project } from '@/lib/wire'
import { navigate } from '@/lib/route'
import { DAEMON_PROJECT, mountApp, RUN } from '@/test/harness'

afterEach(() => {
  vi.unstubAllGlobals()
  window.localStorage.removeItem('aigem.project')
})

const WORK: Project = { id: 'PRJ-1', name: 'work', dir: '/home/dev/work' }
const PROJECTS = [DAEMON_PROJECT, WORK]

test('the sidebar lists the projects and remembers the chosen one', async () => {
  const user = userEvent.setup()
  const h = await mountApp({ projects: PROJECTS })
  const list = await screen.findByRole('list', { name: 'Projects' })
  expect(within(list).getByRole('button', { name: /aigem/ })).toHaveAttribute('aria-current', 'true')

  await user.click(within(list).getByRole('button', { name: /work/ }))
  expect(within(list).getByRole('button', { name: /work/ })).toHaveAttribute('aria-current', 'true')
  expect(window.localStorage.getItem('aigem.project')).toBe('PRJ-1')
  await waitFor(() => expect(h.paths).toContain('/api/skills?project=PRJ-1'))
})

test('a project whose environment failed to load says so', async () => {
  await mountApp({ projects: [DAEMON_PROJECT, { ...WORK, loadError: 'hook exited 1' }] })
  const list = await screen.findByRole('list', { name: 'Projects' })
  expect(within(list).getByRole('button', { name: /work/ })).toHaveTextContent('failed to load')
})

test('a new project is added from the dialog, and a refusal is shown as text', async () => {
  const user = userEvent.setup()
  let attempts = 0
  const h = await mountApp({
    projects: PROJECTS,
    routes: {
      'POST /api/projects': () => {
        attempts++
        return attempts === 1
          ? new Response('cannot add that project: /nope is not a directory', { status: 400 })
          : new Response(JSON.stringify({ id: 'PRJ-2', name: 'thing', dir: '/home/dev/thing' }), {
              status: 201,
            })
      },
    },
  })
  await user.click(await screen.findByRole('button', { name: 'New project' }))
  const dialog = await screen.findByRole('dialog', { name: 'New project' })
  await user.type(within(dialog).getByRole('textbox', { name: /Directory/ }), '/nope')
  await user.click(within(dialog).getByRole('button', { name: 'Add project' }))
  expect(await within(dialog).findByRole('alert')).toHaveTextContent('/nope is not a directory')

  await user.clear(within(dialog).getByRole('textbox', { name: /Directory/ }))
  await user.type(within(dialog).getByRole('textbox', { name: /Directory/ }), '/home/dev/thing')
  await user.click(within(dialog).getByRole('button', { name: 'Add project' }))
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'New project' })).not.toBeInTheDocument())
  expect(h.sent.filter((r) => r.path === '/api/projects').pop()?.body).toBe('{"dir":"/home/dev/thing"}')
  expect(window.localStorage.getItem('aigem.project')).toBe('PRJ-2')
})

test('/projects/{id} selects the project and lands on the sessions', async () => {
  await mountApp({ projects: PROJECTS, path: '/projects/PRJ-1' })
  await waitFor(() => expect(window.location.pathname).toBe('/chat'))
  const list = await screen.findByRole('list', { name: 'Projects' })
  expect(within(list).getByRole('button', { name: /work/ })).toHaveAttribute('aria-current', 'true')
})

test('without the projects feature the sidebar offers nothing to add', async () => {
  await mountApp({ meta: { features: { controlSocket: true, runs: true } } })
  await screen.findByRole('navigation', { name: 'Navigation' })
  expect(screen.queryByRole('button', { name: 'New project' })).not.toBeInTheDocument()
  expect(screen.getByText("This daemon's directory.")).toBeInTheDocument()
})

test('the session list shows the current project, and "All projects" widens it', async () => {
  const user = userEvent.setup()
  const theirs = { ...RUN, id: 'r-2', title: 'Theirs', projectId: 'PRJ-1' }
  await mountApp({ projects: PROJECTS, runs: [RUN, theirs] })
  const list = await screen.findByRole('list', { name: 'Sessions' })
  expect(within(list).getByText('Rotate the signing keys')).toBeInTheDocument()
  expect(within(list).queryByText('Theirs')).not.toBeInTheDocument()

  await user.click(screen.getByRole('button', { name: 'All projects' }))
  expect(within(list).getByText('Theirs')).toBeInTheDocument()

  await user.click(screen.getByRole('button', { name: 'All projects' }))
  const projects = screen.getByRole('list', { name: 'Projects' })
  await user.click(within(projects).getByRole('button', { name: /work/ }))
  expect(within(list).getByText('Theirs')).toBeInTheDocument()
  expect(within(list).queryByText('Rotate the signing keys')).not.toBeInTheDocument()
})

test('a new session opens in the chosen project', async () => {
  const user = userEvent.setup()
  const h = await mountApp({
    projects: PROJECTS,
    routes: {
      'POST /api/runs': () =>
        new Response(JSON.stringify({ ...RUN, id: 'r-9', projectId: 'PRJ-1' }), { status: 201 }),
    },
  })
  const projects = await screen.findByRole('list', { name: 'Projects' })
  await user.click(within(projects).getByRole('button', { name: /work/ }))
  await user.click(screen.getByRole('button', { name: 'New session' }))
  await waitFor(() =>
    expect(h.sent.find((r) => r.path === '/api/runs')?.body).toBe('{"projectId":"PRJ-1"}'),
  )
})

test('the skills screen reads and trusts the chosen project', async () => {
  const user = userEvent.setup()
  const detail = {
    name: 'review',
    description: 'd',
    userInvocable: true,
    modelInvocable: true,
    allowedTools: [],
    disallowedTools: [],
    paths: [],
    body: 'x',
  }
  const h = await mountApp({
    projects: PROJECTS,
    skills: {
      items: [{ name: 'review', description: 'd', userInvocable: true, modelInvocable: true }],
      pending: { names: ['theirs'] },
    },
    routes: {
      '/api/skills/review?project=PRJ-1': () => new Response(JSON.stringify(detail), { status: 200 }),
      '/api/skills/review': () => new Response(JSON.stringify(detail), { status: 200 }),
      'POST /api/skills/trust': () => new Response('{"loaded":["theirs"],"notices":[]}', { status: 200 }),
    },
  })
  const projects = await screen.findByRole('list', { name: 'Projects' })
  await user.click(within(projects).getByRole('button', { name: /work/ }))
  act(() => navigate({ screen: 'skills' }))
  await waitFor(() => expect(h.paths).toContain('/api/skills/review?project=PRJ-1'))

  await user.click(await screen.findByRole('button', { name: 'Review and load' }))
  await user.click(await screen.findByRole('button', { name: 'Load them' }))
  await waitFor(() =>
    expect(h.sent.find((r) => r.path === '/api/skills/trust')?.body).toBe('{"project":"PRJ-1"}'),
  )
})
