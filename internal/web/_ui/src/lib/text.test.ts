import { expect, test } from 'vitest'
import { readable, safeHref } from './text'

// A path carrying a bidi override displays as one name and is another. React
// escapes markup, so this is not about injection - it is about the one screen
// where a person makes a security decision on a string a model supplied.
test('makes a bidi override visible instead of letting it rename a path', () => {
  const disguised = '/home/dev/notes/report‮hs.txt'
  const shown = readable(disguised)
  expect(shown).not.toContain('‮')
  expect(shown).toContain('\\u202e')
  // The rest of the path is untouched: a silently shortened one is its own lie.
  expect(shown).toContain('/home/dev/notes/report')
})

test('leaves ordinary text alone, including non-latin scripts', () => {
  for (const text of ['go test ./...', 'путь/к/файлу', '日本語のパス', '']) {
    expect(readable(text)).toBe(text)
  }
})

test('covers the invisible characters that matter, not just one', () => {
  for (const ch of ['​', '‎', '‪', '⁦', '﻿']) {
    expect(readable(`a${ch}b`)).not.toContain(ch)
  }
})

test('follows only what this page is willing to navigate to', () => {
  expect(safeHref('https://example.test/x')).toBe('https://example.test/x')
  expect(safeHref('http://example.test')).toBe('http://example.test')
  expect(safeHref('mailto:a@b.test')).toBe('mailto:a@b.test')
  expect(safeHref('/models')).toBe('/models')
  expect(safeHref('#section')).toBe('#section')
})

// The claim "a relative link is same-origin by construction" is false for an
// authority-relative URL: it begins with a slash and goes wherever it names.
// A backslash is a slash to the URL parser, so it is the same trick spelled
// differently.
test('refuses an authority-relative URL however it is spelled', () => {
  expect(safeHref('//evil.example/steal')).toBeNull()
  expect(safeHref('/\\evil.example/steal')).toBeNull()
  expect(safeHref('\\\\evil.example/steal')).toBeNull()
  expect(safeHref('  //evil.example')).toBeNull()
})

test('refuses a scheme that executes', () => {
  for (const url of [
    'javascript:alert(1)',
    'JavaScript:alert(1)',
    '  javascript:alert(1)',
    'data:text/html,<script>',
    'vbscript:msgbox',
  ]) {
    expect(safeHref(url), url).toBeNull()
  }
})

// The URL parser removes tab, LF and CR from anywhere in a URL before it parses
// it, so the two slashes of an authority only have to be adjacent afterwards.
// Trimming the ends does not reach an interior one.
test('an authority-relative URL cannot hide behind a control character', () => {
  for (const url of [
    '/\t/evil.example/steal',
    '/\n/evil.example/steal',
    '/\r/evil.example/steal',
    '/\t\\evil.example/steal',
    '/\r\n/evil.example/steal',
  ]) {
    expect(safeHref(url), JSON.stringify(url)).toBeNull()
  }
})

// What comes back is the form that was checked, not the form that was given.
test('returns the URL it actually inspected', () => {
  expect(safeHref('https://exam\tple.test/x')).toBe('https://example.test/x')
})

// U+061C is a bidi formatting control in exactly the class this covers - it
// behaves as U+200F, which was covered - so leaving it out made the claim
// incomplete rather than merely narrow.
test('covers the bidi controls, including the Arabic letter mark', () => {
  for (const ch of ['؜', '‏', '‮', '⁦', '­', '⁠']) {
    expect(readable(`a${ch}b`), JSON.stringify(ch)).not.toContain(ch)
  }
  // The tag block, which renders as nothing at all.
  expect(readable('a\u{e0041}b')).not.toContain('\u{e0041}')
})
