import { useEffect, useState } from 'react'
import { api } from '@/lib/api'
import type { Model, NewModels, Providers } from '@/lib/wire'
import { explain, flash, refresh, setLogin, useApp } from '@/state/app'
import { Modal } from '@/ui/Modal'

const BUTTON =
  'h-7 flex-none whitespace-nowrap rounded-md border border-line px-3 text-[0.78125rem] text-fg-muted ' +
  'enabled:hover:bg-s0 disabled:opacity-50'
const PRIMARY =
  'h-7 rounded-md border border-primary bg-primary px-3 text-[0.78125rem] font-medium text-bg ' +
  'enabled:hover:brightness-110 disabled:opacity-50'
const INPUT =
  'h-7 min-w-0 flex-1 rounded-md border border-line bg-bg px-2 font-mono text-[0.8125rem] text-fg outline-none ' +
  'focus:border-primary'
const ROW = 'flex h-7 w-full items-center gap-2 rounded-[0.3125rem] px-2 text-left hover:bg-s0'

/** Where the candidates come from: a configured provider, or a server by URL. */
type Source = { provider: string } | { baseUrl: string }

const label = (s: Source) => ('provider' in s ? s.provider : s.baseUrl)

/**
 * Add models: pick a provider or a model server on this machine, then the
 * models it serves.
 */
