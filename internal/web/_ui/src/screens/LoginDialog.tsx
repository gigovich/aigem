import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '@/lib/api'
import { safeHref } from '@/lib/text'
import { explain, flash, refresh } from '@/state/app'
import type { Login } from '@/lib/wire'
import { Modal } from '@/ui/Modal'

/**
 * A callback whose identity never changes, calling whatever the latest one is.
 *
 * It is the "latest ref" pattern: an interval that depends on a prop the caller
 * writes inline restarts on every render of the parent, which for a poll means
 * it never fires.
 */
function useEvent(fn: () => void): () => void {
  const latest = useRef(fn)
  useEffect(() => {
    latest.current = fn
  }, [fn])
  return useCallback(() => latest.current(), [])
}

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
  // The caller usually passes an inline arrow, which would re-create the poll
  // below - and reset its clock - on every unrelated re-render of the screen.
  const close = useEvent(onClose)
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
        if (live) setError(explain(err))
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
    // A poll that only reports its failures runs for as long as the dialog is
    // open against a flow that may have gone. Past a handful of consecutive
    // failures it is the daemon that is not answering, not the person who is
    // slow, and there is nothing left to wait for.
    let failures = 0
    const timer = setInterval(() => {
      void api
        .login(id.current)
        .then((l) => {
          failures = 0
          setLogin(l)
          if (l.state === 'done') {
            id.current = ''
            flash(`Signed in to ${l.provider}`)
            void refresh.models()
            void refresh.usage()
            close()
          }
        })
        .catch((err: unknown) => {
          setError(explain(err))
          if (++failures >= 5) {
            clearInterval(timer)
            setLogin((l) => (l ? { ...l, state: 'failed' } : l))
          }
        })
    }, 1500)
    return () => clearInterval(timer)
  }, [state, close])

  const paste = () => {
    if (!id.current || !pasted.trim()) return
    void api
      .pasteLogin(id.current, pasted.trim())
      .then(setLogin)
      .catch((err: unknown) => setError(explain(err)))
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
            {/* Through the same allow-list the markdown renderer uses. The
                daemon pins this URL to the provider's own discovery document,
                but "the one URL on the page that skips the check" is not a
                property worth having. */}
            {safeHref(login.url) ? (
              <a href={safeHref(login.url) ?? ''} target="_blank" rel="noreferrer noopener">
                {login.url}
              </a>
            ) : (
              login.url
            )}
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
