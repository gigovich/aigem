import { useEffect, useState } from 'react'

type Health = { ok: boolean; ui: boolean }

/**
 * The build spine, and nothing more: it proves the bundle is embedded, served
 * and talking to the daemon. The application shell replaces this in the next
 * phase.
 */
export default function App() {
  const [health, setHealth] = useState<Health | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let live = true
    fetch('/healthz')
      .then((res) => (res.ok ? res.json() : Promise.reject(new Error(res.statusText))))
      .then((body: Health) => live && setHealth(body))
      .catch((err: Error) => live && setError(err.message))
    return () => {
      live = false
    }
  }, [])

  return (
    <main className="flex h-full flex-col items-center justify-center gap-3 bg-bg font-sans text-fg">
      <h1 className="m-0 text-base font-semibold tracking-tight">Aigem</h1>
      <p className="m-0 font-mono text-xs text-fg-subtle">
        {error ? `daemon unreachable: ${error}` : health ? 'daemon reachable' : 'connecting…'}
      </p>
    </main>
  )
}
