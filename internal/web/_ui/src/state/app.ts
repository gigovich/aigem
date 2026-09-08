/**
 * The application's state: the daemon's collections, and the shell's own.
 *
 * The collections are caches of what the daemon owns. Nothing here decides what
 * they contain - the control stream says a name moved and the collection it
 * names is read again. That is why there is no optimistic update anywhere in
 * this file: two tabs and a terminal look at the same daemon, and a page that
 * drew what it hoped had happened would disagree with the one that did not.
 */

import { api, ApiError } from '@/lib/api'
import { connectControl } from '@/lib/control'
import type { ControlState } from '@/lib/control'
import { createStore, useStore } from '@/lib/store'
import { CLIENT_ERROR, ControlKind } from '@/lib/wire'
import type {
  Activity,
  Command,
  Feature,
  Meta,
  Model,
  ProviderUsage,
  Run,
  Skills,
} from '@/lib/wire'

export type Theme = 'mocha' | 'latte'
export type Density = 'dense' | 'comfortable'

export type AppState = {
  meta: Meta | null
  /** The first load has not finished; the shell shows a connecting state. */
  loading: boolean
  /** Why the page has nothing, when it has nothing. */
  fatal: string
  control: ControlState

  runs: Run[]
  models: Model[]
  skills: Skills
  commands: Command[]
  usage: ProviderUsage[]
  activity: Activity[]

  theme: Theme
  density: Density
  narrow: boolean
  inspectorOpen: boolean
  paletteOpen: boolean
  quickOpen: boolean
  toast: string
  /** A refusal worth showing: the daemon's own sentence, rendered as text. */
  banner: string
  /**
   * The conversation the shell is attached to. One per tab: the chat screen,
   * the run screen and the quick chat all draw the same run, and a socket each
   * would be three against a daemon that allows 64 across every tab.
   */
  activeRun: string
  /**
   * A command the palette chose, for the composer to pick up.
   *
   * It is a handover and not the composer's value: binding the text itself to
   * the store would move the store on every keystroke, and the shell subscribes
   * to all of it. The chat screen adopts it and keeps its own text from then on.
   *
   * The counter is what makes choosing the same command twice a second
   * handover: comparing the text alone, the second choice looks like the first
   * and the composer stays empty.
   */
  pendingCommand: { text: string; nth: number }
}

const EMPTY_SKILLS: Skills = { items: [] }

export const NARROW_AT = 1120

function initialTheme(): Theme {
  return read('aigem.theme') === 'latte' ? 'latte' : 'mocha'
}

function initialDensity(): Density {
  return read('aigem.density') === 'comfortable' ? 'comfortable' : 'dense'
}

/**
 * localStorage throws rather than returning null in a browser told to block
 * site data, and a preference nobody could read must not be the reason the
 * application does not start.
 */
function read(key: string): string | null {
  try {
    return window.localStorage.getItem(key)
  } catch {
    return null
  }
}

function write(key: string, value: string) {
  try {
    window.localStorage.setItem(key, value)
  } catch {
    /* a preference that could not be saved is not worth an error */
  }
}

/**
 * The state a page starts in, before it has spoken to the daemon.
 *
 * A function rather than a literal so there is one description of "nothing has
 * been read yet" - which is what a fresh tab holds, and what a test harness
 * puts the store back to between cases.
 */
export function initialState(): AppState {
  return {
    meta: null,
    loading: true,
    fatal: '',
    control: 'connecting',
    runs: [],
    models: [],
    skills: EMPTY_SKILLS,
    commands: [],
    usage: [],
    activity: [],
    theme: initialTheme(),
    density: initialDensity(),
    narrow: window.innerWidth < NARROW_AT,
    // The design opens with the inspector shown, and closed below the
    // breakpoint where there is no room for it.
    inspectorOpen: window.innerWidth >= NARROW_AT,
    paletteOpen: false,
    quickOpen: false,
    toast: '',
    banner: '',
    activeRun: '',
    pendingCommand: { text: '', nth: 0 },
  }
}

export const store = createStore<AppState>(initialState())

export function useApp<S>(select: (state: AppState) => S): S {
  return useStore(store, select)
}

