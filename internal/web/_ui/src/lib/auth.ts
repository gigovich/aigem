/**
 * Trade the token in the address bar for a cookie.
 *
 * The daemon prints a link with `?token=`. That token is a bearer credential,
 * so the page spends it once and takes it back out of the URL: left there it
 * would sit in the browser history, in the Referer of anything the page links
 * to, and in every screenshot of the address bar.
 *
 * A page that already holds a cookie arrives with no token and still runs this.
 * The exchange renews a cookie that is close to expiry, and answers without
 * issuing one when it is still fresh - which is why it is safe on every load.
 */
export async function signIn(signal?: AbortSignal): Promise<void> {
  const url = new URL(window.location.href)
  const token = url.searchParams.get('token')
  const res = await fetch('/api/auth/session', {
    method: 'POST',
    signal,
    headers: token ? { Authorization: `Bearer ${token}` } : undefined,
  })
  if (!res.ok) throw new Error(`sign-in refused: ${res.status}`)
  // Only after it was spent. A refused exchange leaves the token where it is,
  // because it is the only credential the page has and a reload is the whole
  // retry story.
  if (token) {
    url.searchParams.delete('token')
    window.history.replaceState(null, '', `${url.pathname}${url.search}${url.hash}`)
  }
}
