/**
 * Making a daemon string safe to *read*.
 *
 * React escapes markup, so nothing here is about injection. It is about the
 * strings a person makes a decision on - the tool an approval is asking about,
 * the path it wants to write, the command it wants to run - all of which come
 * from a model that read files an attacker may have written.
 */

/**
 * Bidirectional and other invisible formatting controls, made visible.
 *
 * A path with U+202E in it displays as `report.sh.txt` and is `report.txt.sh`. That
 * is a rename of the thing the person is approving, done by the string itself,
 * and it is the one class of "escaped correctly and still lies" that matters on
 * an approval dialog. The replacement keeps the character's identity visible
 * rather than dropping it, because a silently shortened path is its own lie.
 */
// Split in two because the variation selectors combine with what precedes them,
// which a single class would be flagged for: they are matched on their own.
const INVISIBLE =
  /[\u00ad\u061c\u200b-\u200f\u202a-\u202e\u2060-\u206f\ufeff]|[\u{e0000}-\u{e007f}]/gu
const VARIATION = /[\ufe00-\ufe0f]/g

const escape = (ch: string) => `\\u${ch.codePointAt(0)?.toString(16).padStart(4, '0')}`

export function readable(text: string): string {
  return text.replace(INVISIBLE, escape).replace(VARIATION, escape)
}

/**
 * A URL this page is willing to follow: http, https, mailto, or a link back
 * into the application.
 *
 * `//host/path` begins with a slash and is neither relative nor same-origin -
 * it borrows the page's scheme and goes wherever it names - and a backslash is
 * a slash to the URL parser, so `/\host` is the same trick spelled differently.
 */
const SAFE_SCHEME = /^(https?:|mailto:)/i

export function safeHref(url: string): string | null {
  // Tab, LF and CR are removed from anywhere in a URL by the parser before it
  // parses, so the two slashes of an authority only have to be adjacent *after*
  // that strip: `/<tab>/evil.example` is `//evil.example`. Trimming the ends
  // does not reach them. The stripped form is what is returned, so what this
  // function checked is what the browser is given.
  const trimmed = url.replace(/[\t\n\r]/g, '').trim()
  if (/^[/\\]{2}/.test(trimmed)) return null
  if (trimmed.startsWith('/') || trimmed.startsWith('#')) return trimmed
  return SAFE_SCHEME.test(trimmed) ? trimmed : null
}
