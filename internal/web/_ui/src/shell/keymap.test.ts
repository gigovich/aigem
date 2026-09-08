import { expect, test } from 'vitest'
import { handleKey, isTypingTarget } from './keymap'
import type { KeyActions, Layers } from './keymap'

const NOTHING_OPEN: Layers = {
  anyOpen: false,
  confirmOpen: false,
  modalOpen: false,
  paletteOpen: false,
}

function actions(): KeyActions & { calls: string[] } {
  const calls: string[] = []
  const note = (name: string) => () => calls.push(name)
  return {
    calls,
    closeLayers: note('close'),
    togglePalette: note('palette'),
    toggleQuick: note('quick'),
    confirm: note('confirm'),
    submitModal: note('submit'),
    paletteMove: (d) => calls.push(`move:${d}`),
    paletteRun: note('run'),
    focusFilter: note('filter'),
  }
}

const press = (key: string, over: Partial<KeyboardEvent> = {}) =>
  ({ key, metaKey: false, ctrlKey: false, shiftKey: false, target: document.body, ...over }) as
    unknown as KeyboardEvent

// This is the predicate the whole map turns on. Without it, "/" typed into the
// message box focuses the filter instead of typing a slash - which is the bug
// that makes an application feel hostile.
test('knows what the person is typing in', () => {
  const input = document.createElement('input')
  const textarea = document.createElement('textarea')
  const select = document.createElement('select')
  const div = document.createElement('div')
  const editable = document.createElement('div')
  editable.contentEditable = 'true'
  // jsdom does not derive isContentEditable from the attribute.
  Object.defineProperty(editable, 'isContentEditable', { value: true })

  expect(isTypingTarget(input)).toBe(true)
  expect(isTypingTarget(textarea)).toBe(true)
  expect(isTypingTarget(select)).toBe(true)
  expect(isTypingTarget(editable)).toBe(true)
  expect(isTypingTarget(div)).toBe(false)
  expect(isTypingTarget(null)).toBe(false)
})

test('opens the palette and the quick chat on either modifier', () => {
  for (const mod of ['metaKey', 'ctrlKey'] as const) {
    const a = actions()
    handleKey(press('k', { [mod]: true }), NOTHING_OPEN, a)
    handleKey(press('J', { [mod]: true }), NOTHING_OPEN, a)
    expect(a.calls).toEqual(['palette', 'quick'])
  }
})

// The shortcut has to work from inside the message box: it is how a person
// leaves it.
test('the palette shortcut works while typing', () => {
  const a = actions()
  const input = document.createElement('input')
  expect(handleKey(press('k', { metaKey: true, target: input }), NOTHING_OPEN, a)).toBe(true)
  expect(a.calls).toEqual(['palette'])
})

test('Escape closes whatever is open, and nothing when nothing is', () => {
  const open = actions()
  expect(handleKey(press('Escape'), { ...NOTHING_OPEN, anyOpen: true }, open)).toBe(true)
  expect(open.calls).toEqual(['close'])

  // Unhandled when there is no layer, so Escape still reaches the page - which
  // is what lets a screen bind it to clearing its own filter.
  const shut = actions()
  expect(handleKey(press('Escape'), NOTHING_OPEN, shut)).toBe(false)
  expect(shut.calls).toEqual([])
})

// A confirm is asking about something destructive. Enter answers it, and
// nothing else reaches the page behind it.
test('a confirm takes Enter and swallows the rest', () => {
  const layers = { ...NOTHING_OPEN, anyOpen: true, confirmOpen: true }
  const a = actions()
  expect(handleKey(press('Enter'), layers, a)).toBe(true)
  expect(a.calls).toEqual(['confirm'])

  const b = actions()
  expect(handleKey(press('/'), layers, b)).toBe(false)
  expect(b.calls).toEqual([])
})

// ...but not while the person is typing in it: a confirm with a field in it
// must not fire on the Enter that ends a line.
test('a confirm ignores Enter from inside a field', () => {
  const a = actions()
  const input = document.createElement('input')
  handleKey(press('Enter', { target: input }), { ...NOTHING_OPEN, anyOpen: true, confirmOpen: true }, a)
  expect(a.calls).toEqual([])
})

test('a modal submits on the modifier and Enter, never on Enter alone', () => {
  const layers = { ...NOTHING_OPEN, anyOpen: true, modalOpen: true }
  const a = actions()
  expect(handleKey(press('Enter', { metaKey: true }), layers, a)).toBe(true)
  expect(handleKey(press('Enter'), layers, a)).toBe(false)
  expect(a.calls).toEqual(['submit'])
})

test('the palette takes the arrows and Enter', () => {
  const layers = { ...NOTHING_OPEN, anyOpen: true, paletteOpen: true }
  const a = actions()
  handleKey(press('ArrowDown'), layers, a)
  handleKey(press('ArrowUp'), layers, a)
  handleKey(press('Enter'), layers, a)
  expect(a.calls).toEqual(['move:1', 'move:-1', 'run'])
})

test('/ focuses the filter, but not from inside a field', () => {
  const a = actions()
  expect(handleKey(press('/'), NOTHING_OPEN, a)).toBe(true)
  expect(a.calls).toEqual(['filter'])

  const b = actions()
  const textarea = document.createElement('textarea')
  expect(handleKey(press('/', { target: textarea }), NOTHING_OPEN, b)).toBe(false)
  expect(b.calls).toEqual([])
})

// The layer order is the point: with a confirm on top of the palette, Enter
// answers the confirm rather than running the highlighted command.
test('the topmost layer wins', () => {
  const a = actions()
  handleKey(press('Enter'), { anyOpen: true, confirmOpen: true, modalOpen: true, paletteOpen: true }, a)
  expect(a.calls).toEqual(['confirm'])
})

test('an unhandled key is left for the page', () => {
  const a = actions()
  expect(handleKey(press('a'), NOTHING_OPEN, a)).toBe(false)
  expect(a.calls).toEqual([])
})
