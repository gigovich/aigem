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
import { ControlKind } from '@/lib/wire'
import type {
  Activity,
  Command,
  Meta,
  Model,
  ProviderUsage,
  Run,
  Skills,
} from '@/lib/wire'

export type Theme = 'mocha' | 'latte'
export type Density = 'dense' | 'comfortable'

/** What the inspector is describing, or nothing. */
export type Selection =
  | { kind: 'run'; id: string }
  | { kind: 'model'; id: string }
  | { kind: 'skill'; id: string }
  | null

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
  selection: Selection
  paletteOpen: boolean
  quickOpen: boolean
  toast: string
  /** A refusal worth showing: the daemon's own sentence, rendered as text. */
  banner: string
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

export const store = createStore<AppState>({
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
  // The design opens with the inspector shown, and closed below the breakpoint
  // where there is no room for it.
  inspectorOpen: window.innerWidth >= NARROW_AT,
  selection: null,
  paletteOpen: false,
  quickOpen: false,
  toast: '',
  banner: '',
})

export function useApp<S>(select: (state: AppState) => S): S {
  return useStore(store, select)
}

function patch(next: Partial<AppState>) {
  store.set((s) => ({ ...s, ...next }))
}

export function has(feature: string): boolean {
  return store.get().meta?.features[feature] === true
}

/**
 * Read one collection, if this daemon serves it.
 *
 * A route whose feature is absent answers 501, and asking anyway would fill the
 * page with errors about screens it correctly does not offer.
 */
async function load<K extends keyof AppState>(
  feature: string,
  key: K,
  fetcher: () => Promise<AppState[K]>,
) {
  if (!has(feature)) return
  try {
    patch({ [key]: await fetcher() })
  } catch (err) {
    if (err instanceof ApiError && err.status === 501) return
    setBanner(describe(err))
  }
}

export const refresh = {
  runs: () => load('runs', 'runs', () => api.runs()),
  models: () => load('models', 'models', () => api.models()),
  skills: () => load('skills', 'skills', () => api.skills()),
  commands: () => load('commands', 'commands', () => api.commands()),
  usage: () => load('usage', 'usage', () => api.usage()),
  // Newest first is what the design draws, and the feed is written oldest
  // first, so the reversal happens once here rather than in the screen.
  activity: () =>
    load('activity', 'activity', async () => (await api.activity(0, 200)).reverse()),
}

/** Read everything this daemon offers. It is the gap recovery and the boot. */
export async function refreshAll() {
  await Promise.all(Object.values(refresh).map((fn) => fn()))
}

/**
 * What to show a person about an error.
 *
 * A 400 from this API is a sentence written to be read; a 500 carries nothing,
 * and inventing a detail for it would be worse than saying the daemon failed.
 */
export function describe(err: unknown): string {
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

export function setTheme(theme: Theme) {
  document.documentElement.dataset.theme = theme
  write('aigem.theme', theme)
  patch({ theme })
}

export function setDensity(density: Density) {
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

export function select(selection: Selection) {
  store.set((s) => ({
    ...s,
    selection,
    // Selecting something with the inspector closed is how a person opens it;
    // below the breakpoint there is no room, and it stays shut.
    inspectorOpen: selection && !s.narrow ? true : s.inspectorOpen,
  }))
}

export function setPalette(open: boolean) {
  patch({ paletteOpen: open })
}

export function setQuick(open: boolean) {
  patch({ quickOpen: open })
}

/** The design's breakpoint: below it the inspector closes and stays closed. */
export function applyWidth(width: number) {
  const narrow = width < NARROW_AT
  store.set((s) =>
    s.narrow === narrow ? s : { ...s, narrow, inspectorOpen: narrow ? false : s.inspectorOpen },
  )
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
      if (!store.get().meta) patch({ loading: false, fatal: describe(err) })
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
