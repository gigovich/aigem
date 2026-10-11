import { useState } from 'react'
import { api } from '@/lib/api'
import { explain, flash, reloadProjects, selectProject } from '@/state/app'
import { DirTree } from '@/ui/DirTree'
import { Modal } from '@/ui/Modal'

const SUBMIT =
  'h-7 self-end rounded-md border border-primary bg-primary px-3 text-[0.78125rem] font-medium text-bg ' +
  'enabled:hover:brightness-110 disabled:opacity-50'

const INPUT =
  'h-7 rounded-md border border-line bg-bg px-2 font-mono text-[0.8125rem] text-fg outline-none focus:border-primary'

/**
 * Add a project: a directory picked from under the daemon's root, and
 * optionally a name.
 *
 * The daemon's refusal is shown inside the form as text - it names the path
 * that was picked and why it would not do - rather than as a banner behind it.
 */
export function ProjectForm({ onDone }: { onDone?: () => void }) {
  const [dir, setDir] = useState<string | null>(null)
  const [name, setName] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const add = async () => {
    if (dir === null || busy) return
    setBusy(true)
    setError('')
    try {
      const trimmed = name.trim()
      const path = dir || '.'
      const project = await api.addProject(trimmed ? { dir: path, name: trimmed } : { dir: path })
      await reloadProjects()
      selectProject(project.id)
      flash(`Added ${project.name}`)
      onDone?.()
    } catch (err) {
      setError(explain(err))
      setBusy(false)
    }
  }

  return (
    <form
      className="flex flex-col gap-3"
      onSubmit={(e) => {
        e.preventDefault()
        void add()
      }}
    >
      <div className="flex flex-col gap-1 text-[0.78125rem] text-fg-subtle">
        Directory
        <DirTree selected={dir} onSelect={setDir} />
      </div>
      <label className="flex flex-col gap-1 text-[0.78125rem] text-fg-subtle">
        Name, if not the directory's
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder={dir ? dir.split('/').pop() : undefined}
          className={INPUT}
        />
      </label>
      {error && (
        <p role="alert" className="m-0 text-[0.78125rem] text-attention">
          {error}
        </p>
      )}
      <button
        type="submit"
        disabled={dir === null || busy}
        className={SUBMIT}
      >
        {busy ? 'Adding…' : 'Add project'}
      </button>
    </form>
  )
}

export function NewProjectDialog({ onClose }: { onClose: () => void }) {
  return (
    <Modal title="New project" onClose={onClose} width={480}>
      <ProjectForm onDone={onClose} />
    </Modal>
  )
}

/** What a daemon with no projects shows in place of every screen. */
export function FirstProject() {
  return (
    <div className="mx-auto flex w-full max-w-[480px] flex-col gap-3 overflow-auto px-4 py-10">
      <h2 className="m-0 text-[0.9375rem] font-semibold">Add your first project</h2>
      <p className="m-0 text-[0.8125rem] text-fg-subtle">
        A project is a directory under the daemon's root. Pick one.
      </p>
      <ProjectForm />
    </div>
  )
}
