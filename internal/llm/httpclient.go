package llm

import (
	"net/http"
	"sync"
	"time"
)

// callTimeout bounds a single model call. It is generous because a long
// reasoning turn legitimately streams for minutes.
const callTimeout = 10 * time.Minute

// sharedClient is the HTTP client every backend in the process uses.
//
// One process can hold a backend per bot, and a client per backend means a
// connection pool per backend: each bot pays its own TLS handshakes and keeps
// its own idle connections to the same provider host. Sharing one client shares
// the pool, which is what makes several bots against one provider cheaper than
// several processes were. http.Client is safe for concurrent use, and the
// per-request bearer token is set on the request, never on the client.
var (
	sharedClientOnce sync.Once
	sharedClientVal  *http.Client
)

// sharedHTTPClient returns the process-wide HTTP client for provider calls.
func sharedHTTPClient() *http.Client {
	sharedClientOnce.Do(func() {
		tr, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			sharedClientVal = &http.Client{Timeout: callTimeout}
			return
		}
		trc := tr.Clone()
		// The defaults assume a handful of hosts; here almost every connection goes
		// to one provider, so the per-host idle pool is what decides whether a call
		// reuses a connection or pays a fresh handshake.
		trc.MaxIdleConns = 64
		trc.MaxIdleConnsPerHost = 16
		trc.IdleConnTimeout = 90 * time.Second
		sharedClientVal = &http.Client{Timeout: callTimeout, Transport: trc}
	})
	return sharedClientVal
}
