/**
 * The router: eight screens, written by hand.
 *
 * A library would bring a matcher, a data layer and a build-time convention for
 * a set of routes that fits in the union below and is fixed by the design. The
 * daemon serves the application for every path that is not `/api/`, so a deep
 * link is a real URL rather than a fragment - which is what makes "open run" a
 * link a person can send to their other machine.
 */

/** The screens, exactly `state.screen` in the design canvas. */
export const SCREENS = [
  'tickets',
  'task',
  'run',
  'chat',
  'repos',
  'models',
  'skills',
  'activity',
] as const

export type Screen = (typeof SCREENS)[number]

/** A route is a screen and at most one id: which run, which skill. */
export type Route = { screen: Screen; id?: string }

export const HOME: Route = { screen: 'chat' }

function isScreen(value: string): value is Screen {
  return (SCREENS as readonly string[]).includes(value)
}

/**
 * Parse a pathname. Anything unrecognised is the home screen rather than a
 * "not found" page: this is a local tool with eight screens, and a typo in the
 * address bar is better answered by showing the application.
 */
export function parse(pathname: string): Route {
  const [screen, id] = pathname.replace(/^\/+/, '').split('/')
  if (!screen || !isScreen(screen)) return HOME
  return id ? { screen, id: decodeURIComponent(id) } : { screen }
}

export function format(route: Route): string {
  return route.id ? `/${route.screen}/${encodeURIComponent(route.id)}` : `/${route.screen}`
}

function sameRoute(a: Route, b: Route): boolean {
  return a.screen === b.screen && a.id === b.id
}

const listeners = new Set<() => void>()
let current = parse(window.location.pathname)

function announce() {
  for (const l of listeners) l()
}

/**
 * The snapshot is cached and only replaced when the route actually changes.
 *
 * useSyncExternalStore compares snapshots by identity and re-renders when they
 * differ, so returning a fresh object per call is an infinite render loop
 * rather than a wasted allocation.
 */
export function getRoute(): Route {
  return current
}

export function subscribeRoute(listener: () => void): () => void {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

function set(route: Route, replace: boolean) {
  if (sameRoute(route, current)) return
  current = route
  window.history[replace ? 'replaceState' : 'pushState'](null, '', format(route))
  announce()
}

export function navigate(route: Route) {
  set(route, false)
}

export function replace(route: Route) {
  set(route, true)
}

// Back and forward are the browser's, and the store has to follow them.
window.addEventListener('popstate', () => {
  const next = parse(window.location.pathname)
  if (sameRoute(next, current)) return
  current = next
  announce()
})

/**
 * Rewrite the address bar to the canonical path for the route it already holds.
 *
 * The sign-in link is the daemon's root, and the root is the home screen - so
 * without this the address bar says "/" while the page is on /chat, and
 * navigating to the screen it is already showing changes nothing that a person
 * could copy or bookmark.
 */
export function canonicalise() {
  const path = format(current)
  if (window.location.pathname === path) return
  window.history.replaceState(null, '', `${path}${window.location.search}${window.location.hash}`)
}

/**
 * Adopt whatever the address bar says now.
 *
 * The sign-in rewrites the URL to take the token out of it, and that
 * replaceState does not raise popstate - so without this the store would keep
 * the route it read before the page had a credential.
 */
export function resync() {
  const next = parse(window.location.pathname)
  if (sameRoute(next, current)) return
  current = next
  announce()
}
