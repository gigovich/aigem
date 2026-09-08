import { beforeEach, expect, test } from 'vitest'
import { format, getRoute, HOME, navigate, parse, replace, resync, SCREENS, subscribeRoute } from './route'

beforeEach(() => {
  window.history.replaceState(null, '', '/')
  resync()
})

test('parses every screen the design names', () => {
  for (const screen of SCREENS) {
    expect(parse(`/${screen}`)).toEqual({ screen })
  }
})

test('carries the one id a route can hold, decoded', () => {
  expect(parse('/run/r-1')).toEqual({ screen: 'run', id: 'r-1' })
  expect(parse('/skills/code%2Freview')).toEqual({ screen: 'skills', id: 'code/review' })
})

// This is a local tool with eight screens. A typo in the address bar is better
// answered by showing the application than by a "not found" page nothing links
// to - and the root is what the daemon's sign-in link points at.
test('falls back to the home screen for anything it does not know', () => {
  expect(parse('/')).toEqual(HOME)
  expect(parse('')).toEqual(HOME)
  expect(parse('/nowhere')).toEqual(HOME)
})

test('formats back to what it parsed', () => {
  for (const path of ['/run/r-1', '/models', '/skills/code%2Freview']) {
    expect(format(parse(path))).toBe(path)
  }
})

test('navigating pushes a real URL and tells its listeners', () => {
  let calls = 0
  const stop = subscribeRoute(() => calls++)
  navigate({ screen: 'models' })

  expect(window.location.pathname).toBe('/models')
  expect(getRoute()).toEqual({ screen: 'models' })
  expect(calls).toBe(1)
  stop()
})

// useSyncExternalStore compares snapshots by identity: a getRoute that built a
// fresh object per call would re-render forever, and one that announced a route
// it is already on would do it on every click of the current tab.
test('navigating to the route it is already on changes nothing', () => {
  navigate({ screen: 'models' })
  let calls = 0
  const stop = subscribeRoute(() => calls++)
  const before = getRoute()
  navigate({ screen: 'models' })

  expect(calls).toBe(0)
  expect(getRoute()).toBe(before)
  stop()
})

test('replacing does not grow the history', () => {
  const depth = window.history.length
  replace({ screen: 'activity' })
  expect(window.location.pathname).toBe('/activity')
  expect(window.history.length).toBe(depth)
})

// The sign-in rewrites the address bar to take the token out of it, and a
// replaceState raises no popstate. Without this the store would keep the route
// it read before the page held a credential.
test('resync adopts a URL that was rewritten underneath it', () => {
  let calls = 0
  const stop = subscribeRoute(() => calls++)
  window.history.replaceState(null, '', '/skills/code-review')
  expect(getRoute()).toEqual(HOME)

  resync()
  expect(getRoute()).toEqual({ screen: 'skills', id: 'code-review' })
  expect(calls).toBe(1)
  stop()
})
