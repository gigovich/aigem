import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react'
import { api, ApiError } from '@/lib/api'
import { canonicalise, getRoute, replace, resync, subscribeRoute } from '@/lib/route'
import type { Route } from '@/lib/route'
import { signIn } from '@/lib/auth'
import {
  clearBanner,
  conversationId,
  dropRun,
  explain,
  flash,
  openSession,
  refresh,
  selectProject,
  setBanner,
  setLogin,
  setNav,
  setPalette,
  setQuick,
  start,
  store,
  useApp,
} from '@/state/app'
import { useInspectorContent } from '@/state/inspector'
import { useRunEvents } from '@/hooks/useRunEvents'
import { CommandPalette } from '@/shell/CommandPalette'
import { paletteItems, ordered } from '@/shell/commands'
import { Header } from '@/shell/Header'
import { Inspector } from '@/shell/Inspector'
import { handleKey } from '@/shell/keymap'
import { QuickChat } from '@/shell/QuickChat'
import { Sidebar } from '@/shell/Sidebar'
import { StatusBar } from '@/shell/StatusBar'
import { Toast, ToastHost } from '@/shell/ToastHost'
import { Activity } from '@/screens/Activity'
import { Chat } from '@/screens/Chat'
import { LoginDialog } from '@/screens/LoginDialog'
import { Models } from '@/screens/Models'
import { NewProjectDialog } from '@/screens/NewProjectDialog'
import { Run } from '@/screens/Run'
import { Skills } from '@/screens/Skills'
import { Task } from '@/screens/Task'
import { Tickets } from '@/screens/Tickets'
import { Worktrees } from '@/screens/Worktrees'
import { Modal } from '@/ui/Modal'

const FOCUSABLE = 'button:not([disabled]):not([tabindex="-1"]), [href], input, select, textarea'

/**
 * The application shell.
 *
 * It owns three things and nothing else: the sign-in, the layers that sit above
 * every screen, and the one run socket a tab holds. The last of those is why
 * the chat screen, the run screen and the quick chat are all handed the same
 * conversation rather than opening their own - three sockets per tab against a
 * daemon that allows 64 across all of them is a limit reached by opening a
 * handful of tabs.
 */
