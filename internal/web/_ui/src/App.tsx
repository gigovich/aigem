import { useEffect, useState } from 'react'
import { signIn } from '@/lib/auth'

type Meta = { version: string; ui: boolean }

/**
 * The build spine, and nothing more: it proves the bundle is embedded, served,
 * signed in and talking to the daemon. The application shell replaces this in
 * the next phase.
 *
 * It asks /api/meta rather than /healthz on purpose: /healthz needs no
 * credential, so a spine built on it would go green with the sign-in broken.
 */
export default function App() {
  const [status, setStatus] = useState('connecting…')

  useEffect(() => {
    const abort = new AbortController()
    signIn(abort.signal)
      .then(() => fetch('/api/meta', { signal: abort.signal }))
      .then((res) => (res.ok ? (res.json() as Promise<Meta>) : Promise.reject(new Error(res.statusText))))
      .then((body) => setStatus(`daemon reachable: ${body.version}`))
      .catch((err: Error) => {
        if (err.name === 'AbortError') return
        // The sign-in says why on its own; anything else got as far as the
        // daemon and failed there.
        setStatus(err.message.startsWith('sign-in') ? err.message : `daemon unreachable: ${err.message}`)
      })
    return () => abort.abort()
  }, [])

  return (
    <main className="flex h-full flex-col items-center justify-center gap-3">
      <h1 className="m-0 text-base font-semibold tracking-tight">Aigem</h1>
      {/* The text changes after mount, so a screen reader is only told about it
          if the region announces itself. */}
      <p role="status" aria-live="polite" className="m-0 font-mono text-meta text-fg-subtle">
        {status}
      </p>
    </main>
  )
}
