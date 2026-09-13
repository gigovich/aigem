/**
 * The run stream: one socket per open run per tab.
 *
 * It is the opposite of the control stream in both directions. It replays -
 * `?since=` is the last sequence this client applied, and the daemon splices
 * the backlog in front of the live events - and it takes the conversation's own
 * operations up.
 *
 * Two failures shape this file:
 *
 *   - `desync` means this client fell too far behind and the socket is about to
 *     end. Recovery is to refetch from the sequence the event names and dial
 *     again, not to wait on a stream that has already closed.
 *   - a refused handshake tells a browser nothing but that it failed. So when
 *     the socket will not open, the page asks over HTTP why - which is the same
 *     request it would have made to catch up anyway.
 */

import { api, ApiError, socketURL } from './api'
import { backoff } from './control'
import { CLIENT_ERROR, EventKind } from './wire'
import type { ClientError, RunEvent, RunOp } from './wire'

export type RunSocketState = 'connecting' | 'open' | 'closed' | 'gone'

export type RunHandlers = {
  /** Events in order, live and replayed alike. */
  onEvents: (events: RunEvent[]) => void
  /** An operation this client sent that the daemon would not carry out. */
  onRefusal: (err: ClientError) => void
  onStatus?: (state: RunSocketState, reason?: string) => void
}

export type RunConnection = {
  /** Send one operation. Silently dropped while the socket is not open: the
   *  conversation's own events are the acknowledgement, and a queue here would
   *  replay a submit into a run the person has since left. */
  send: (op: RunOp) => boolean
  close: () => void
}

/**
 * Attach to a run and stay attached.
 *
 * `since` is the last sequence the caller already holds; every event handed to
 * `onEvents` after that is the caller's to apply in order. The connection
 * tracks the sequence itself, so a reconnection resumes where the last one
 * stopped rather than where this call started.
 */
export function connectRun(
  id: string,
  handlers: RunHandlers,
  options: { since?: number; label?: string; random?: () => number } = {},
): RunConnection {
  let socket: WebSocket | null = null
  let timer: ReturnType<typeof setTimeout> | null = null
  let attempt = 0
  let stopped = false
  let since = options.since ?? 0

  const status = (state: RunSocketState, reason?: string) => handlers.onStatus?.(state, reason)

  /**
   * Whether the run is still worth dialling, read from its record before every
   * dial: a browser cannot read the status of a refused handshake, and every
   * run is closed after a daemon restart.
   *
   * The answer is in the record's `live`, not in a status code: `GET
   * /api/runs/{id}` describes a closed run perfectly well and answers 200 for
   * it - only the routes that need the session (the socket, the artifacts)
   * answer 409. Waiting for a 409 here is waiting for something this route
   * never sends, which is a tab redialling a closed conversation for as long as
   * it is open. Every run is closed after a daemon restart, so that is the
   * ordinary case and not the edge.
   */
  const diagnose = async () => {
    try {
      const run = await api.run(id)
      // Compared against false rather than tested for truth: `live` is the one
      // boolean this API always sends, and a record that somehow arrived
      // without it must not end the stream on an absence.
      if (run.live === false) {
        stopped = true
        status('gone', 'this conversation is closed; its transcript still reads')
      }
    } catch (err) {
      if (err instanceof ApiError && (err.status === 404 || err.status === 409)) {
        stopped = true
        status('gone', err.status === 404 ? 'this run no longer exists' : err.detail)
      }
    }
  }

  /**
   * Catch up over HTTP, then dial again.
   *
   * This is the `desync` recovery and the reconnection path both: after either,
   * what the client holds may be behind what the run has, and the socket's own
   * replay is bounded by what the run still remembers.
   */
  const catchUp = async () => {
    for (;;) {
      const page = await api.runEvents(id, since, 0)
      if (page.length === 0) return
      handlers.onEvents(page)
      const last = page[page.length - 1]
      if (!last || last.seq <= since) return
      since = last.seq
      // A page that came back full is the signal to ask again; there is no
      // "more" marker on this API.
      if (page.length < 2000) return
    }
  }

  const retry = () => {
    if (stopped) return
    timer = setTimeout(() => {
      void open()
    }, backoff(attempt++, options.random))
  }

  const message = (raw: string) => {
    let msg: RunEvent | ClientError
    try {
      msg = JSON.parse(raw) as RunEvent | ClientError
    } catch {
      return
    }
    if ('kind' in msg && msg.kind === CLIENT_ERROR) {
      handlers.onRefusal(msg)
      return
    }
    const event = msg
    if (event.kind === EventKind.Desync) {
      // The socket ends after this. `from` is the last sequence that did
      // arrive, so the refetch starts there rather than from what this client
      // had applied - which may be older still.
      since = Math.max(since, event.from ?? 0)
      return
    }
    if (typeof event.seq === 'number') since = Math.max(since, event.seq)
    handlers.onEvents([event])
  }

  async function open() {
    if (stopped) return
    timer = null
    status('connecting')
    try {
      await catchUp()
    } catch (err) {
      // 410 means the history no longer reaches that point: the answer is to
      // reload the timeline from the start, which is what a zero cursor does.
      if (err instanceof ApiError && err.status === 410) {
        since = 0
        handlers.onEvents([])
      } else if (err instanceof ApiError && (err.status === 404 || err.status === 409)) {
        stopped = true
        status('gone', err.detail)
        return
      }
    }
    await diagnose()
    if (stopped) return
    const ws = new WebSocket(
      // The label is what the presence event shows the other tabs; without it
      // a terminal and a browser on the same run cannot say who is attached,
      // which is the whole reason presence exists.
      socketURL(`/api/runs/${encodeURIComponent(id)}/socket`, { since, label: options.label }),
    )
    socket = ws
    ws.onopen = () => {
      attempt = 0
      status('open')
    }
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') message(e.data)
    }
    ws.onclose = () => {
      if (socket !== ws) return
      socket = null
      status('closed')
      retry()
    }
    ws.onerror = () => {}
  }

  void open()

  return {
    send: (op) => {
      if (!socket || socket.readyState !== WebSocket.OPEN) return false
      socket.send(JSON.stringify(op))
      return true
    },
    close: () => {
      stopped = true
      if (timer) clearTimeout(timer)
      timer = null
      const ws = socket
      socket = null
      if (!ws) return
      if (ws.readyState === WebSocket.CONNECTING) ws.onopen = () => ws.close()
      else ws.close()
    },
  }
}
