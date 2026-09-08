/** Formatting shared by every screen, so a number reads the same everywhere. */

const GROUPED = new Intl.NumberFormat('en-US')

/** `144,208` - grouped, because the design's context label is read at a glance. */
export function count(n: number): string {
  return GROUPED.format(n)
}

/** `144,208 / 200,000 tokens`, the context label from the design canvas. */
export function tokens(used: number, total: number): string {
  return `${count(used)} / ${count(total)} tokens`
}

/** `200K` - the context column on the models screen. */
export function compact(n: number): string {
  if (n >= 1_000_000) return `${Math.round(n / 100_000) / 10}M`
  if (n >= 1_000) return `${Math.round(n / 1_000)}K`
  return String(n)
}

/*
 * The two clock formatters are built once and reused.
 *
 * `toLocaleTimeString(locale, options)` constructs an ICU formatter per call -
 * measured at about a millisecond each, thirty-five times the cost of the same
 * call without an options object, which V8 does cache. These run once per row
 * of a timeline that can be thousands of rows long, so building them here is
 * the difference between a stream that scrolls and one that blocks.
 */
const HMS = new Intl.DateTimeFormat('en-GB', {
  hour12: false,
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
})
const HM = new Intl.DateTimeFormat('en-GB', { hour12: false, hour: '2-digit', minute: '2-digit' })

/** `14:32:01` - the timestamp column of an event stream. */
export function clock(at: string | number | Date): string {
  const d = new Date(at)
  if (Number.isNaN(d.getTime())) return ''
  return HMS.format(d)
}

/** `14:32` - the activity feed, which is a minute-resolution record. */
export function hhmm(at: string | number | Date): string {
  const d = new Date(at)
  if (Number.isNaN(d.getTime())) return ''
  return HM.format(d)
}

/** `now`, `12m`, `2h`, `3d` - the "updated" column, as the design writes it. */
export function ago(at: string | number | Date, now: number = Date.now()): string {
  const t = new Date(at).getTime()
  if (Number.isNaN(t)) return ''
  const secs = Math.max(0, Math.round((now - t) / 1000))
  if (secs < 45) return 'now'
  const mins = Math.round(secs / 60)
  if (mins < 60) return `${mins}m`
  const hours = Math.round(mins / 60)
  if (hours < 24) return `${hours}h`
  return `${Math.round(hours / 24)}d`
}

/** `02:14 elapsed` - a run header's clock. */
export function elapsed(from: string | number | Date, now: number = Date.now()): string {
  const t = new Date(from).getTime()
  if (Number.isNaN(t)) return ''
  const secs = Math.max(0, Math.round((now - t) / 1000))
  const h = Math.floor(secs / 3600)
  const m = Math.floor((secs % 3600) / 60)
  const s = secs % 60
  const pad = (n: number) => String(n).padStart(2, '0')
  return h > 0 ? `${pad(h)}:${pad(m)}:${pad(s)}` : `${pad(m)}:${pad(s)}`
}

/** `1.4 kB` - a blob's size, next to the head of a trimmed tool result. */
export function bytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} kB`
  return `${(n / (1024 * 1024)).toFixed(1)} MB`
}

/** The percentage a progress bar draws, clamped so a bad number cannot escape. */
export function percent(used: number, total: number): number {
  if (!(total > 0)) return 0
  return Math.max(0, Math.min(100, Math.round((used / total) * 100)))
}

/**
 * The version, short enough for the header's logo column.
 *
 * `aigem version` reports "v0.4.0-64-g01d54ef (01d54ef, 2026-09-08T09:04:38Z)":
 * the build's identity followed by where it came from. Only the first part fits
 * beside the product name, and the header carries the whole string as its title
 * for anyone who needs the commit.
 */
export function shortVersion(version: string): string {
  const [first] = version.split(' ')
  return first ?? version
}
