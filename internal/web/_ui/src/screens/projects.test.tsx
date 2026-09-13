import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'
import type { Project } from '@/lib/wire'
import { DAEMON_PROJECT, mountApp } from '@/test/harness'

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
