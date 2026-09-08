/**
 * A store for `useSyncExternalStore`, in about forty lines.
 *
 * The state this application holds is one object of collections the daemon
 * owns, refetched when the control stream says they moved. A reducer library
 * would add a vocabulary for describing that; what it would not add is anything
 * React 19 does not already do with a snapshot and a subscribe.
 *
 * The rules the hook imposes and this honours: a snapshot is immutable, it is
 * compared by identity, and it must be the same object until something actually
 * changes - a store that builds a fresh one per read renders forever.
 */

import { useRef, useSyncExternalStore } from 'react'

export type Store<T> = {
  get: () => T
  set: (next: T | ((prev: T) => T)) => void
  subscribe: (listener: () => void) => () => void
}

export function createStore<T>(initial: T): Store<T> {
  let state = initial
  const listeners = new Set<() => void>()
  return {
    get: () => state,
    set: (next) => {
      const value = typeof next === 'function' ? (next as (prev: T) => T)(state) : next
      if (Object.is(value, state)) return
      state = value
      // Copied first: a listener that unsubscribes during the walk would
      // otherwise mutate the set being iterated.
      for (const l of [...listeners]) l()
    },
    subscribe: (listener) => {
      listeners.add(listener)
      return () => {
        listeners.delete(listener)
      }
    },
  }
}

type Memo<T, S> = { state: T; select: (state: T) => S; value: S }

/**
 * Read a slice of a store.
 *
 * The selector's result is cached against both the snapshot and the selector it
 * was computed from, so a selector that builds an object - `(s) => ({ a: s.a })`
 * - returns the same object until one of them changes. Without that the hook
 * sees a new snapshot on every read and re-renders forever; caching on the
 * state alone would instead ignore a selector that closed over a new prop.
 */
export function useStore<T, S>(store: Store<T>, select: (state: T) => S): S {
  const memo = useRef<Memo<T, S> | null>(null)
  const read = () => {
    const state = store.get()
    const hit = memo.current
    if (hit && Object.is(hit.state, state) && hit.select === select) return hit.value
    const value = select(state)
    memo.current = { state, select, value }
    return value
  }
  return useSyncExternalStore(store.subscribe, read, read)
}
