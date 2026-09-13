import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { navigate } from '@/lib/route'
import { EventKind } from '@/lib/wire'
import type { Model, Run, Skills } from '@/lib/wire'
import { mountApp, RUN } from '@/test/harness'

afterEach(() => {
  vi.unstubAllGlobals()
})

beforeEach(() => {
  window.history.replaceState(null, '', '/')
})

const MODELS: Model[] = [
  {
    ref: 'anthropic/claude-opus-5',
    provider: 'anthropic',
    name: 'Claude Opus 5',
    contextWindow: 200000,
    maxTokens: 64000,
    reasoning: true,
    needsAuth: true,
    authenticated: true,
    default: true,
  },
  {
    ref: 'openai/gpt-5',
    provider: 'openai',
    name: 'GPT-5',
    contextWindow: 400000,
    needsAuth: true,
    authenticated: false,
    default: false,
  },
]

const SKILLS: Skills = {
  items: [
    {
      name: 'code-review',
      description: 'Review implementation quality before a run opens a pull request.',
      userInvocable: true,
      modelInvocable: true,
    },
  ],
  pending: { names: ['migration-safety'] },
}

const go = (path: string) => act(() => navigate({ screen: path as 'models' }))

test('models lists what the daemon knows and says which cannot be used', async () => {
  await mountApp({ models: MODELS })
  go('models')

  const grid = await screen.findByRole('grid', { name: 'Models' })
  expect(within(grid).getByText('Claude Opus 5')).toBeInTheDocument()
  expect(within(grid).getByText('default')).toBeInTheDocument()
  expect(within(grid).getByText('200K')).toBeInTheDocument()
  // Colour is not the only carrier: the words say it too.
  expect(within(grid).getByText('No key')).toBeInTheDocument()
  expect(within(grid).getByText('Available')).toBeInTheDocument()
})

// Making a model the default is what every later session opens on, including
// one a terminal starts. It is not something a click should do on its own.
test('models asks before it changes the default, and only then writes', async () => {
  const user = userEvent.setup()
  const h = await mountApp({ models: MODELS })
  go('models')

  await user.click(await screen.findByText('GPT-5'))
  await user.click(await screen.findByRole('button', { name: 'Make default' }))
  const dialog = await screen.findByRole('dialog', { name: /default model/ })
  expect(h.sent.filter((r) => r.path === '/api/models/default')).toHaveLength(0)

  await user.click(within(dialog).getByRole('button', { name: 'Set as default' }))
  await waitFor(() => {
    expect(h.sent.filter((r) => r.path === '/api/models/default')).toHaveLength(1)
  })
  expect(h.sent[h.sent.length - 1]?.body).toBe('{"ref":"openai/gpt-5"}')
})

test('selecting a model fills the inspector', async () => {
  const user = userEvent.setup()
  await mountApp({ models: MODELS })
  go('models')

  await user.click(await screen.findByText('Claude Opus 5'))
  const inspector = await screen.findByRole('complementary', { name: 'Inspector' })
  expect(inspector).toHaveTextContent('Claude Opus 5')
  expect(inspector).toHaveTextContent('anthropic')
})

test('skills lists the catalogue and the definitions awaiting approval', async () => {
  await mountApp({
    skills: SKILLS,
    routes: {
      '/api/skills/code-review': () =>
        new Response(
          JSON.stringify({
            name: 'code-review',
            description: 'Review implementation quality.',
            userInvocable: true,
            modelInvocable: true,
            allowedTools: ['read_file'],
            disallowedTools: [],
            paths: [],
            body: '## Checklist\n\n- reversibility\n',
          }),
          { status: 200 },
        ),
    },
  })
  go('skills')

  expect(await screen.findByRole('heading', { name: 'code-review', level: 1 })).toBeInTheDocument()
  expect(screen.getByText('migration-safety')).toBeInTheDocument()
  // The markdown is rendered as elements, never as HTML.
  expect(await screen.findByRole('heading', { name: 'Checklist' })).toBeInTheDocument()
})

// Loading a project's skills changes what the agent will do on its own in every
// conversation this daemon holds. It gets a question, not a button.
test('a skill a person may invoke can be run from its inspector', async () => {
  const user = userEvent.setup()
  await mountApp({ runs: [RUN], skills: SKILLS })
  go('skills')
  await user.click(await screen.findByRole('button', { name: 'Run in a session' }))

  await waitFor(() => expect(window.location.pathname).toBe('/chat'))
  expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('/skill:code-review ')
})

test('a skill only the model may invoke offers nothing to run', async () => {
  await mountApp({
    skills: { items: [{ ...SKILLS.items[0]!, userInvocable: false }] },
  })
  go('skills')
  expect(await screen.findByRole('heading', { name: 'code-review', level: 1 })).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: /in a session/ })).not.toBeInTheDocument()
})