function patch(next: Partial<AppState>) {
  store.set((s) => ({ ...s, ...next }))
}

/**
 * The three run counts the chrome shows.
 *
 * Derived here rather than at each of the five places that needed them, because
 * `live`, `running` and `waiting` are three separate flags on the record and
 * what each means is the daemon's to change.
 */
export function runCounts(s: AppState): { live: number; running: number; waiting: number } {
  let live = 0
  let running = 0
  let waiting = 0
  for (const r of s.runs) {
    if (r.live) live++
    if (r.running) running++
    if (r.waiting) waiting++
  }
  return { live, running, waiting }
}

export function has(feature: Feature): boolean {
  // Read through rather than indexed: a daemon that answered without a feature
  // map at all must leave the page with no screens, not with no page.
  const features = store.get().meta?.features
  return features?.[feature] === true
}

/**
 * Read one collection, if this daemon serves it.
 *
 * A route whose feature is absent answers 501, and asking anyway would fill the
 * page with errors about screens it correctly does not offer.
 */
async function load<K extends keyof AppState>(
  feature: Feature,
  key: K,
  fetcher: () => Promise<AppState[K]>,
) {
  if (!has(feature)) return
  try {
    patch({ [key]: await fetcher() })
  } catch (err) {
    if (err instanceof ApiError && err.status === 501) return
    setBanner(explain(err))
  }
}

export const refresh = {
  runs: () => load('runs', 'runs', () => api.runs()),
  models: () => load('models', 'models', () => api.models()),
  skills: () => load('skills', 'skills', () => api.skills()),
  commands: () => load('commands', 'commands', () => api.commands()),
  usage: () => load('usage', 'usage', () => api.usage()),
  activity: () => load('activity', 'activity', readActivityTail),
}

/** How much of the feed a page holds; the screen shows the recent end of it. */
const ACTIVITY_SHOWN = 200
const ACTIVITY_PAGE = 1000
/**
 * How many pages the walk will read before giving up.
 *
 * The cursor check below is what ends it on a daemon behaving sensibly. This is
 * what ends it on one that is not - a page whose last entry never advances is
 * otherwise a loop that never returns, and the tab it is running in is the one
 * a person is looking at.
 */
const ACTIVITY_PAGES = 50

/**
 * The end of the activity feed, newest first.
 *
 * `since` is a cursor and `limit` takes the first N after it, so asking for two
 * hundred from zero is the two hundred *oldest* entries - which is a screen
 * that stops updating the moment the feed passes two hundred, and never says
 * so. There is no "last N" on this API, so the way to the end is to page to it.
 * The daemon trims the feed at thirty days on startup, which is what bounds
 * this loop; the page size is the route's own cap.
 */
async function readActivityTail(): Promise<Activity[]> {
  const tail: Activity[] = []
  let since = 0
  for (let read = 0; read < ACTIVITY_PAGES; read++) {
    const page = await api.activity(since, ACTIVITY_PAGE)
    if (page.length === 0) break
    tail.push(...page)
    if (tail.length > ACTIVITY_SHOWN) tail.splice(0, tail.length - ACTIVITY_SHOWN)
    const last = page[page.length - 1]
    if (!last?.seq || last.seq <= since) break
    since = last.seq
    // A page that came back short is the end; this API has no "more" marker.
    if (page.length < ACTIVITY_PAGE) break
  }
  // The design draws newest first and the feed is written oldest first.
  return tail.reverse()
}

/** Read everything this daemon offers. It is the gap recovery and the boot. */
export async function refreshAll() {
  await Promise.all(Object.values(refresh).map((fn) => fn()))
}

/**
 * What to show a person about an error.
 *
 * Named `explain` and not `describe`: the latter is Vitest's own global, in
 * scope in every test file in this tree, and a shadowed import there type-checks.
 *
 * A 400 from this API is a sentence written to be read; a 500 carries nothing,
 * and inventing a detail for it would be worse than saying the daemon failed.
 */
export function explain(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.detail) return err.detail
    if (err.status === 503) return 'The daemon is at capacity. Close a run and try again.'
    return `The daemon answered ${err.status}.`
  }
  if (err instanceof Error) return err.message
  return 'Something went wrong.'
}

