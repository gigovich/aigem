import { useState } from 'react'
import { api } from '@/lib/api'
import { explain, flash, refresh, selectProject } from '@/state/app'
import { Modal } from '@/ui/Modal'

const INPUT =
  'h-[28px] rounded-md border border-line bg-bg px-2 font-mono text-[12px] text-fg outline-none focus:border-primary'

/**
 * Add a project: a directory on the daemon's machine, and optionally a name.
 *
 * The daemon's refusal is shown inside the dialog as text - it names the path
 * that was typed and why it would not do - rather than as a banner behind it.
 */
export function NewProjectDialog({ onClose }: { onClose: () => void }) {
  const [dir, setDir] = useState('')
  const [name, setName] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const ready = dir.trim() !== '' && !busy

  const add = async () => {
    if (!ready) return
    setBusy(true)
    setError('')
    try {
      const trimmed = name.trim()
      const project = await api.addProject(trimmed ? { dir: dir.trim(), name: trimmed } : { dir: dir.trim() })
      await refresh.projects()
      selectProject(project.id)
      flash(`Added ${project.name}`)
      onClose()
    } catch (err) {
      setError(explain(err))
      setBusy(false)
    }
  }

  return (
    <Modal
      title="New project"
      onClose={onClose}
      width={480}
      confirm={{ label: busy ? 'Adding…' : 'Add project', onClick: () => void add(), disabled: !ready }}
    >
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          void add()
        }}
      >
        <label className="flex flex-col gap-1 text-[11.5px] text-fg-subtle">
          Directory on the daemon's machine
          <input
            value={dir}
            onChange={(e) => setDir(e.target.value)}
            placeholder="/home/you/work/thing"
            spellCheck={false}
            className={INPUT}
          />
        </label>
        <label className="flex flex-col gap-1 text-[11.5px] text-fg-subtle">
          Name, if not the directory's
          <input value={name} onChange={(e) => setName(e.target.value)} className={INPUT} />
        </label>
        {error && (
          <p role="alert" className="m-0 text-[11.5px] text-attention">
            {error}
          </p>
        )}
      </form>
    </Modal>
  )
}
