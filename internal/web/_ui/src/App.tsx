import { useCallback, useEffect, useMemo, useState, useSyncExternalStore } from 'react'
import { api } from '@/lib/api'
import { canonicalise, getRoute, navigate, resync, subscribeRoute } from '@/lib/route'
import type { Route } from '@/lib/route'
import { signIn } from '@/lib/auth'
import {
  clearBanner,
  describe,
  flash,
  refresh,
  setActiveRun,
  setBanner,
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
import { ToastHost } from '@/shell/ToastHost'
import { Activity } from '@/screens/Activity'
import { Chat } from '@/screens/Chat'
import { Models } from '@/screens/Models'
import { Repos, Task, Tickets } from '@/screens/Placeholders'
import { Run } from '@/screens/Run'
import { Skills } from '@/screens/Skills'
import { Modal } from '@/ui/Modal'

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
  const { loading, fatal, banner, paletteOpen, quickOpen, activeRun, runs } = app
  const features = app.meta?.features ?? {}
  const inspector = useInspectorContent()
  const [signedIn, setSignedIn] = useState(false)
  const [query, setQuery] = useState('')
  const [index, setIndex] = useState(0)
  const [explainProjects, setExplainProjects] = useState(false)
  const [closing, setClosing] = useState<string>('')

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
   *
   * The fallback prefers a live session and settles for a closed one, because a
   * restarted daemon finds every run closed and its timeline still reads - a
   * page that showed "no session yet" over a list of conversations would be
   * describing the daemon's restart rather than the person's work.
   */
  const runId = useMemo(() => {
    if (route.screen === 'run' && route.id) return route.id
    if (activeRun && runs.some((r) => r.id === activeRun)) return activeRun
    const newest = [...runs].reverse()
    return (newest.find((r) => r.live) ?? newest[0])?.id ?? ''
  }, [route, activeRun, runs])

  const conversation = useRunEvents(runId || undefined)

  useEffect(() => {
    if (!conversation.refusal) return
    setBanner(conversation.refusal)
    conversation.dismissRefusal()
  }, [conversation])

  const newSession = useCallback(async () => {
    if (!features.runs) {
      flash('This daemon does not serve runs.')
      return
    }
    try {
      const run = await api.openRun({})
      await refresh.runs()
      setActiveRun(run.id)
      navigate({ screen: 'chat' })
    } catch (err) {
      setBanner(describe(err))
    }
  }, [features.runs])

  const closeSession = useCallback(async (id: string) => {
    setClosing('')
    try {
      await api.closeRun(id)
      await refresh.runs()
      flash('Session closed. Its transcript is still readable.')
    } catch (err) {
      setBanner(describe(err))
    }
  }, [])

  const items = useMemo(
    () => paletteItems(app, { newSession: () => void newSession() }),
    [app, newSession],
  )
  const matches = useMemo(() => ordered(items, query), [items, query])

  const layerOpen = paletteOpen || quickOpen || explainProjects || closing !== ''

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const handled = handleKey(
        e,
        {
          anyOpen: layerOpen,
          confirmOpen: closing !== '',
          modalOpen: explainProjects,
          paletteOpen,
        },
        {
          closeLayers: () => {
            setPalette(false)
            setQuick(false)
            setExplainProjects(false)
            setClosing('')
          },
          togglePalette: () => {
            setQuery('')
            setIndex(0)
            setPalette(!store.get().paletteOpen)
          },
          toggleQuick: () => setQuick(!store.get().quickOpen),
          confirm: () => void closeSession(closing),
          submitModal: () => setExplainProjects(false),
          paletteMove: (delta) =>
            setIndex((i) => Math.max(0, Math.min(matches.length - 1, i + delta))),
          paletteRun: () => matches[index]?.run(),
          focusFilter: () => {
            document.querySelector<HTMLElement>('[data-filter]')?.focus()
          },
        },
      )
      if (handled) e.preventDefault()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [layerOpen, paletteOpen, explainProjects, closing, matches, index, closeSession])

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
        <Sidebar route={route} onNewProject={() => setExplainProjects(true)} />
        <main className="flex min-w-0 flex-1 flex-col overflow-hidden bg-surface">
          {banner && (
            <div
              role="alert"
              className="flex items-start gap-3 border-b border-line bg-bg px-[18px] py-2 text-[12px] text-attention"
            >
              <span className="min-w-0 flex-1">{banner}</span>
              <button
                type="button"
                onClick={clearBanner}
                aria-label="Dismiss"
                className="cursor-pointer text-fg-subtle hover:text-fg"
              >
                <span aria-hidden="true">×</span>
              </button>
            </div>
          )}
          <Screen
            route={route}
            runId={runId}
            conversation={conversation}
            onNew={() => void newSession()}
            onClose={setClosing}
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
          onSubmit={(text) => conversation.send({ op: 'submit', text })}
        />
      )}
      {explainProjects && (
        <Modal
          title="Projects arrive in a later phase"
          onClose={() => setExplainProjects(false)}
          cancelLabel="Close"
          width={480}
        >
          A project is a workspace directory with repositories under it, and a run works in an
          isolated worktree of one of them. None of that exists yet: this daemon works in the one
          directory it was started in, and every conversation shares it.
        </Modal>
      )}
      {closing && (
        <Modal
          title="Close this session?"
          onClose={() => setClosing('')}
          confirm={{
            label: 'Close session',
            danger: true,
            onClick: () => void closeSession(closing),
          }}
        >
          The conversation is saved and its session ends. The transcript stays readable and nothing
          it wrote to disk is undone — but it cannot be continued.
        </Modal>
      )}
      <ToastHost />
    </div>
  )
}

type Conversation = ReturnType<typeof useRunEvents>

function Screen({
  route,
  runId,
  conversation,
  onNew,
  onClose,
}: {
  route: Route
  runId: string
  conversation: Conversation
  onNew: () => void
  onClose: (id: string) => void
}) {
  switch (route.screen) {
    case 'chat':
      return (
        <Chat
          run={conversation.view}
          runId={runId}
          state={conversation.state}
          send={conversation.send}
          onNew={onNew}
          onClose={onClose}
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
      return <Models />
    case 'skills':
      return <Skills />
    case 'activity':
      return <Activity />
    case 'tickets':
      return <Tickets />
    case 'task':
      return <Task />
    case 'repos':
      return <Repos />
  }
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
}

function crumbs(route: Route): { label: string; route?: Route }[] {
  const trail: { label: string; route?: Route }[] = [
    { label: 'local', route: { screen: 'chat' } },
    { label: TITLES[route.screen], route: { screen: route.screen } },
  ]
  if (route.id) trail.push({ label: route.id })
  return trail
}
