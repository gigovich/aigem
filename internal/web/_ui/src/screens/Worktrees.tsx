import { useEffect, useState } from 'react'
import { api } from '@/lib/api'
import type { Repository } from '@/lib/wire'
import { currentProject, explain, setBanner, useApp } from '@/state/app'
import { DataGrid } from '@/ui/DataGrid'
import type { Column } from '@/ui/DataGrid'
import { EmptyState } from '@/ui/EmptyState'

/**
 * A project's repositories, with the branch a run will merge into. The
 * worktrees themselves arrive with runs on tickets; until then every row says
 * so.
 */
export function Worktrees() {
  const { project, name } = useApp((s) => ({ project: s.project, name: currentProject(s)?.name ?? '' }))
  const [repos, setRepos] = useState<{ project: string; items: Repository[] } | null>(null)

  useEffect(() => {
    if (!project) return
    const abort = new AbortController()
    void api
      .projectRepos(project, abort.signal)
      .then((items) => setRepos({ project, items }))
      .catch((err: unknown) => {
        if (!abort.signal.aborted) setBanner(explain(err))
      })
    return () => abort.abort()
  }, [project])

  const columns: Column<Repository>[] = [
    { key: 'name', header: 'Repository', width: '200px', cell: (r) => r.name || `${name} (the project itself)` },
    { key: 'main', header: 'Main branch', width: '140px', cell: (r) => r.main ?? 'neither main nor master' },
    { key: 'dir', header: 'Directory', width: 'minmax(200px, 1fr)', cell: (r) => r.dir },
    { key: 'worktrees', header: 'Worktrees', width: '140px', cell: () => 'no worktrees yet' },
  ]

  const loaded = repos?.project === project ? repos.items : null
  return (
    <>
      <div className="flex-none border-b border-line px-[18px] pt-[14px] pb-3">
        <h1 className="m-0 text-[16px] font-semibold tracking-[-0.015em]">Repositories & worktrees</h1>
      </div>
      {!project ? (
        <EmptyState
          title="This daemon's directory has no project record."
          detail="Choose or add a project in the sidebar to list its repositories. Worktrees arrive with runs on tickets."
        />
      ) : loaded === null ? (
        <p className="px-[18px] py-4 text-[12px] text-fg-subtle">Reading the repositories…</p>
      ) : (
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
      )}
    </>
  )
}
