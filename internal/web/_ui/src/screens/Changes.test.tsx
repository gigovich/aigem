import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'
import { navigate } from '@/lib/route'
import type { Artifact } from '@/lib/wire'
import { mountApp, RUN } from '@/test/harness'

afterEach(() => {
  vi.unstubAllGlobals()
})

const artifacts = (files: Artifact[]) => ({
  '/api/runs/r-1/artifacts': () => new Response(JSON.stringify(files), { status: 200 }),
})

const SECOND = { ...RUN, id: 'r-2', title: 'Another conversation' }

async function openChanges(
  routes: Record<string, () => Response | Promise<Response>>,
  run = RUN,
) {
  const user = userEvent.setup()
  const h = await mountApp({ runs: [run, SECOND], routes })
  act(() => navigate({ screen: 'run', id: run.id }))
  await screen.findByRole('radio', { name: 'Changes' })
  await user.click(screen.getByRole('radio', { name: 'Changes' }))
  return { user, h }
}

test('draws a diff of what the run changed', async () => {
  await openChanges(artifacts([{ path: 'retry.go', old: 'one\ntwo\n', new: 'one\nTWO\n' }]))

  expect(await screen.findByText('retry.go')).toBeInTheDocument()
  const main = screen.getByRole('main')
  expect(main).toHaveTextContent('two')
  expect(main).toHaveTextContent('TWO')
  expect(main).toHaveTextContent('+1')
  expect(main).toHaveTextContent('−1')
})

// The daemon leaves the content out past its budget and reports the sizes. A
// page that drew that as an empty diff would be saying nothing changed about a
// file the daemon said had.
test('says so when the daemon would not send both sides', async () => {
  await openChanges(
    artifacts([{ path: 'huge.json', truncated: true, oldBytes: 4096, newBytes: 8192 }]),
  )

  expect(await screen.findByText(/too large for the daemon to send/)).toBeInTheDocument()
  expect(screen.getByRole('main')).toHaveTextContent('4.0 kB to 8.0 kB')
})

// A closed run's artifacts were written beside its journal on every save, so
// the daemon still answers them - and the page must still read them.
test('a closed run still shows what it changed', async () => {
  await openChanges(
    artifacts([{ path: '/w/a.go', created: false, old: 'x\n', new: 'y\n', oldBytes: 2, newBytes: 2 }]),
    { ...RUN, status: 'closed', live: false },
  )
  expect(await screen.findByText('/w/a.go')).toBeInTheDocument()
})

test('a run that changed nothing says that', async () => {
  await openChanges(artifacts([]))
  expect(await screen.findByText('This run has not changed any files.')).toBeInTheDocument()
})

test('shows the daemon sentence when the changes cannot be read', async () => {
  await openChanges({
    '/api/runs/r-1/artifacts': () => new Response('this run has no live session', { status: 409 }),
  })
  expect(await screen.findByText('this run has no live session')).toBeInTheDocument()
})

// A file whose only change is its line endings is a change the daemon reported;
// drawing it as a diff with nothing in it would be the page denying it.
test('names a change that has no visible lines', async () => {
  await openChanges(artifacts([{ path: 'crlf.txt', old: 'a\r\nb\r\n', new: 'a\nb\n' }]))
  expect(await screen.findByText(/line endings or the final newline/)).toBeInTheDocument()
})

// One run's diffs must not paint under another's header - not even for the
// frame before the next answer arrives, which is why the second run's request
// is held open here.
test('drops what it read the moment the run changes', async () => {
  await openChanges({
    '/api/runs/r-1/artifacts': () =>
      new Response(JSON.stringify([{ path: 'first.go', old: 'a\n', new: 'b\n' }]), { status: 200 }),
    '/api/runs/r-2/artifacts': () => new Promise<Response>(() => undefined),
  })
  await screen.findByText('first.go')

  act(() => navigate({ screen: 'run', id: 'r-2' }))
  expect(screen.queryByText('first.go')).not.toBeInTheDocument()
})

// An answer that arrives after the person has moved on belongs to a run they
// are no longer looking at.
test('an answer that arrives late does not land on the run that replaced it', async () => {
  let answerFirst: (r: Response) => void = () => undefined
  await openChanges({
    '/api/runs/r-1/artifacts': () =>
      new Promise<Response>((ok) => {
        answerFirst = ok
      }),
    '/api/runs/r-2/artifacts': () => new Response(JSON.stringify([]), { status: 200 }),
  })

  act(() => navigate({ screen: 'run', id: 'r-2' }))
  await screen.findByText('This run has not changed any files.')

  act(() => answerFirst(new Response('the first run is gone', { status: 409 })))
  await waitFor(() =>
    expect(screen.queryByText('the first run is gone')).not.toBeInTheDocument(),
  )
  expect(screen.getByText('This run has not changed any files.')).toBeInTheDocument()
})

// A path with a bidi override in it displays as one name and is another, on the
// screen where a person reads back what the agent wrote.
test('a file name that lies about itself is shown as what it is', async () => {
  await openChanges(
    artifacts([{ path: 'notes/report‮hs.txt', old: 'a\n', new: 'b\n' }]),
  )
  const main = await screen.findByRole('main')
  expect(main.textContent).toContain('\\u202e')
  expect(main.textContent).not.toContain('‮')
})

test('a diff too long to compute reports its size instead', async () => {
  const long = `${'x\n'.repeat(3000)}`
  await openChanges(artifacts([{ path: 'generated.txt', old: long, new: 'y\n' }]))
  expect(await screen.findByText(/too long to diff in a browser/)).toBeInTheDocument()
})

test('the changed files are a list the keyboard can reach', async () => {
  await openChanges(artifacts([{ path: 'a.go', old: 'x\n', new: 'y\n' }]))
  const main = await screen.findByRole('main')
  // The switch between the two views is a radiogroup, which the arrows drive.
  expect(within(main).getByRole('radiogroup', { name: 'What to show' })).toBeInTheDocument()
})

// A lone carriage return is a character inside a line - a progress bar's
// output - and the browser must not turn it into a second row.
test('a carriage return inside a line is drawn as a symbol, on one row', async () => {
  await openChanges(
    artifacts([{ path: '/w/p.log', old: '', new: 'step 1\rstep 2\n', oldBytes: 0, newBytes: 14 }]),
  )
  const line = await screen.findByText(/step 1/)
  expect(line.textContent).toBe('step 1␍step 2')
})