export function ModelForm({ onDone }: { onDone?: () => void }) {
  const [providers, setProviders] = useState<Providers | null>(null)
  const [source, setSource] = useState<Source | null>(null)
  const [candidates, setCandidates] = useState<Model[] | null>(null)
  const [checked, setChecked] = useState<Set<string>>(() => new Set())
  const [url, setUrl] = useState('')
  const [filter, setFilter] = useState('')
  const [ctx, setCtx] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  // Read again when a sign-in dialog closes: that is what makes a provider usable here.
  const signingIn = useApp((s) => s.login !== '')

  useEffect(() => {
    if (signingIn) return
    const abort = new AbortController()
    api.providers(abort.signal).then(setProviders, (err) => {
      if (!abort.signal.aborted) setError(explain(err))
    })
    return () => abort.abort()
  }, [signingIn])

  const choose = async (from: Source, listed?: Model[]) => {
    setError('')
    setSource(from)
    setCandidates(listed ?? null)
    setChecked(new Set())
    setFilter('')
    if (listed) return
    try {
      setCandidates(await api.providerModels('provider' in from ? from : { url: from.baseUrl }))
    } catch (err) {
      setError(explain(err))
      setSource(null)
    }
  }

  const picked = (candidates ?? []).filter((m) => checked.has(m.ref))
  const needsCtx = picked.some((m) => !m.contextWindow)

  const add = async () => {
    if (!source || picked.length === 0 || busy) return
    const fallback = Number(ctx)
    if (needsCtx && !(fallback > 0)) {
      setError('Give the context window for the models that do not report one.')
      return
    }
    setBusy(true)
    setError('')
    const models = picked.map((m) => (m.contextWindow ? m : { ...m, contextWindow: fallback }))
    const req: NewModels = { ...source, models }
    try {
      await api.addModels(req)
      await refresh.models()
      flash(`Added ${models.length} model${models.length > 1 ? 's' : ''}`)
      onDone?.()
    } catch (err) {
      setError(explain(err))
      setBusy(false)
    }
  }

  const alert = error && (
    <p role="alert" className="m-0 text-[0.78125rem] text-attention">
      {error}
    </p>
  )

  if (!source) {
    return (
      <div className="flex flex-col gap-3 text-[0.8125rem]">
        {!providers && !error && <p className="m-0 text-fg-subtle">Looking for providers and local servers…</p>}
        {providers && (
          <ul aria-label="Providers" className="m-0 flex list-none flex-col gap-0.5 p-0">
            {providers.providers.map((p) => (
              <li key={p.id} className="flex items-center gap-2">
                <button
                  type="button"
                  disabled={p.needsAuth && !p.authenticated}
                  onClick={() => void choose({ provider: p.id })}
                  className={`${ROW} disabled:opacity-50`}
                >
                  <span className="flex-1 font-mono">{p.id}</span>
                </button>
                {p.needsAuth && !p.authenticated &&
                  (p.canLogin ? (
                    <button type="button" className={BUTTON} onClick={() => setLogin(p.id)}>
                      Sign in
                    </button>
                  ) : (
                    <span className="flex-none font-mono text-[0.71875rem] text-fg-subtle">
                      aigem auth login {p.id}
                    </span>
                  ))}
              </li>
            ))}
            {providers.local.map((s) => (
              <li key={s.baseUrl}>
                <button type="button" onClick={() => void choose({ baseUrl: s.baseUrl }, s.models)} className={ROW}>
                  <span className="flex-1 font-mono">{s.name}</span>
                  <span className="font-mono text-[0.71875rem] text-fg-subtle">{s.baseUrl}</span>
                </button>
              </li>
            ))}
          </ul>
        )}
        <form
          className="flex gap-2"
          onSubmit={(e) => {
            e.preventDefault()
            if (url.trim()) void choose({ baseUrl: url.trim() })
          }}
        >
          <input
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="http://127.0.0.1:8000"
            aria-label="Server URL"
            className={INPUT}
          />
          <button type="submit" disabled={!url.trim()} className={BUTTON}>
            List models
          </button>
        </form>
        {alert}
      </div>
    )
  }

  const needle = filter.trim().toLowerCase()
  const shown = (candidates ?? []).filter((m) => m.ref.toLowerCase().includes(needle))
  return (
    <div className="flex flex-col gap-3 text-[0.8125rem]">
      <div className="flex items-center gap-2">
        <span className="min-w-0 flex-1 truncate font-mono text-fg-subtle">{label(source)}</span>
        <input
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          placeholder="Filter…"
          aria-label="Filter models"
          className={INPUT}
        />
      </div>
      {!candidates ? (
        <p className="m-0 text-fg-subtle">Asking for its models…</p>
      ) : candidates.length === 0 ? (
        <p className="m-0 text-fg-subtle">Nothing new to add here.</p>
      ) : (
        <ul aria-label="Models" className="m-0 flex max-h-72 list-none flex-col overflow-auto p-0">
          {shown.map((m) => (
            <li key={m.ref}>
              <label className={ROW}>
                <input
                  type="checkbox"
                  checked={checked.has(m.ref)}
                  onChange={() =>
                    setChecked((c) => {
                      const next = new Set(c)
                      if (!next.delete(m.ref)) next.add(m.ref)
                      return next
                    })
                  }
                />
                <span className="min-w-0 flex-1 truncate font-mono">{m.name || m.ref}</span>
                <span className="font-mono text-[0.71875rem] text-fg-subtle">
                  {m.contextWindow ? `${Math.round(m.contextWindow / 1000)}k` : 'context ?'}
                </span>
              </label>
            </li>
          ))}
        </ul>
      )}
      {needsCtx && (
        <label className="flex items-center gap-2 text-[0.78125rem] text-fg-subtle">
          Context window for models that do not report one
          <input
            value={ctx}
            onChange={(e) => setCtx(e.target.value.replace(/\D/g, ''))}
            inputMode="numeric"
            placeholder="32768"
            className={INPUT}
          />
        </label>
      )}
      {alert}
      <div className="flex justify-end gap-2">
        <button type="button" className={BUTTON} onClick={() => setSource(null)}>
          Back
        </button>
        <button type="button" className={PRIMARY} disabled={picked.length === 0 || busy} onClick={() => void add()}>
          {busy ? 'Adding…' : `Add ${picked.length || ''} model${picked.length === 1 ? '' : 's'}`}
        </button>
      </div>
    </div>
  )
}

export function AddModelDialog({ onClose }: { onClose: () => void }) {
  return (
    <Modal title="Add models" onClose={onClose} width={520}>
      <ModelForm onDone={onClose} />
    </Modal>
  )
}

/** What a daemon with no models of the person's own shows in place of every screen. */
export function FirstModel() {
  return (
    <div className="mx-auto flex w-full max-w-[520px] flex-col gap-3 overflow-auto px-4 py-10">
      <h2 className="m-0 text-[0.9375rem] font-semibold">Add your first model</h2>
      <p className="m-0 text-[0.8125rem] text-fg-subtle">
        Pick a provider or a model server on this machine, then the models to use.
      </p>
      <ModelForm />
    </div>
  )
}
