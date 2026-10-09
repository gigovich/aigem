import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from 'vitest'

/**
 * The palettes and metrics, transcribed from the design canvas.
 *
 * The canvas is the single source of truth for how phase one looks, and it is
 * not in this repository - it is a Claude Design artboard. So the numbers are
 * written down here once, character for character, and the CSS is checked
 * against them. "Approximately the same colour" is exactly the failure this
 * exists to catch: a border two shades off is invisible in review and wrong on
 * every screen at once.
 *
 * Canvas: project `Aigem product questions`, file `Aigem.dc.html`,
 * `:root` and `:root[data-theme="latte"]`.
 */

const MOCHA = {
  '--bg': '#11111b',
  '--shell': '#181825',
  '--surface': '#1e1e2e',
  '--s0': '#313244',
  '--s1': '#45475a',
  '--s2': '#585b70',
  '--fg': '#cdd6f4',
  '--fg-muted': '#a6adc8',
  '--fg-subtle': '#7f849c',
  '--border': '#2a2b3c',
  '--border-strong': '#45475a',
  '--primary': '#89b4fa',
  '--info': '#74c7ec',
  '--success': '#a6e3a1',
  '--warning': '#f9e2af',
  '--attention': '#fab387',
  '--danger': '#f38ba8',
  '--agent': '#cba6f7',
  '--running': '#94e2d5',
  '--added': '#a6e3a1',
  '--modified': '#f9e2af',
  '--deleted': '#f38ba8',
}

const LATTE = {
  '--bg': '#dce0e8',
  '--shell': '#e6e9ef',
  '--surface': '#eff1f5',
  '--s0': '#ccd0da',
  '--s1': '#bcc0cc',
  '--s2': '#acb0be',
  '--fg': '#4c4f69',
  '--fg-muted': '#6c6f85',
  '--fg-subtle': '#8c8fa1',
  '--border': '#ccd0da',
  '--border-strong': '#acb0be',
  '--primary': '#1e66f5',
  '--info': '#209fb5',
  '--success': '#40a02b',
  '--warning': '#df8e1d',
  '--attention': '#fe640b',
  '--danger': '#d20f39',
  '--agent': '#8839ef',
  '--running': '#179299',
  '--added': '#40a02b',
  '--modified': '#df8e1d',
  '--deleted': '#d20f39',
}

const METRICS = {
  '--row-h': '1.875rem',
  '--gut': '0.75rem',
  '--fs': '0.875rem',
  '--fs-meta': '0.75rem',
  // The sidebar and the inspector at the ordinary width.
  '--rail': '13rem',
  '--panel': '19rem',
}
const NARROW = { '--rail': '10.5rem', '--panel': '15.75rem' }

const css = (name: string) => readFileSync(join(import.meta.dirname, '../src/theme', name), 'utf8')

/** The declarations inside one selector's block, as a name/value map. */
function block(source: string, selector: string): Record<string, string> {
  const start = source.indexOf(selector + ' {')
  expect(start, `${selector} is not declared`).toBeGreaterThanOrEqual(0)
  const open = source.indexOf('{', start)
  const end = source.indexOf('\n}', open)
  expect(end, `${selector} is not closed`).toBeGreaterThan(open)
  const out: Record<string, string> = {}
  for (const line of source.slice(open + 1, end).split('\n')) {
    const m = /^\s*(--[a-z0-9-]+)\s*:\s*(.+?);\s*$/.exec(line)
    if (m?.[1] && m[2]) out[m[1]] = m[2]
  }
  return out
}

test('mocha is the canvas palette, value for value', () => {
  const declared = block(css('mocha.css'), ':root')
  for (const [role, value] of Object.entries(MOCHA)) {
    expect(declared[role], `${role} in mocha`).toBe(value)
  }
})

test('latte is the canvas palette, value for value', () => {
  const declared = block(css('latte.css'), ":root[data-theme='latte']")
  for (const [role, value] of Object.entries(LATTE)) {
    expect(declared[role], `${role} in latte`).toBe(value)
  }
})

test('the metrics are the canvas metrics', () => {
  const root = block(css('tokens.css'), ':root')
  for (const [name, value] of Object.entries(METRICS)) {
    expect(root[name], name).toBe(value)
  }
})

// The canvas's narrow layout: 13rem becomes 10.5rem and 19rem becomes 15.75rem, at
// 1120px. A width that drifts here breaks the single vertical rule the header
// and the sidebar draw between them.
test('the narrow layout is the canvas breakpoint and the canvas widths', () => {
  const source = css('tokens.css')
  expect(source).toContain('@media (max-width: 1119px)')
  const narrow = block(source.slice(source.indexOf('@media')), ':root')
  for (const [name, value] of Object.entries(NARROW)) {
    expect(narrow[name], `${name} at narrow`).toBe(value)
  }
})

// The two animations the canvas names, plus the sheet slide. A screen that
// referenced one this file does not declare would simply not animate, which is
// the kind of miss nothing else notices.
test('the canvas animations are all declared', () => {
  const source = css('tokens.css')
  for (const name of ['aigem-pulse', 'aigem-in', 'aigem-sheet']) {
    expect(source, `@keyframes ${name}`).toContain(`@keyframes ${name}`)
  }
})
