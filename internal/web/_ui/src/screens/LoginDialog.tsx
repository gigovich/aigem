import { useEffect, useRef, useState } from 'react'
import { api } from '@/lib/api'
import { describe, flash, refresh } from '@/state/app'
import type { Login } from '@/lib/wire'
import { Modal } from '@/ui/Modal'

/**
 * Signing a provider in, from the browser.
 *
 * The flow outlives the request that started it - it is a device code or a
 * redirect the person completes elsewhere - so this polls, and cancels the
 * daemon's flow when the dialog is closed. Leaving it running would hold the
 * single loopback port a ChatGPT sign-in needs and refuse the next attempt.
 *
 * Nothing here ever shows a token: the API does not carry one, and the only
 * values on screen are the authorization URL and the device code the person is
 * meant to read out.
 */
export function LoginDialog({ provider, onClose }: { provider: string; onClose: () => void }) {
  const [login, setLogin] = useState<Login | null>(null)
  const [error, setError] = useState('')
  const [pasted, setPasted] = useState('')
  const id = useRef('')

  useEffect(() => {
    let live = true
    const abort = new AbortController()
    void api
      .beginLogin(provider, abort.signal)
      .then((l) => {
        if (!live) return
        id.current = l.id
        setLogin(l)
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err))
      })
    return () => {
      live = false
      abort.abort()
      // The daemon owns the flow, not this request. A dialog closed halfway
      // through leaves a login holding a callback port until it times out.
      if (id.current) void api.cancelLogin(id.current).catch(() => undefined)
    }
  }, [provider])

  const state = login?.state
  useEffect(() => {
    if (state !== 'pending' || !id.current) return
    const timer = setInterval(() => {
      void api
        .login(id.current)
        .then((l) => {
          setLogin(l)
          if (l.state === 'done') {
            id.current = ''
            flash(`Signed in to ${l.provider}`)
            void refresh.models()
            void refresh.usage()
            onClose()
          }
        })
        .catch((err: unknown) => setError(describe(err)))
    }, 1500)
    return () => clearInterval(timer)
  }, [state, onClose])

  const paste = () => {
    if (!id.current || !pasted.trim()) return
    void api
      .pasteLogin(id.current, pasted.trim())
      .then(setLogin)
      .catch((err: unknown) => setError(describe(err)))
  }

  return (
    <Modal
      title={`Sign in to ${provider}`}
      subtitle={login?.state ?? 'starting'}
      onClose={onClose}
      cancelLabel="Close"
      width={520}
      confirm={
        login?.acceptsPaste
          ? { label: 'Submit', onClick: paste, disabled: !pasted.trim() }
          : undefined
      }
    >
      {error && <p className="m-0 mb-3 text-danger">{error}</p>}
      {login?.state === 'failed' && (
        <p className="m-0 mb-3 text-danger">{login.error || 'The provider refused the sign-in.'}</p>
      )}
      {login?.state === 'cancelled' && <p className="m-0 mb-3">This sign-in was cancelled.</p>}
      {login?.url && (
        <>
          <p className="m-0">Open this address and approve the request:</p>
          <p className="mt-2 font-mono text-[11.5px] break-all">
            <a href={login.url} target="_blank" rel="noreferrer noopener">
              {login.url}
            </a>
          </p>
        </>
      )}
      {login?.code && (
        <p className="mt-3 m-0">
          Then enter the code{' '}
          <span className="font-mono text-[13px] text-fg">{login.code}</span>.
        </p>
      )}
      {login?.acceptsPaste && (
        <label className="mt-4 block">
          <span className="mb-[5px] block text-[11px] text-fg-muted">
            Paste the address you were redirected to
          </span>
          <input
            value={pasted}
            onChange={(e) => setPasted(e.target.value)}
            className="h-[30px] w-full rounded-md border border-line bg-bg px-[10px] font-mono text-[11.5px] outline-none focus:border-primary"
          />
        </label>
      )}
      {!login && !error && <p className="m-0">Asking the provider to start a sign-in…</p>}
    </Modal>
  )
}