export default function App() {
  const route = useSyncExternalStore(subscribeRoute, getRoute, getRoute)
  // The whole state, not a slice: the palette is built out of what the daemon
  // offers, and a shell that subscribed to a subset would go on offering the
  // commands that were true when it last re-rendered. The store moves when a
  // collection is refetched or a layer opens - never per event of a
  // conversation, which lives in the run hook's own state.
  const app = useApp((s) => s)
  const { loading, fatal, banner, paletteOpen, quickOpen, activeRun, runs, login, phone, navOpen } =
    app
  const inspector = useInspectorContent()
  const [signedIn, setSignedIn] = useState(false)
  const [query, setQuery] = useState('')
  const [index, setIndex] = useState(0)
  const [newProject, setNewProject] = useState(false)
  const [removing, setRemoving] = useState<string>('')

  useEffect(() => {
    const abort = new AbortController()
    signIn(abort.signal)
      .then(() => {
        // The exchange rewrote the address bar to take the token out of it, and
        // a replaceState raises no popstate - so the router is told by hand.
        resync()
        canonicalise()
        setSignedIn(true)
      })
      .catch((err: Error) => {
        if (err.name === 'AbortError') return
        store.set((s) => ({ ...s, loading: false, fatal: err.message }))
      })
    return () => abort.abort()
  }, [])

  useEffect(() => {
    if (!signedIn) return
    return start()
  }, [signedIn])

  /**
   * The conversation this tab is attached to: the run in the address bar, the
   * one chosen in the session list, or the most recent one there is.
   */
  const runId = useMemo(
    () => conversationId(route, runs, activeRun, app.project) ?? '',
    [route, activeRun, runs, app.project],
  )

  const conversation = useRunEvents(runId || undefined)


  useEffect(() => {
    if (!conversation.refusal) return
    setBanner(conversation.refusal)
    conversation.dismissRefusal()
  }, [conversation])

  const removeSession = useCallback(async (id: string) => {
    setRemoving('')
    try {
      await api.removeRun(id)
    } catch (err) {
      // A 404 means another tab deleted it first, which is the outcome asked for.
      if (!(err instanceof ApiError && err.status === 404)) {
        setBanner(explain(err))
        return
      }
    }
    // Dropped before leaving the view, so the fallback conversation is never
    // the run that was just deleted, even when the refresh fails.
    dropRun(id)
    await refresh.runs()
    const open = getRoute()
    if ((open.screen === 'chat' || open.screen === 'run') && open.id === id) replace({ screen: 'chat' })
    flash('Session deleted.')
  }, [])

  const items = useMemo(() => paletteItems(app), [app])
  const matches = useMemo(() => ordered(items, query), [items, query])

  const layerOpen = paletteOpen || quickOpen || newProject || removing !== '' || navOpen

  // The palette's list and highlight are read through a ref so the window
  // listener below is registered once. With them in the dependency array it was
  // torn down and re-added on every keystroke in the palette. Written in an
  // effect rather than during render: a ref is not part of the render output.
  const paletteRef = useRef({ matches, index })
  useEffect(() => {
    paletteRef.current = { matches, index }
  }, [matches, index])

  const drawer = useRef<HTMLDivElement>(null)
  const wasOpen = useRef(false)
  useEffect(() => {
    if (!phone) {
      wasOpen.current = false
      return
    }
    if (navOpen) {
      drawer.current?.querySelector<HTMLElement>(FOCUSABLE)?.focus()
    } else if (wasOpen.current) {
      document.querySelector<HTMLElement>('[aria-label="Open navigation"]')?.focus()
    }
    wasOpen.current = navOpen
  }, [phone, navOpen])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const handled = handleKey(
        e,
        {
          anyOpen: layerOpen,
          confirmOpen: removing !== '',
          modalOpen: newProject,
          paletteOpen,
        },
        {
          closeLayers: () => {
            setPalette(false)
            setQuick(false)
            setNav(false)
            setNewProject(false)
            setRemoving('')
          },
          togglePalette: () => {
            setQuery('')
            setIndex(0)
            setPalette(!store.get().paletteOpen)
          },
          toggleQuick: () => setQuick(!store.get().quickOpen),
          // Unused: the keymap deliberately does not bind Enter to a
          // destructive confirm. See handleKey.
          confirm: () => undefined,
          paletteMove: (delta) =>
            setIndex((i) =>
              Math.max(0, Math.min(paletteRef.current.matches.length - 1, i + delta)),
            ),
          paletteRun: () => {
            const { matches: shown, index: at } = paletteRef.current
            shown[at]?.run()
          },
          focusFilter: () => {
            document.querySelector<HTMLElement>('[data-filter]')?.focus()
          },
        },
      )
      if (handled) e.preventDefault()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [layerOpen, paletteOpen, newProject, removing])

  const sidebar = <Sidebar route={route} onNewProject={() => setNewProject(true)} />

  if (fatal) {
    return (
      <main className="flex h-full flex-col items-center justify-center gap-3 p-6 text-center">
        <h1 className="m-0 text-base font-semibold tracking-tight">Aigem</h1>
        <p role="status" className="m-0 max-w-[60ch] font-mono text-meta text-fg-subtle">
          {fatal}
        </p>
      </main>
    )
  }

  if (loading || !signedIn) {
    return (
      <main className="flex h-full flex-col items-center justify-center gap-3">
        <h1 className="m-0 text-base font-semibold tracking-tight">Aigem</h1>
        <p role="status" aria-live="polite" className="m-0 font-mono text-meta text-fg-subtle">
          connecting…
        </p>
      </main>
    )
  }

  return (
    <div className="flex h-full flex-col overflow-hidden bg-bg">
      <Header crumbs={crumbs(route)} />
      <div className="flex min-h-0 flex-1">
        {!phone && sidebar}
        {phone && navOpen && (
          <>
            <button
              type="button"
              aria-label="Close navigation"
              onClick={() => setNav(false)}
              className="fixed inset-0 z-[64] bg-black/40"
            />
            {/* eslint-disable-next-line jsx-a11y/no-noninteractive-element-interactions --
                a dialog is not an interactive role, and the trap must listen where the keys bubble */}
            <div
              ref={drawer}
              role="dialog"
              aria-modal="true"
              aria-label="Navigation menu"
              onKeyDown={(e) => {
                if (e.key !== 'Tab') return
                const items = drawer.current?.querySelectorAll<HTMLElement>(FOCUSABLE)
                if (!items || items.length === 0) return
                const first = items[0]!
                const last = items[items.length - 1]!
                if (e.shiftKey && document.activeElement === first) {
                  e.preventDefault()
                  last.focus()
                } else if (!e.shiftKey && document.activeElement === last) {
                  e.preventDefault()
                  first.focus()
                }
              }}
              className="fixed inset-y-0 left-0 z-[65] flex w-60 shadow-panel"
            >
              {sidebar}
            </div>
          </>
        )}
        <main className="flex min-w-0 flex-1 flex-col overflow-hidden bg-surface">
          {banner && (
            <div
              role="alert"
              className="flex items-start gap-3 border-b border-line bg-bg px-4.5 py-2 text-[0.8125rem] text-attention"
            >
              <span className="min-w-0 flex-1">{banner}</span>
              <button
                type="button"
                onClick={clearBanner}
                aria-label="Dismiss"
                className="text-fg-subtle hover:text-fg"
              >
                <span aria-hidden="true">×</span>
              </button>
            </div>
          )}
          <Screen
            route={route}
            runId={runId}
            conversation={conversation}
            onNew={() => void openSession()}
            onRemove={setRemoving}
          />
        </main>
        <Inspector content={inspector} />
      </div>
      <StatusBar />

      {paletteOpen && (
        <CommandPalette
          matches={matches}
          query={query}
          onQuery={(q) => {
            setQuery(q)
            setIndex(0)
          }}
          index={index}
          onIndex={setIndex}
          grouped={!query.trim()}
        />
      )}
      {quickOpen && (
        <QuickChat
          run={conversation.view}
          runId={runId}
          ready={conversation.state === 'open'}
          onSubmit={(text) => conversation.send({ op: 'submit', text })}
        />
      )}
      {login && <LoginDialog provider={login} onClose={() => setLogin('')} />}
      {newProject && <NewProjectDialog onClose={() => setNewProject(false)} />}
      {removing && (
        <Modal
          title="Delete this session?"
          onClose={() => setRemoving('')}
          confirm={{
            label: 'Delete',
            danger: true,
            onClick: () => void removeSession(removing),
          }}
        >
          This cannot be undone. A running session ends, and its transcript is deleted. Files it
          wrote to disk stay as they are.
        </Modal>
      )}
      <ToastHost />
      <Toast />
    </div>
  )
}

