import { useEffect, useState } from 'react'
import { api } from '@/lib/api'
import { diffLines, unified } from '@/lib/diff'
import { bytes as sizeOf } from '@/lib/format'
import type { Artifact } from '@/lib/wire'
import { explain } from '@/state/app'
import { DiffView } from '@/ui/DiffView'
import { EmptyState } from '@/ui/EmptyState'

/**
 * What a run changed on disk.
 *
 * The daemon sends both sides of every file rather than a patch, so the diff is
 * computed here. Past its budget it sends neither and says how big they were,
 * which is why `truncated` is drawn as a statement about the change rather than
 * as an empty diff.
 */
export function Changes({ runId, live }: { runId: string; live: boolean }) {
  const [files, setFiles] = useState<Artifact[] | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    // Artifacts live with the session and not in the journal, so a closed run
    // has nothing to read. Answered by the early return below rather than here,
    // where setting state would be a second render for a question that has a
    // static answer.
    if (!live) return
    const abort = new AbortController()
    void api
      .runArtifacts(runId, abort.signal)
      .then(setFiles)
      .catch((err: unknown) => {
        if (abort.signal.aborted) return
        setError(explain(err))
      })
    return () => abort.abort()
  }, [runId, live])

  if (!live) {
    return (
      <EmptyState
        title="This conversation is closed."
        detail="The files it changed are still on disk; the daemon reads them through the session, which has ended."
      />
    )
  }
  if (error) return <EmptyState title="The changes could not be read." detail={error} />
  if (!files) return <EmptyState title="Reading what changed…" />
  if (files.length === 0) return <EmptyState title="This run has not changed any files." />

  return (
    <div className="flex-1 overflow-y-auto px-[18px] py-3">
      {files.map((f) => (
        <div key={f.path} className="mb-4 max-w-[110ch]">
          {f.truncated ? (
            <div className="rounded-md border border-line bg-bg p-3">
              <div className="flex items-center gap-2 font-mono text-[11px] text-fg-muted">
                <span aria-hidden="true" style={{ color: 'var(--modified)' }}>
                  ~
                </span>
                <span>{f.path}</span>
              </div>
              <p className="mt-2 mb-0 text-[11.5px] text-fg-subtle">
                {f.created ? 'Created' : 'Changed'}, {sizeOf(f.oldBytes ?? 0)} to{' '}
                {sizeOf(f.newBytes ?? 0)} — too large for the daemon to send both sides.
              </p>
            </div>
          ) : (
            <FileDiff file={f} />
          )}
        </div>
      ))}
    </div>
  )
}

function FileDiff({ file }: { file: Artifact }) {
  const diff = diffLines(file.old ?? '', file.new ?? '')
  if (diff.kind === 'too-large') {
    return (
      <div className="rounded-md border border-line bg-bg p-3">
        <div className="font-mono text-[11px] text-fg-muted">{file.path}</div>
        <p className="mt-2 mb-0 text-[11.5px] text-fg-subtle">
          {diff.oldLines} lines became {diff.newLines} — too long to diff in a browser.
        </p>
      </div>
    )
  }
  return (
    <>
      <DiffView
        path={file.path}
        change={file.created ? '+' : '~'}
        lines={unified(diff.lines)}
      />
      <div className="mt-1 flex gap-3 font-mono text-[10.5px]">
        <span style={{ color: 'var(--added)' }}>+{diff.added}</span>
        <span style={{ color: 'var(--deleted)' }}>−{diff.removed}</span>
      </div>
    </>
  )
}
