import { useEffect, useMemo, useState } from 'react'
import { api } from '@/lib/api'
import { diffLines, unified } from '@/lib/diff'
import { bytes as sizeOf } from '@/lib/format'
import { readable } from '@/lib/text'
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
export function Changes({ runId, live, seq }: { runId: string; live: boolean; seq: number }) {
  const [state, setState] = useState<{ runId: string; files: Artifact[] | null; error: string }>({
    runId,
    files: null,
    error: '',
  })
  // Read through the run it was read for, rather than reset when that changes:
  // run A's diffs must not paint under run B's header even for the frame before
  // the next answer arrives, and a comparison at the point of use cannot be a
  // frame late the way a state update can.
  const { files, error } = state.runId === runId ? state : { files: null, error: '' }

  useEffect(() => {
    // Artifacts live with the session and not in the journal, so a closed run
    // has nothing to read. Answered by the early return below rather than here,
    // where setting state would be a second render for a question that has a
    // static answer.
    if (!live) return
    const abort = new AbortController()
    void api
      .runArtifacts(runId, abort.signal)
      .then((got) => setState({ runId, files: got, error: '' }))
      .catch((err: unknown) => {
        if (abort.signal.aborted) return
        setState({ runId, files: [], error: explain(err) })
      })
    return () => abort.abort()
    // `seq` is in here so a conversation that goes on writing files is read
    // again: the daemon has no event for "the working tree moved", and a tab
    // left open on this view would otherwise show what was true when it opened.
  }, [runId, live, seq])

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
                <span className="break-all">{readable(f.path)}</span>
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
  // Memoised on the contents themselves: the run screen re-renders on every
  // event of the conversation, and the table this builds is the one thing on
  // the page whose cost is measured in hundreds of milliseconds.
  const diff = useMemo(() => diffLines(file.old ?? '', file.new ?? ''), [file.old, file.new])
  if (diff.kind === 'invisible') {
    return (
      <div className="rounded-md border border-line bg-bg p-3">
        <div className="font-mono text-[11px] break-all text-fg-muted">{readable(file.path)}</div>
        <p className="mt-2 mb-0 text-[11.5px] text-fg-subtle">
          Every line is unchanged; the line endings or the final newline are not.
        </p>
      </div>
    )
  }
  if (diff.kind === 'too-large') {
    return (
      <div className="rounded-md border border-line bg-bg p-3">
        <div className="font-mono text-[11px] break-all text-fg-muted">{readable(file.path)}</div>
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