type Conversation = ReturnType<typeof useRunEvents>

function Screen({
  route,
  runId,
  conversation,
  onNew,
  onRemove,
}: {
  route: Route
  runId: string
  conversation: Conversation
  onNew: () => void
  onRemove: (id: string) => void
}) {
  switch (route.screen) {
    case 'chat':
      return (
        <Chat
          run={conversation.view}
          runId={runId}
          state={conversation.state}
          reason={conversation.reason}
          send={conversation.send}
          selected={route.id}
          onNew={onNew}
          onRemove={onRemove}
        />
      )
    case 'run':
      return (
        <Run
          run={conversation.view}
          runId={runId}
          state={conversation.state}
          send={conversation.send}
        />
      )
    case 'models':
      return <Models selected={route.id} />
    case 'skills':
      return <Skills selected={route.id} />
    case 'activity':
      return <Activity />
    case 'tickets':
      return <Tickets />
    case 'task':
      return <Task id={route.id} />
    case 'repos':
      return <Worktrees />
    case 'projects':
      return <ProjectSelect id={route.id} />
    default: {
      // Exhaustive: adding a screen to SCREENS without a case here would
      // otherwise render nothing at all, from a sidebar row that navigates to a
      // blank panel with no error anywhere.
      const unreachable: never = route.screen
      return unreachable
    }
  }
}

/** `/projects/{id}` is a selection, not a screen: choose the project and go to its sessions. */
function ProjectSelect({ id }: { id?: string }) {
  useEffect(() => {
    // The list first: a link is where a project id arrives from outside this
    // tab, and selecting one refuses an id the page has not been told about.
    void refresh.projects().then(() => selectProject(id ?? ''))
    replace({ screen: 'chat' })
  }, [id])
  return null
}

const TITLES: Record<Route['screen'], string> = {
  tickets: 'Tickets',
  task: 'Task',
  run: 'Run',
  chat: 'Sessions',
  repos: 'Worktrees',
  models: 'Models',
  skills: 'Skills',
  activity: 'Activity',
  projects: 'Projects',
}

function crumbs(route: Route): { label: string; route?: Route }[] {
  const trail: { label: string; route?: Route }[] = [
    { label: 'local', route: { screen: 'chat' } },
    { label: TITLES[route.screen], route: { screen: route.screen } },
  ]
  if (route.id) trail.push({ label: route.id })
  return trail
}
