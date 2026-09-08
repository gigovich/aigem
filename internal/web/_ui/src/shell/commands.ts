/**
 * What the palette offers, and how it is filtered.
 *
 * The list is phase one's: the canvas also carries projects, tickets, epics and
 * worktrees, and every one of those would be a command that navigates to an
 * empty state. A palette that offers what does not work is worse than a short
 * palette.
 */

import { navigate } from '@/lib/route'
import type { Route } from '@/lib/route'
import { flash, setDraft, setPalette, setQuick, store, toggleDensity, toggleTheme } from '@/state/app'
import type { AppState } from '@/state/app'

export type PaletteGroup = 'Navigate' | 'Create' | 'Execute' | 'Preferences'

export type PaletteItem = {
  id: string
  label: string
  hint: string
  icon: string
  group: PaletteGroup
  run: () => void
}

const go = (route: Route) => () => {
  setPalette(false)
  navigate(route)
}

/** The commands this daemon can actually carry out, given its feature map. */
export function paletteItems(state: AppState, actions: { newSession: () => void }): PaletteItem[] {
  const features = state.meta?.features ?? {}
  const items: PaletteItem[] = []

  if (features.runs) {
    items.push({
      id: 'sessions',
      label: 'Open agent session',
      hint: 'steer a run manually',
      icon: '▭',
      group: 'Navigate',
      run: go({ screen: 'chat' }),
    })
    items.push({
      id: 'new-session',
      label: 'New session',
      hint: 'empty transcript',
      icon: '+',
      group: 'Create',
      run: () => {
        setPalette(false)
        actions.newSession()
      },
    })
  }
  if (features.activity) {
    items.push({
      id: 'activity',
      label: 'Activity feed',
      hint: 'everything this daemon did',
      icon: '≡',
      group: 'Navigate',
      run: go({ screen: 'activity' }),
    })
  }
  if (features.models) {
    items.push({
      id: 'models',
      label: 'Switch model',
      hint: state.meta?.defaultModel ?? 'the routing pool',
      icon: '◇',
      group: 'Execute',
      run: go({ screen: 'models' }),
    })
  }
  if (features.skills) {
    items.push({
      id: 'skills',
      label: 'Open skill',
      hint: `${state.skills.items.length} loaded`,
      icon: '◈',
      group: 'Navigate',
      run: go({ screen: 'skills' }),
    })
    const pending = state.skills.pending?.names.length ?? 0
    if (pending > 0) {
      items.push({
        id: 'trust-skills',
        label: 'Review project skills',
        hint: `${pending} awaiting approval`,
        icon: '!',
        group: 'Execute',
        run: go({ screen: 'skills' }),
      })
    }
  }
  items.push({
    id: 'quick-chat',
    label: 'Quick chat',
    hint: '⌘J · ask anything',
    icon: '▭',
    group: 'Navigate',
    run: () => {
      setPalette(false)
      setQuick(true)
    },
  })
  // The daemon's own catalogue. A command is run inside a conversation, so this
  // drops the person into the composer with it typed, which is where a slash
  // command is entered anyway.
  //
  // The daemon spells these with the slash already on ("/new"), so it is put
  // back only when it is missing - a label of "//new" would be a command nobody
  // could find by typing its name.
  for (const c of state.commands) {
    const name = c.name.startsWith('/') ? c.name : `/${c.name}`
    items.push({
      id: `cmd-${name}`,
      label: name,
      hint: c.description,
      icon: '›',
      group: 'Execute',
      run: () => {
        setPalette(false)
        setDraft(`${name} `)
        navigate({ screen: 'chat' })
        // After the composer has been re-rendered with the text in it: the
        // palette has no reference to it, and the point of choosing a command
        // here is to go on typing its argument.
        queueMicrotask(() => document.querySelector<HTMLElement>('[data-composer]')?.focus())
      },
    })
  }
  items.push(
    {
      id: 'theme',
      label: 'Toggle theme',
      hint: 'mocha / latte',
      icon: '◐',
      group: 'Preferences',
      run: () => {
        setPalette(false)
        toggleTheme()
        flash(`Theme: ${store.get().theme}`)
      },
    },
    {
      id: 'density',
      label: 'Toggle density',
      hint: 'dense / comfortable',
      icon: '≡',
      group: 'Preferences',
      run: () => {
        setPalette(false)
        toggleDensity()
        flash(`Density: ${store.get().density}`)
      },
    },
  )
  return items
}

/**
 * A subsequence match over the label and the hint.
 *
 * Subsequence rather than substring because that is what makes a palette worth
 * having: "nsn" finds "New session". The score prefers an early, tight match,
 * so the thing whose name starts with what was typed comes first.
 */
export function score(item: PaletteItem, query: string): number {
  if (!query) return 0
  const haystack = `${item.label} ${item.hint}`.toLowerCase()
  const needle = query.toLowerCase().replace(/\s+/g, '')
  let at = -1
  let first = -1
  let gaps = 0
  for (const ch of needle) {
    const found = haystack.indexOf(ch, at + 1)
    if (found < 0) return -1
    if (first < 0) first = found
    else gaps += found - at - 1
    at = found
  }
  return first * 2 + gaps
}

export function filter(items: PaletteItem[], query: string): PaletteItem[] {
  if (!query.trim()) return items
  return items
    .map((item) => ({ item, s: score(item, query) }))
    .filter(({ s }) => s >= 0)
    .sort((a, b) => a.s - b.s)
    .map(({ item }) => item)
}

export const GROUP_ORDER: PaletteGroup[] = ['Navigate', 'Create', 'Execute', 'Preferences']

/** The filtered list, grouped and flattened in the canvas's group order. */
export function ordered(items: PaletteItem[], query: string): PaletteItem[] {
  const matched = filter(items, query)
  if (query.trim()) return matched
  return GROUP_ORDER.flatMap((g) => matched.filter((i) => i.group === g))
}
