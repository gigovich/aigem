/**
 * The control stream: one socket per tab, read-only.
 *
 * It carries no state of its own. Every frame is a revision and a name, and
 * what the page does with one is refetch the collection the name points at -
 * so this file's whole job is to stay connected, hand the frames up, and say
 * when a gap means everything has to be read again.
 *
 * The gap rule is the daemon's: a `rev` more than one past the last handled
 * means something was missed. Not "not equal to" - a frame that is not a
 * mutation repeats the revision the client is already at, and reading that as a
 * gap would make every one of the page's own mistakes cost a refetch.
 */

import { socketURL } from './api'
import type { ControlFrame, Meta } from './wire'

export type ControlHandlers = {
  /** A fresh base. It arrives on every connection, including reconnections. */
  onHello: (meta: Meta) => void
  /** One published mutation, by name. `data` is the daemon's payload. */
  onDelta: (kind: string, data: unknown) => void
  /** Something was missed: read every collection this page shows again. */
  onGap: () => void
  /** Connected state, for the status bar. */
  onStatus?: (state: ControlState) => void
}

export type ControlState = 'connecting' | 'open' | 'closed'

/**
 * Reconnect backoff. Full jitter over an exponential ceiling: a daemon that
 * restarts has every tab dialling it, and a fixed delay makes them arrive
 * together for as long as the page is open.
 */
const BACKOFF_MIN = 500
const BACKOFF_MAX = 15_000

export function backoff(attempt: number, random: () => number = Math.random): number {
  const ceiling = Math.min(BACKOFF_MAX, BACKOFF_MIN * 2 ** Math.max(0, attempt))
  return Math.round(BACKOFF_MIN + random() * (ceiling - BACKOFF_MIN))
}

export type ControlConnection = { close: () => void }

/**
 * Connect, and keep connecting.
 *
 * The returned handle is the only way to stop: an unmounted page that left this
 * running would go on refetching collections nothing renders.
 */
export function connectControl(
  handlers: ControlHandlers,
  options: { random?: () => number } = {},
): ControlConnection {
  let socket: WebSocket | null = null
  let timer: ReturnType<typeof setTimeout> | null = null
  let attempt = 0
  let stopped = false
  // The last revision this connection delivered. It is per connection because
  // hello re-bases: a reconnection's numbers are not continuous with the ones
  // before it, and comparing across the break would report a gap that the
  // reconnection has already recovered from.
  let rev = 0
  let based = false

  const status = (state: ControlState) => handlers.onStatus?.(state)

  const retry = () => {
    if (stopped) return
    const delay = backoff(attempt++, options.random)
    timer = setTimeout(open, delay)
  }

  const frame = (raw: string) => {
    let msg: ControlFrame
    try {
      msg = JSON.parse(raw) as ControlFrame
    } catch {
      // A frame this page cannot parse is a frame it has missed the meaning of.
      handlers.onGap()
      return
    }
    if (msg.type === 'hello') {
      rev = msg.rev
      based = true
      attempt = 0
      handlers.onHello(msg.data)
      return
    }
    // Before the gap check: a page that has not been given a base has nothing
    // to compare against, and the daemon sends hello first on every connection.
    if (!based) return
    if (msg.rev > rev + 1) handlers.onGap()
    rev = Math.max(rev, msg.rev)
    // "A delta that arrives with no data carries nothing the client can apply"
    // - the daemon had nothing to say or could not encode it, and the honest
    // response is to read the collections again.
    if (msg.data === undefined) {
      handlers.onGap()
      return
    }
    handlers.onDelta(msg.type, msg.data)
  }

  function open() {
    if (stopped) return
    timer = null
    based = false
    status('connecting')
    const ws = new WebSocket(socketURL('/api/socket'))
    socket = ws
    ws.onopen = () => status('open')
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') frame(e.data)
    }
    ws.onclose = () => {
      if (socket !== ws) return
      socket = null
      status('closed')
      retry()
    }
    // A failed handshake reports nothing a browser can read, so there is
    // nothing to do here that onclose does not already do - it always follows.
    ws.onerror = () => {}
  }

  open()

  return {
    close: () => {
      stopped = true
      if (timer) clearTimeout(timer)
      timer = null
      const ws = socket
      socket = null
      ws?.close()
    },
  }
}
