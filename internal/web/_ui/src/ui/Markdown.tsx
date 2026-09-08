import type { ReactNode } from 'react'

/**
 * Markdown, rendered into elements rather than into HTML.
 *
 * This page draws model output and the text of skills the agent may have been
 * pointed at by a page an attacker wrote. `dangerouslySetInnerHTML` with a
 * sanitiser in front of it is the usual answer and the wrong one here: the
 * sanitiser becomes the only thing standing between that text and the DOM, and
 * it is one dependency upgrade away from being the vulnerability.
 *
 * So there is no HTML path at all. The parser below produces React elements, a
 * tag this file does not know how to build cannot be built, and anything it
 * does not recognise stays text. That is a smaller subset of Markdown than a
 * library would give, and it is a property no library can offer.
 *
 * The one place a URL is honoured is a link, and only `http`, `https` and
 * `mailto` - which is what keeps `javascript:` and `data:` out.
 */

const SAFE_SCHEME = /^(https?:|mailto:)/i

function href(url: string): string | null {
  const trimmed = url.trim()
  // A relative link is same-origin and cannot carry a scheme, so it is safe by
  // construction; anything absolute has to name a scheme this page allows.
  if (trimmed.startsWith('/') || trimmed.startsWith('#')) return trimmed
  return SAFE_SCHEME.test(trimmed) ? trimmed : null
}

// Inline spans, in the order they are tried: code first, because its content is
// never re-parsed.
const INLINE = /(`[^`]+`)|(\[[^\]]+\]\([^)\s]+\))|(\*\*[^*]+\*\*)|(_[^_]+_)/

function inline(text: string, key: string): ReactNode[] {
  const out: ReactNode[] = []
  let rest = text
  let n = 0
  for (;;) {
    const m = INLINE.exec(rest)
    if (!m || m.index === undefined) break
    if (m.index > 0) out.push(rest.slice(0, m.index))
    const token = m[0]
    const id = `${key}-${n++}`
    if (token.startsWith('`')) {
      out.push(
        <code key={id} className="rounded-sm bg-s0 px-1 font-mono text-[0.92em]">
          {token.slice(1, -1)}
        </code>,
      )
    } else if (token.startsWith('[')) {
      const split = token.indexOf('](')
      const label = token.slice(1, split)
      const url = href(token.slice(split + 2, -1))
      out.push(
        url ? (
          <a key={id} href={url} rel="noreferrer noopener" target="_blank">
            {label}
          </a>
        ) : (
          // A scheme this page will not follow is shown as what it said, so a
          // reader can see the link was there and that it was not made live.
          <span key={id}>{token}</span>
        ),
      )
    } else if (token.startsWith('**')) {
      out.push(
        <strong key={id} className="font-semibold text-fg">
          {token.slice(2, -2)}
        </strong>,
      )
    } else {
      out.push(<em key={id}>{token.slice(1, -1)}</em>)
    }
    rest = rest.slice(m.index + token.length)
  }
  if (rest) out.push(rest)
  return out
}

type Block =
  | { kind: 'p' | 'h1' | 'h2' | 'h3'; text: string }
  | { kind: 'code'; text: string }
  | { kind: 'ul'; items: string[] }

/** What ends a paragraph: the start of any other block. */
const BLOCK_START = /^(#{1,3}\s|```|\s*[-*]\s)/

function parse(source: string): Block[] {
  const blocks: Block[] = []
  const lines = source.replace(/\r\n?/g, '\n').split('\n')
  let i = 0
  while (i < lines.length) {
    const line = lines[i] ?? ''
    if (line.startsWith('```')) {
      const body: string[] = []
      i++
      while (i < lines.length && !(lines[i] ?? '').startsWith('```')) {
        body.push(lines[i] ?? '')
        i++
      }
      // Past the closing fence, or past the end when the writer never wrote one.
      i++
      blocks.push({ kind: 'code', text: body.join('\n') })
      continue
    }
    const heading = /^(#{1,3})\s+(.*)$/.exec(line)
    if (heading?.[1] && heading[2] !== undefined) {
      const kind = (['h1', 'h2', 'h3'] as const)[heading[1].length - 1] ?? 'h3'
      blocks.push({ kind, text: heading[2] })
      i++
      continue
    }
    if (/^\s*[-*]\s+/.test(line)) {
      const items: string[] = []
      while (i < lines.length && /^\s*[-*]\s+/.test(lines[i] ?? '')) {
        items.push((lines[i] ?? '').replace(/^\s*[-*]\s+/, ''))
        i++
      }
      blocks.push({ kind: 'ul', items })
      continue
    }
    if (line.trim() === '') {
      i++
      continue
    }
    // The first line is taken unconditionally. Every branch above has already
    // declined it, and a paragraph that consumed nothing would leave `i` where
    // it was - which is the whole document rendered as an unterminated loop
    // rather than as text.
    const para: string[] = [line]
    i++
    while (i < lines.length && (lines[i] ?? '').trim() !== '' && !BLOCK_START.test(lines[i] ?? '')) {
      para.push(lines[i] ?? '')
      i++
    }
    blocks.push({ kind: 'p', text: para.join(' ') })
  }
  return blocks
}

const HEADING = {
  h1: 'mt-4 mb-2 text-[15px] font-semibold text-fg',
  h2: 'mt-4 mb-2 text-[13.5px] font-semibold text-fg',
  h3: 'mt-3 mb-1 text-[12.5px] font-semibold text-fg',
}

export function Markdown({ source, className = '' }: { source: string; className?: string }) {
  const blocks = parse(source)
  return (
    <div className={`max-w-[88ch] text-[12.5px] leading-[1.55] text-pretty text-fg-muted ${className}`}>
      {blocks.map((b, i) => {
        const key = `b${i}`
        if (b.kind === 'code') {
          return (
            <pre
              key={key}
              className="my-3 overflow-x-auto rounded-md border border-line bg-bg p-3 font-mono text-[11.5px]"
            >
              <code>{b.text}</code>
            </pre>
          )
        }
        if (b.kind === 'ul') {
          return (
            <ul key={key} className="my-2 list-disc pl-5">
              {b.items.map((item, j) => (
                <li key={`${key}-${j}`} className="my-1">
                  {inline(item, `${key}-${j}`)}
                </li>
              ))}
            </ul>
          )
        }
        if (b.kind === 'p') {
          return (
            <p key={key} className="my-2">
              {inline(b.text, key)}
            </p>
          )
        }
        const Tag = b.kind
        return (
          <Tag key={key} className={HEADING[b.kind]}>
            {inline(b.text, key)}
          </Tag>
        )
      })}
    </div>
  )
}