let toastTimer: ReturnType<typeof setTimeout> | null = null

export function flash(message: string) {
  patch({ toast: message })
  if (toastTimer) clearTimeout(toastTimer)
  toastTimer = setTimeout(() => patch({ toast: '' }), 2200)
}

export function setBanner(message: string) {
  patch({ banner: message })
}

export function clearBanner() {
  patch({ banner: '' })
}

function setTheme(theme: Theme) {
  document.documentElement.dataset.theme = theme
  write('aigem.theme', theme)
  patch({ theme })
}

function setDensity(density: Density) {
  document.documentElement.dataset.density = density
  write('aigem.density', density)
  patch({ density })
}

export function toggleTheme() {
  setTheme(store.get().theme === 'mocha' ? 'latte' : 'mocha')
}

export function toggleDensity() {
  setDensity(store.get().density === 'dense' ? 'comfortable' : 'dense')
}

export function setInspector(open: boolean) {
  patch({ inspectorOpen: open })
}

export function toggleInspector() {
  store.set((s) => ({ ...s, inspectorOpen: !s.inspectorOpen }))
}

export function setPalette(open: boolean) {
  patch({ paletteOpen: open })
}

export function setQuick(open: boolean) {
  patch({ quickOpen: open })
}

export function setActiveRun(id: string) {
  patch({ activeRun: id })
}

export function setPendingCommand(text: string) {
  store.set((s) => ({ ...s, pendingCommand: { text, nth: s.pendingCommand.nth + 1 } }))
}

/**
 * The design's breakpoint: below it there is no room for the inspector.
 *
 * Widening brings it back rather than leaving it shut. Closing it below the
 * breakpoint is a decision the layout made, not one the person made, and a
 * window dragged narrow and back should not cost them the panel.
 */
function applyWidth(width: number) {
  const narrow = width < NARROW_AT
  store.set((s) => (s.narrow === narrow ? s : { ...s, narrow, inspectorOpen: !narrow }))
}

/**
 * Bring the page up: sign-in has already happened, so this is the first read of
 * the daemon and the subscription to everything that moves it afterwards.
 */
export function start(): () => void {
  setTheme(store.get().theme)
  setDensity(store.get().density)

  const onResize = () => applyWidth(window.innerWidth)
  window.addEventListener('resize', onResize)

  const control = connectControl({
    onHello: (meta) => {
      patch({ meta, loading: false, fatal: '' })
      void refreshAll()
    },
    onDelta: (kind) => {
      switch (kind) {
        case ControlKind.RunUpdated:
          void refresh.runs()
          break
        case ControlKind.ModelDefault:
          void refresh.models()
          void reloadMeta()
          break
        case ControlKind.AuthUpdated:
          void refresh.models()
          void refresh.usage()
          break
        case ControlKind.SkillsUpdated:
          void refresh.skills()
          void refresh.commands()
          break
        case ControlKind.ActivityUpdated:
          void refresh.activity()
          break
        case CLIENT_ERROR:
          // Not a state change: it is this connection's own mistake coming
          // back. Refetching every collection over it is the cost the gap rule
          // exists to avoid.
          break
        default:
          // A kind this build does not know is still a mutation: the collection
          // it named has moved, and reading everything is the honest answer.
          void refreshAll()
      }
    },
    onGap: () => {
      void reloadMeta()
      void refreshAll()
    },
    onStatus: (state) => patch({ control: state }),
  })

  // The socket's hello is the first meta, but a daemon that will not hold a
  // socket must still bring the page up rather than leaving it on "connecting".
  void api
    .meta()
    .then((meta) => {
      if (!store.get().meta) {
        patch({ meta, loading: false })
        void refreshAll()
      }
    })
    .catch((err: unknown) => {
      if (!store.get().meta) patch({ loading: false, fatal: explain(err) })
    })

  return () => {
    control.close()
    window.removeEventListener('resize', onResize)
  }
}

async function reloadMeta() {
  try {
    patch({ meta: await api.meta() })
  } catch {
    /* the control stream will re-base on its next hello */
  }
}