test('skills asks before it loads a project definition, and only then posts', async () => {
  const user = userEvent.setup()
  const h = await mountApp({
    skills: SKILLS,
    routes: {
      '/api/skills/code-review': () =>
        new Response(
          JSON.stringify({
            name: 'code-review',
            description: '',
            userInvocable: true,
            modelInvocable: true,
            allowedTools: [],
            disallowedTools: [],
            paths: [],
            body: '',
          }),
          { status: 200 },
        ),
      'POST /api/skills/trust': () =>
        new Response(JSON.stringify({ loaded: ['migration-safety'], notices: [] }), { status: 200 }),
    },
  })
  go('skills')

  await user.click(await screen.findByRole('button', { name: 'Review and load' }))
  expect(h.sent.filter((r) => r.path === '/api/skills/trust')).toHaveLength(0)

  const dialog = await screen.findByRole('dialog', { name: /Load this project/ })
  await user.click(within(dialog).getByRole('button', { name: 'Load them' }))
  await waitFor(() => {
    expect(h.sent.filter((r) => r.path === '/api/skills/trust')).toHaveLength(1)
  })
})

test('activity draws the feed newest first and links to the run', async () => {
  const user = userEvent.setup()
  await mountApp({
    runs: [RUN],
    activity: [
      { seq: 1, at: '2026-09-08T12:00:00Z', kind: 'run.opened', text: 'older', runRef: 'r-1' },
      { seq: 2, at: '2026-09-08T12:05:00Z', kind: 'run.closed', text: 'newer' },
    ],
  })
  go('activity')

  // The feed is written oldest first and read newest first, so the reversal is
  // the assertion: "newer" has to come out of the document before "older".
  const rows = await screen.findAllByText(/^(older|newer)$/)
  expect(rows.map((r) => r.textContent)).toEqual(['newer', 'older'])

  await user.click(screen.getByRole('button', { name: 'r-1' }))
  expect(window.location.pathname).toBe('/run/r-1')
})

test('every placeholder screen says what it is waiting for', async () => {
  await mountApp()
  for (const [path, heading] of [
    ['tickets', 'Tickets'],
    ['task', 'Task'],
    ['repos', 'Repositories & worktrees'],
  ] as const) {
    go(path)
    expect(await screen.findByRole('heading', { name: heading, level: 1 })).toBeInTheDocument()
    expect(screen.getByText(/need a project/)).toBeInTheDocument()
  }
})

test('a run screen shows the timeline, the agent tree and the context window', async () => {
  const h = await mountApp({ runs: [RUN] })
  go('run')
  act(() => navigate({ screen: 'run', id: 'r-1' }))
  await waitFor(() => expect(h.runSocket()).toBeTruthy())
  h.runSocket()?.open()
  h.emit({
    seq: 1,
    time: '2026-09-08T12:00:01Z',
    kind: EventKind.SessionMeta,
    id: 's-1',
    text: 'Rotate the signing keys',
    name: 'anthropic/claude-opus-5',
    ctx: 200000,
  })
  h.emit({ seq: 2, time: '2026-09-08T12:00:02Z', kind: EventKind.Usage, tokens: 100000 })
  h.emit({
    seq: 3,
    time: '2026-09-08T12:00:03Z',
    kind: EventKind.AgentStart,
    id: 'a-1',
    agent: 'Research',
  })

  expect(await screen.findByRole('listbox', { name: 'Agent tree' })).toHaveTextContent('Research')
  expect(screen.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '50')
  expect(screen.getByText(/100,000 \/ 200,000 tokens/)).toBeInTheDocument()
})

// Stopping a run is bound up with discarding the worktree it worked in, and
// neither exists yet. A button that quietly did half of that is worse than none.
test('the run screen has no stop button in this phase', async () => {
  await mountApp({ runs: [RUN] })
  act(() => navigate({ screen: 'run', id: 'r-1' }))
  await screen.findByRole('heading', { name: /Rotate the signing keys/ })
  expect(screen.queryByRole('button', { name: /Stop run/i })).not.toBeInTheDocument()
})

const CLOSED: Run = { ...RUN, id: 'r-2', live: false, status: 'closed', title: 'An old one' }

test('the session list shows both live and closed conversations', async () => {
  await mountApp({ runs: [RUN, CLOSED] })
  const list = await screen.findByRole('list', { name: 'Sessions' })
  expect(within(list).getByText('Rotate the signing keys')).toBeInTheDocument()
  expect(within(list).getByText('An old one')).toBeInTheDocument()
})

// The tree is a listbox, and a listbox nothing can select is a role promising a
// keyboard contract it does not keep.
test('the agent tree can be selected from the keyboard', async () => {
  const user = userEvent.setup()
  const h = await mountApp({ runs: [RUN] })
  act(() => navigate({ screen: 'run', id: 'r-1' }))
  await waitFor(() => expect(h.runSocket()).toBeTruthy())
  act(() => h.runSocket()?.open())
  h.emit({ seq: 1, time: '2026-09-08T12:00:01Z', kind: EventKind.SessionMeta, name: 'p/m' })
  h.emit({
    seq: 2,
    time: '2026-09-08T12:00:02Z',
    kind: EventKind.AgentStart,
    id: 'a-1',
    agent: 'Research',
  })

  const tree = await screen.findByRole('listbox', { name: 'Agent tree' })
  const options = within(tree).getAllByRole('option')
  expect(options).toHaveLength(2)
  // One tab stop for the widget, the arrows inside it.
  expect(options.filter((o) => o.getAttribute('tabindex') === '0')).toHaveLength(1)

  // The arrows move the focus; Enter chooses. Selection does not follow focus,
  // because choosing a node here filters the stream and arrowing past one
  // should not.
  options[0]?.focus()
  await user.keyboard('{ArrowDown}')
  expect(options[1]).toHaveFocus()
  expect(options[1]).toHaveAttribute('aria-selected', 'false')

  await user.keyboard('{Enter}')
  expect(screen.getAllByRole('option')[1]).toHaveAttribute('aria-selected', 'true')
})

