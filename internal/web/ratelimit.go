package web

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// What stands between this daemon and an offline guess at its token is the
// token's own 256 bits. What stands between it and an *online* guess is this
// file: a token bucket per client address, spent only by failures, so a
// reconnect loop or a page full of parallel requests costs nothing and a
// password-guesser costs itself a minute per ten tries.
//
// And a second, unrelated limit: how many websockets may be open at once. A
// client whose socket keeps failing reconnects with backoff, but a client whose
// socket keeps *succeeding* and then dying - a proxy with a short idle timeout,
// a phone waking and sleeping - does not, and every one of those attaches a
// subscriber and two goroutines to a session that never notices.

const (
	// authFailureBurst is how many failures an address may make back to back.
	// It is not one: a page reloaded against a daemon that has restarted fails
	// every in-flight request at once with a token that was valid a moment ago,
	// and locking the operator out for that would be a limiter defending the
	// daemon against its own user.
	authFailureBurst = 10
	// authFailureWindow is how long the full burst takes to come back, one
	// token at a time.
	authFailureWindow = time.Minute

	// maxSockets is how many websockets this daemon holds at once. It is a
	// backstop, not a capacity plan: one browser holds one, plus one per open
	// agent timeline, and a number in the tens means something is looping.
	maxSockets = 16

	// maxTrackedAddresses bounds the failure table. Every entry is an address
	// that has failed recently; the table is swept when it grows past this,
	// dropping the ones that have paid off their failures.
	maxTrackedAddresses = 1024
)

// limiter refuses an address that has been failing authentication.
type limiter struct {
	burst  float64
	window time.Duration
	now    func() time.Time

	mu    sync.Mutex
	spent map[string]*debt
}

// debt is what one address has left: how many failures it may still make, and
// when that was last brought up to date. Tokens are recomputed on read rather
// than refilled on a timer, so an idle address costs nothing but a map entry.
type debt struct {
	tokens float64
	at     time.Time
}

func newLimiter() *limiter {
	return &limiter{
		burst:  authFailureBurst,
		window: authFailureWindow,
		now:    time.Now,
		spent:  map[string]*debt{},
	}
}

// blocked reports whether this address has spent its whole burst, and how long
// until it gets a token back.
func (l *limiter) blocked(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, ok := l.spent[key]
	if !ok {
		return 0, false
	}
	l.settle(d, l.now())
	if d.tokens >= 1 {
		return 0, false
	}
	// The wait is until the next whole token, not until the burst is back: one
	// token is all it takes to try again.
	return time.Duration((1 - d.tokens) * float64(l.perToken())), true
}

// fail records one refused authentication.
func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	d, ok := l.spent[key]
	if !ok {
		if len(l.spent) >= maxTrackedAddresses {
			l.sweep(now)
		}
		d = &debt{tokens: l.burst, at: now}
		l.spent[key] = d
	}
	l.settle(d, now)
	if d.tokens >= 1 {
		d.tokens--
	}
}

// clear forgives an address. A successful authentication is proof this is not
// the caller the limiter is for, and leaving the count standing would mean a
// browser that reconnected through one stale token stayed one failure from a
// lockout for the next minute.
func (l *limiter) clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.spent, key)
}

// settle refills a bucket up to now. Caller holds l.mu.
func (l *limiter) settle(d *debt, now time.Time) {
	elapsed := now.Sub(d.at)
	if elapsed <= 0 {
		return
	}
	d.tokens += float64(elapsed) / float64(l.perToken())
	if d.tokens > l.burst {
		d.tokens = l.burst
	}
	d.at = now
}

func (l *limiter) perToken() time.Duration {
	return time.Duration(float64(l.window) / l.burst)
}

// sweep drops every address whose burst has come all the way back: it is
// indistinguishable from one the limiter has never seen. Caller holds l.mu.
func (l *limiter) sweep(now time.Time) {
	for key, d := range l.spent {
		l.settle(d, now)
		if d.tokens >= l.burst {
			delete(l.spent, key)
		}
	}
}

// clientAddr is the key the limiter counts by.
//
// It is the peer address and nothing else. X-Forwarded-For is written by
// whoever is talking to us, so honouring it would let one client claim a fresh
// bucket per request - the exact opposite of a limit. Behind a reverse proxy
// that means the whole world shares one bucket, and ten wrong tokens a minute
// from anywhere pauses everyone; with one operator and a proxy that is already
// authenticating, that trade is the right way round.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// sockets counts open websockets against maxSockets.
type sockets struct {
	mu   sync.Mutex
	open int
}

func (s *sockets) acquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open >= maxSockets {
		return false
	}
	s.open++
	return true
}

func (s *sockets) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open > 0 {
		s.open--
	}
}

// isUpgrade reports whether this request is a websocket handshake. Both headers
// are lists, and the tokens in them are case-insensitive.
func isUpgrade(r *http.Request) bool {
	return httpTokenIn(r.Header.Values("Connection"), "upgrade") &&
		httpTokenIn(r.Header.Values("Upgrade"), "websocket")
}

func httpTokenIn(values []string, want string) bool {
	for _, v := range values {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
}
