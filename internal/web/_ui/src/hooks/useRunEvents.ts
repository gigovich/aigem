/**
 * One run's socket, owned by the component that draws it.
 *
 * The socket and the sequence live here rather than in the application store,
 * because a run stream is only worth holding while something is looking at it:
 * a page that kept every run it had visited attached would hold 32 sockets
 * against a daemon that allows 64 across every tab.
 */

import { useCallback, useEffect, useRef, useState } from 'react'
import { connectRun } from '@/lib/socket'
import type { RunConnection, RunSocketState } from '@/lib/socket'
import type { RunOp } from '@/lib/wire'
import { applyAll, emptyRun } from '@/state/run'
import type { RunView } from '@/state/run'

export type UseRun = {
  view: RunView
  state: RunSocketState
  /** Why the stream stopped, when it stopped for a reason worth showing. */
  reason: string
  /** The last operation the daemon refused, as a sentence for the person. */
  refusal: string
  send: (op: RunOp) => boolean
  dismissRefusal: () => void
}

type Session = {
  id: string | undefined
  view: RunView
  state: RunSocketState
  reason: string
  refusal: string
}

/**
 * What this tab calls itself on a run's presence list.
 *
 * One per page load, not per run: what the other clients need to tell apart is
 * the tabs, and a person with two of them open on the same conversation is
 * exactly who presence is for. It is not an identity - the daemon serves one
 * signed-in person - so it says nothing about who they are.
 */
const TAB = `tab ${Math.random().toString(36).slice(2, 6)}`

const fresh = (id: string | undefined): Session => ({
  id,
  view: emptyRun(),
  state: 'connecting',
  reason: '',
  refusal: '',
})

export function useRunEvents(id: string | undefined): UseRun {
  const [session, setSession] = useState<Session>(() => fresh(id))
  const conn = useRef<RunConnection | null>(null)

  // Adjusted during render rather than in an effect: switching runs must not
  // paint the previous conversation's transcript under the new run's header for
  // the frame between the commit and the effect. React re-runs this component
  // immediately and commits only the second result.
  if (session.id !== id) setSession(fresh(id))

  useEffect(() => {
    if (!id) return
    const merge = (patch: Partial<Session>) =>
      setSession((s) => (s.id === id ? { ...s, ...patch } : s))
    const c = connectRun(
      id,
      {
        // Batched by the caller: a replay hands a whole page at once, so
        // folding it in one setState is one render rather than two thousand.
        onEvents: (events) =>
          setSession((s) => {
            if (s.id !== id) return s
            return { ...s, view: events.length === 0 ? emptyRun() : applyAll(s.view, events) }
          }),
        onRefusal: (err) => merge({ refusal: err.error }),
        onStatus: (state, why) =>
          setSession((s) =>
            s.id !== id || (s.state === state && s.reason === (why ?? ''))
              ? s
              : { ...s, state, reason: why ?? '' },
          ),
      },
      { label: TAB },
    )
    conn.current = c
    return () => {
      conn.current = null
      c.close()
    }
  }, [id])

  const send = useCallback((op: RunOp) => conn.current?.send(op) ?? false, [])
  const dismissRefusal = useCallback(
    () => setSession((s) => (s.refusal ? { ...s, refusal: '' } : s)),
    [],
  )

  return {
    view: session.view,
    state: session.state,
    reason: session.reason,
    refusal: session.refusal,
    send,
    dismissRefusal,
  }
}