// The diff's sign is the content, so it needs a word - but not on the four
// hundred and eighty unchanged lines of a five hundred line diff.
test('a diff names its changed lines and says nothing about the rest', async () => {
  const h = await mountApp({
    runs: [RUN],
    routes: {
      '/api/runs/r-1/artifacts': () =>
        new Response(
          JSON.stringify([{ path: 'a.go', old: 'one\ntwo\n', new: 'one\nTWO\n' }]),
          { status: 200 },
        ),
    },
  })
  act(() => navigate({ screen: 'run', id: 'r-1' }))
  await waitFor(() => expect(h.runSocket()).toBeTruthy())

  const user = userEvent.setup()
  await user.click(await screen.findByRole('radio', { name: 'Changes' }))
  await screen.findByText('a.go')

  const body = screen.getByText('a.go').closest('div')?.parentElement
  expect(body?.textContent).toContain('removed')
  expect(body?.textContent).toContain('added')
  // "context" appears nowhere: it is the word the reader would wade through.
  expect(body?.textContent).not.toContain('context')
})

// The status bar has been promising `/ filter` since the shell was built. A
// project can declare a thousand models; a table that long is searched, not
// read.
test('the models table filters, and slash focuses the filter', async () => {
  const user = userEvent.setup()
  await mountApp({ models: MODELS })
  go('models')

  const box = await screen.findByRole('textbox', { name: 'Filter models' })
  expect(screen.getByText('GPT-5')).toBeInTheDocument()

  await user.type(box, 'opus')
  expect(screen.getByText('Claude Opus 5')).toBeInTheDocument()
  expect(screen.queryByText('GPT-5')).not.toBeInTheDocument()

  await user.clear(box)
  await user.type(box, 'zzz')
  expect(await screen.findByText(/No model matches/)).toBeInTheDocument()
})

test('slash from the page focuses the filter, and from a field types a slash', async () => {
  const user = userEvent.setup()
  await mountApp({ models: MODELS })
  go('models')
  const box = await screen.findByRole('textbox', { name: 'Filter models' })

  await user.click(document.body)
  await user.keyboard('/')
  expect(box).toHaveFocus()
  expect(box).toHaveValue('')

  await user.keyboard('a/b')
  expect(box).toHaveValue('a/b')
})

// The run screen has the stream as a prop and was reading the record instead.
// The record's booleans are whatever was true when the daemon last announced
// one; on the screen whose whole subject is watching a run, that is the wrong
// source.
test('the run screen reads its status from the stream it is drawing', async () => {
  const h = await mountApp({ runs: [RUN] })
  act(() => navigate({ screen: 'run', id: 'r-1' }))
  await waitFor(() => expect(h.runSocket()).toBeTruthy())
  act(() => h.runSocket()?.open())

  h.emit({ seq: 1, time: '2026-09-08T12:00:01Z', kind: EventKind.TurnStart })
  await waitFor(() => expect(screen.getByRole('main')).toHaveTextContent('Running'))

  h.emit({
    seq: 2,
    time: '2026-09-08T12:00:02Z',
    kind: EventKind.ApprovalRequest,
    id: 'a-1',
    approval: { kind: 'tool', tool: 'bash', options: [] },
  })
  await waitFor(() => expect(screen.getByRole('main')).toHaveTextContent('Needs attention'))
})

// A double click is two conversations against a daemon that holds thirty-two,
// and the second is one nobody asked for.
test('a second click while the first is still opening does nothing', async () => {
  const user = userEvent.setup()
  let answer: (r: Response) => void = () => undefined
  const h = await mountApp({
    runs: [],
    routes: {
      'POST /api/runs': () =>
        new Promise<Response>((ok) => {
          answer = ok
        }),
    },
  })

  const button = await screen.findByRole('button', { name: 'New session' })
  await user.click(button)
  // The request is held open, which is the window a double click lands in. The
  // button says so, and a click on it does nothing.
  await waitFor(() => expect(screen.getByRole('button', { name: 'Starting…' })).toBeDisabled())
  await user.click(screen.getByRole('button', { name: 'Starting…' }))

  act(() => answer(new Response(JSON.stringify(RUN), { status: 201 })))
  await waitFor(() =>
    expect(h.sent.filter((r) => r.method === 'POST' && r.path === '/api/runs')).toHaveLength(1),
  )
})
