import { useEffect, useState } from 'react'
import { api } from '@/lib/api'
import { explain } from '@/state/app'
import { Modal } from '@/ui/Modal'

/**
 * The whole of a tool result the timeline only kept the head of.
 *
 * The route answers `text/plain` with the tool's own output and nothing around
 * it, so this renders it as text - never as markup. It is the output of a
 * command the agent ran against files somebody else may have written.
 */
export function BlobDialog({
  runId,
  seq,
  onClose,
}: {
  runId: string
  seq: number
  onClose: () => void
}) {
  const [body, setBody] = useState<string | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    const abort = new AbortController()
    void api
      .runBlob(runId, seq, abort.signal)
      .then(setBody)
      .catch((err: unknown) => {
        if (abort.signal.aborted) return
        setError(explain(err))
      })
    return () => abort.abort()
  }, [runId, seq])

  return (
    <Modal title="Tool output" subtitle={`event ${seq}`} onClose={onClose} cancelLabel="Close" width={760}>
      {error && <p className="m-0 text-danger">{error}</p>}
      {body === null && !error && <p className="m-0">Reading it…</p>}
      {body !== null && (
        <pre className="m-0 max-h-[60vh] overflow-auto rounded-md border border-line bg-bg p-3 font-mono text-[11.5px] whitespace-pre-wrap">
          {body}
        </pre>
      )}
    </Modal>
  )
}
