import { act, cleanup, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'
import { EventKind } from '@/lib/wire'
import type { RunEvent } from '@/lib/wire'
import { mountApp, RUN } from '@/test/harness'

afterEach(() => {
  vi.unstubAllGlobals()
})

const APPROVAL: RunEvent = {
  seq: 2,
  time: '2026-09-08T12:00:02Z',
  kind: EventKind.ApprovalRequest,
  id: 'a-1',
  approval: {
    kind: 'tool',
    tool: 'run_command',
    args: { command: 'go test ./...' },
    options: [
      { value: 'once', label: 'Once' },
      { value: 'always', label: 'Always' },
      { value: 'deny', label: 'Forbid' },
    ],
  },
}

async function attached() {
  const h = await mountApp({ runs: [RUN] })
  await waitFor(() => expect(h.runSocket()).toBeTruthy())
  act(() => h.runSocket()?.open())
  return h
}

test('sends a typed message up the socket on the modifier and Enter', async () => {
  const user = userEvent.setup()
  const h = await attached()

  const box = screen.getByRole('textbox', { name: 'Message' })
  await user.type(box, 'rotate the keys')
  // Enter alone is a newline: this is a composer, not a chat line.
  await user.keyboard('{Enter}')
  expect(h.runSocket()?.sent).toHaveLength(0)

  await user.keyboard('{Control>}{Enter}{/Control}')
  await waitFor(() => expect(h.runSocket()?.sent).toHaveLength(1))
  const sent = JSON.parse(h.runSocket()?.sent[0] ?? '{}') as { op: string; text: string }
  expect(sent.op).toBe('submit')
  expect(sent.text).toContain('rotate the keys')
})

// A leading slash is a command inside the conversation, not a message that
// happens to start with a slash.
test('sends a slash command as a command', async () => {
  const user = userEvent.setup()
  const h = await attached()

  await user.type(screen.getByRole('textbox', { name: 'Message' }), '/compact now')
  await user.keyboard('{Control>}{Enter}{/Control}')

  await waitFor(() => expect(h.runSocket()?.sent).toHaveLength(1))
  expect(JSON.parse(h.runSocket()?.sent[0] ?? '{}')).toEqual({
    op: 'command',
    name: 'compact',
    args: 'now',
  })
})

// The buttons are the request's own options, never a fixed pair: "Always" on a
// write outside the working directory is a promise the sandbox refuses to keep,
// so the daemon does not offer it there.
test('an approval offers exactly the options the request carries', async () => {
  const user = userEvent.setup()
  const h = await attached()
  h.emit(APPROVAL)

  const card = await screen.findByRole('group', { name: 'Approval required' })
  expect(within(card).getByRole('button', { name: 'Forbid' })).toBeInTheDocument()
  expect(within(card).getByRole('button', { name: 'Always' })).toBeInTheDocument()
  expect(within(card).getByRole('button', { name: 'Approve & run' })).toBeInTheDocument()

  await user.click(within(card).getByRole('button', { name: 'Approve & run' }))
  await waitFor(() => expect(h.runSocket()?.sent).toHaveLength(1))
  expect(JSON.parse(h.runSocket()?.sent[0] ?? '{}')).toMatchObject({
    op: 'resolve',
    id: 'a-1',
    decision: 'once',
  })
})

// The button that refuses has to refuse. This is the security boundary of the
// product: a Forbid that sends "once" grants what the person just denied, and
// nothing about the screen would look wrong.
test('each approval button sends the decision it is labelled with', async () => {
  for (const [label, decision] of [
    ['Forbid', 'deny'],
    ['Always', 'always'],
    ['Approve & run', 'once'],
  ] as const) {
    const user = userEvent.setup()
    const h = await attached()
    h.emit(APPROVAL)
    const card = await screen.findByRole('group', { name: 'Approval required' })

    await user.click(within(card).getByRole('button', { name: label }))
    await waitFor(() => expect(h.runSocket()?.sent).toHaveLength(1))
    expect(JSON.parse(h.runSocket()?.sent[0] ?? '{}'), label).toMatchObject({
      op: 'resolve',
      id: 'a-1',
      decision,
    })
    cleanup()
    vi.unstubAllGlobals()
  }
})

// A decision written into a socket that is not open goes nowhere, and a live
// button says it went somewhere.
test('the approval buttons are dead while the socket is', async () => {
  const h = await mountApp({ runs: [RUN] })
  await waitFor(() => expect(h.runSocket()).toBeTruthy())
  act(() => h.runSocket()?.open())
  h.emit(APPROVAL)
  await screen.findByRole('group', { name: 'Approval required' })

  act(() => h.runSocket()?.drop())
  await waitFor(() => {
    const card = screen.getByRole('group', { name: 'Approval required' })
    expect(within(card).getByRole('button', { name: 'Approve & run' })).toBeDisabled()
  })
  const card = screen.getByRole('group', { name: 'Approval required' })
  expect(within(card).getByRole('button', { name: 'Forbid' })).toBeDisabled()
  expect(within(card).getByRole('button', { name: 'Always' })).toBeDisabled()
})

// A long command with one hostile line at the end is the shape an attacker
// wants approved, and a box showing nine of its sixty lines is the mechanism.
test('shows the whole command being approved, and says how long it is', async () => {
  const h = await attached()
  const command = Array.from({ length: 40 }, (_, i) => `line ${i}`).join('\n')
  h.emit({
    ...APPROVAL,
    approval: { ...APPROVAL.approval!, args: { command } },
  })

  const card = await screen.findByRole('group', { name: 'Approval required' })
  expect(card).toHaveTextContent('line 39')
  expect(card).toHaveTextContent(/lines — read to the end/)
})

// A path carrying a bidi override displays as one name and is another, and this
// is the screen where that decides whether a script runs.
test('a path that lies about its name is shown as what it is', async () => {
  const h = await attached()
  h.emit({
    ...APPROVAL,
    approval: {
      kind: 'path',
      tool: 'write_file',
      path: '/home/dev/report\u202Ehs.txt',
      write: true,
      options: [
        { value: 'once', label: 'Once' },
        { value: 'deny', label: 'Deny' },
      ],
    },
  })

  const card = await screen.findByRole('group', { name: 'Approval required' })
  expect(card).toHaveTextContent('\\u202e')
  expect(card.textContent).not.toContain('\u202E')
})

test('a path approval shows only what the daemon offered for it', async () => {
  const h = await attached()
  h.emit({
    ...APPROVAL,
    approval: {
      kind: 'path',
      tool: 'write_file',
      path: '/etc/hosts',
      write: true,
      options: [
        { value: 'once', label: 'Once' },
        { value: 'deny', label: 'Deny' },
      ],
    },
  })

  const card = await screen.findByRole('group', { name: 'Approval required' })
  expect(within(card).getByText('/etc/hosts')).toBeInTheDocument()
  expect(within(card).queryByRole('button', { name: /Always/ })).not.toBeInTheDocument()
})

// The answer comes back as an event, and the card goes because the daemon said
// it was decided - not because this tab clicked something.
test('the approval clears when the daemon says it was resolved', async () => {
  const h = await attached()
  h.emit(APPROVAL)
  await screen.findByRole('group', { name: 'Approval required' })

  h.emit({
    seq: 3,
    time: '2026-09-08T12:00:03Z',
    kind: EventKind.ApprovalResolved,
    id: 'a-1',
    decision: 'once',
    by: 'the terminal',
  })

  await waitFor(() =>
    expect(screen.queryByRole('group', { name: 'Approval required' })).not.toBeInTheDocument(),
  )
})

test('step mode is a toggle that goes up the socket', async () => {
  const user = userEvent.setup()
  const h = await attached()

  const button = screen.getByRole('button', { name: 'Step mode' })
  expect(button).toHaveAttribute('aria-pressed', 'false')
  await user.click(button)

  await waitFor(() => expect(h.runSocket()?.sent).toHaveLength(1))
  expect(JSON.parse(h.runSocket()?.sent[0] ?? '{}')).toEqual({ op: 'step_mode', on: true })
})

test('interrupt appears only while a turn is running', async () => {
  const user = userEvent.setup()
  const h = await attached()
  expect(screen.queryByRole('button', { name: 'Interrupt' })).not.toBeInTheDocument()

  h.emit({ seq: 2, time: '2026-09-08T12:00:02Z', kind: EventKind.TurnStart })
  await user.click(await screen.findByRole('button', { name: 'Interrupt' }))
  await waitFor(() => expect(h.runSocket()?.sent).toHaveLength(1))
  expect(JSON.parse(h.runSocket()?.sent[0] ?? '{}')).toEqual({ op: 'interrupt' })

  h.emit({ seq: 3, time: '2026-09-08T12:00:03Z', kind: EventKind.TurnEnd })
  await waitFor(() =>
    expect(screen.queryByRole('button', { name: 'Interrupt' })).not.toBeInTheDocument(),
  )
})

// Closing a session ends it for every client of that run, and it cannot be
// continued afterwards. It gets a question.
test('closing a session asks first, and only then deletes', async () => {
  const user = userEvent.setup()
  const h = await mountApp({
    runs: [RUN],
    routes: { 'DELETE /api/runs/r-1': () => new Response(null, { status: 204 }) },
  })

  await user.click(await screen.findByRole('button', { name: /Close Rotate the signing keys/ }))
  expect(h.sent.filter((r) => r.method === 'DELETE')).toHaveLength(0)

  const dialog = await screen.findByRole('dialog', { name: 'Close this session?' })
  await user.click(within(dialog).getByRole('button', { name: 'Close session' }))
  await waitFor(() => {
    expect(h.sent.filter((r) => r.method === 'DELETE' && r.path === '/api/runs/r-1')).toHaveLength(1)
  })
})

test('starting a session posts a run and shows it', async () => {
  const user = userEvent.setup()
  const h = await mountApp({
    runs: [],
    routes: {
      'POST /api/runs': () => new Response(JSON.stringify({ ...RUN, id: 'r-9' }), { status: 201 }),
    },
  })

  await user.click(await screen.findByRole('button', { name: 'New session' }))
  await waitFor(() => {
    expect(h.sent.filter((r) => r.method === 'POST' && r.path === '/api/runs')).toHaveLength(1)
  })
  expect(window.location.pathname).toBe('/chat')
})

// The daemon's refusals are written for a person and must reach them as text.
test('shows the daemon\'s sentence when a session cannot be opened', async () => {
  const user = userEvent.setup()
  await mountApp({
    runs: [],
    routes: {
      'POST /api/runs': () =>
        new Response('32 conversations are already open; close one first', { status: 503 }),
    },
  })

  await user.click(await screen.findByRole('button', { name: 'New session' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('32 conversations are already open')
})

// A tool result over the daemon's threshold reaches the timeline as its head,
// with the whole of it kept beside the journal. Without the offer to fetch it,
// `bytes` and `blob` are two fields nobody can act on.
test('offers the whole of a tool result the timeline trimmed', async () => {
  const user = userEvent.setup()
  const h = await mountApp({
    runs: [RUN],
    routes: {
      '/api/runs/r-1/blobs/3': () =>
        new Response('the whole of the output', {
          status: 200,
          headers: { 'Content-Type': 'text/plain' },
        }),
    },
  })
  await waitFor(() => expect(h.runSocket()).toBeTruthy())
  act(() => h.runSocket()?.open())
  h.emit({
    seq: 3,
    time: '2026-09-08T12:00:03Z',
    kind: EventKind.ToolEnd,
    name: 'run_command',
    bytes: 40960,
    blob: true,
  })

  await user.click(await screen.findByRole('button', { name: 'show all' }))
  const dialog = await screen.findByRole('dialog', { name: 'Tool output' })
  await waitFor(() => expect(dialog).toHaveTextContent('the whole of the output'))
})

// A trimmed result the daemon could not keep says so by not promising one. A
// page that offered the fetch anyway would be offering a 404.
test('offers nothing when the daemon kept no body', async () => {
  const h = await mountApp({ runs: [RUN] })
  await waitFor(() => expect(h.runSocket()).toBeTruthy())
  act(() => h.runSocket()?.open())
  h.emit({
    seq: 3,
    time: '2026-09-08T12:00:03Z',
    kind: EventKind.ToolEnd,
    name: 'run_command',
    bytes: 40960,
  })

  await screen.findByRole('log')
  expect(screen.queryByRole('button', { name: 'show all' })).not.toBeInTheDocument()
})
