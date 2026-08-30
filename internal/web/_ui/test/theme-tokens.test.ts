import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from 'vitest'

// Read from disk rather than through an import: the Tailwind plugin intercepts
// `.css?raw` and hands back an empty string, so a raw import here would assert
// nothing at all.
const theme = (name: string) => readFileSync(join(import.meta.dirname, '../src/theme', name), 'utf8')
const indexCss = readFileSync(join(import.meta.dirname, '../src/index.css'), 'utf8')

const declared = (css: string): Set<string> =>
  new Set<string>(css.match(/--[a-z0-9-]+(?=\s*:)/g) ?? [])

// index.css says "add a role here only once it is defined in every palette
// file". Nothing else enforces that: a role missing from one palette compiles
// fine and renders as an unset var(), which reads as an invisible element
// rather than an error - and only in the theme nobody happened to look at.
test('every role the Tailwind bridge exposes is defined in both palettes', () => {
  const bridge = indexCss.slice(indexCss.indexOf('@theme inline'), indexCss.indexOf('@layer base'))
  const used = new Set<string>(
    bridge.match(/var\((--[a-z0-9-]+)\)/g)?.map((m: string) => m.slice(4, -1)) ?? [],
  )
  expect(used.size).toBeGreaterThan(10)

  const mocha = declared(theme('mocha.css'))
  const latte = declared(theme('latte.css'))
  const rest = new Set([...declared(theme('tokens.css')), ...declared(theme('fonts.css'))])

  for (const role of used) {
    const defined = (mocha.has(role) && latte.has(role)) || rest.has(role)
    expect(defined, `${role} is not defined in both palettes`).toBe(true)
  }
})

// A palette that defines a colour the other does not is the same defect seen
// from the other side: the theme toggle then changes what an element means.
test('the two palettes declare the same set of roles', () => {
  expect([...declared(theme('latte.css'))].sort()).toEqual([...declared(theme('mocha.css'))].sort())
})
