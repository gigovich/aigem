import { useEffect, useState } from 'react'
import { api } from '@/lib/api'
import { navigate } from '@/lib/route'
import type { Repository, Worktree } from '@/lib/wire'
import { currentProject, explain, flash, setBanner, useApp } from '@/state/app'
import { DataGrid } from '@/ui/DataGrid'
import type { Column } from '@/ui/DataGrid'
import { EmptyState } from '@/ui/EmptyState'
import { Modal } from '@/ui/Modal'

const BUTTON =
  'h-6 rounded-md border border-line px-2 text-[0.75rem] text-fg-muted enabled:hover:border-line-strong enabled:hover:text-fg disabled:opacity-50'

function count(n: number): string {
  return n === 0 ? 'no worktrees' : `${n} worktree${n === 1 ? '' : 's'}`
}

/**
 * A project's repositories, with the branch a run merges into, and the `aigem/*` branches
 * ticket runs left in them.
 */
export function Worktrees() {
  const { project, name, tickets, served } = useApp((s) => ({
    project: s.project,
    name: currentProject(s)?.name ?? '',
    tickets: s.tickets,
    served: s.meta?.features?.tickets === true,
  }))
  const [repos, setRepos] = useState<{ project: string; items: Repository[] } | null>(null)
  const [trees, setTrees] = useState<{ project: string; items: Worktree[] } | null>(null)
  const [discarding, setDiscarding] = useState<Worktree | null>(null)
  const [reload, setReload] = useState(0)

  useEffect(() => {
    if (!project) return
    const abort = new AbortController()
    void api
      .projectRepos(project, abort.signal)
      .then((items) => setRepos({ project, items }))
      .catch((err: unknown) => {
        if (abort.signal.aborted) return
        setBanner(explain(err))
        // An answer, so the screen stops saying it is still reading.
        setRepos({ project, items: [] })
      })
    return () => abort.abort()
  }, [project])

  // Read again when the project's tickets change: a run starting or finishing is what moves
  // a worktree.
  useEffect(() => {
    if (!project || !served) return
    const abort = new AbortController()
    void api
      .worktrees(project, abort.signal)
      .then((items) => setTrees({ project, items }))
      .catch((err: unknown) => {
        if (!abort.signal.aborted) setBanner(explain(err))
      })
    return () => abort.abort()
  }, [project, served, tickets, reload])

  const shown = trees?.project === project ? trees.items : []
  const discard = async (w: Worktree) => {
    setDiscarding(null)
    try {
      await api.discardWorktree(project, w.name)
      flash(`Discarded aigem/${w.name}`)
      setReload((n) => n + 1)
    } catch (err) {
      setBanner(explain(err))
    }
  }

  const columns: Column<Repository>[] = [
    { key: 'name', header: 'Repository', width: '12.5rem', cell: (r) => r.name || `${name} (the project itself)` },
    { key: 'main', header: 'Main branch', width: '8.75rem', cell: (r) => r.main || 'neither main nor master' },
    { key: 'dir', header: 'Directory', width: 'minmax(12.5rem, 1fr)', cell: (r) => r.dir },
    {
      key: 'worktrees',
      header: 'Worktrees',
      width: '8.75rem',
      cell: (r) => count(shown.filter((w) => w.repo === r.name).length),
    },
  ]
  const treeColumns: Column<Worktree>[] = [
    { key: 'repo', header: 'Repository', width: '10rem', cell: (w) => w.repo || name },
    { key: 'branch', header: 'Branch', width: '9rem', cell: (w) => `aigem/${w.name}` },
    { key: 'state', header: 'State', width: '5.5rem', cell: (w) => w.state },
    { key: 'path', header: 'Worktree', width: 'minmax(12.5rem, 1fr)', cell: (w) => w.path || 'removed' },
    {
      key: 'actions',
      header: 'Actions',
      width: '17rem',
      cell: (w) => (
        <span className="flex gap-1.5">
          {w.run && (
            <button
              type="button"
              aria-label={`Open run ${w.run}`}
              onClick={() => navigate({ screen: 'run', id: w.run })}
              className={BUTTON}
            >
              Open run
            </button>
          )}
          {w.ticket && (
            <button
              type="button"
              aria-label={`Open ticket ${w.ticket}`}
              onClick={() => navigate({ screen: 'task', id: w.ticket })}
              className={BUTTON}
            >
              Open ticket
            </button>
          )}
          <button
            type="button"
            aria-label={`Discard aigem/${w.name}`}
            disabled={w.state === 'running'}
            onClick={() => setDiscarding(w)}
            className={BUTTON}
          >
            Discard
          </button>
        </span>
      ),
    },
  ]

  const loaded = repos?.project === project ? repos.items : null
  return (
    <>
      <div className="flex-none border-b border-line px-4.5 pt-3.5 pb-3">
        <h1 className="m-0 text-[1.0625rem] font-semibold tracking-[-0.015em]">Repositories & worktrees</h1>
      </div>
      {!project ? (
        <EmptyState
          title="This daemon's directory has no project record."
          detail="Choose or add a project in the sidebar to list its repositories and worktrees."
        />
      ) : loaded === null ? (
        <p className="px-4.5 py-4 text-[0.8125rem] text-fg-subtle">Reading the repositories…</p>
      ) : (
        <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
          <DataGrid
            label="Repositories"
            columns={columns}
            rows={loaded}
            rowKey={(r) => r.dir}
            minWidth={680}
            empty={
              <EmptyState
                title="No repositories."
                detail={`${name} holds no git checkout, at its root or one level down.`}
              />
            }
          />
          {served && (
            <>
              <h2 className="m-0 border-y border-line px-4.5 py-2 text-[0.6875rem] font-semibold tracking-[.07em] text-fg-subtle uppercase">
                Worktrees
              </h2>
              <DataGrid
                label="Worktrees"
                columns={treeColumns}
                rows={shown}
                rowKey={(w) => `${w.repo}/${w.name}`}
                minWidth={680}
                empty={
                  <EmptyState
                    title="No worktrees."
                    detail="Running a ticket makes one; its branch stays after the merge."
                  />
                }
              />
            </>
          )}
        </div>
      )}
      {discarding && (
        <Modal
          title={`Discard ${discarding.name}?`}
          subtitle={`aigem/${discarding.name}`}
          onClose={() => setDiscarding(null)}
          confirm={{ label: 'Discard', danger: true, onClick: () => void discard(discarding) }}
        >
          The worktree and the branch are deleted. Work that was not merged into main is lost.
        </Modal>
      )}
    </>
  )
}
