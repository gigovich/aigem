package web

import (
	"cmp"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// A token bucket per client address, spent only by failures, so that a caller
// which keeps failing stops getting work done on its behalf.
//
// It is deliberately not the thing that protects the token: that is the token's
// own 256 bits, compared in constant time, which no rate limit improves and no
// rate limit is needed for. So this runs *after* the credential is checked and
// never refuses a good one - see the note in guard() for the outage that
// ordering it the other way round caused.
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
	// backstop, not a capacity plan.
	//
	// Counted per connection, and an open tab holds two - the inbox stream and
	// the timeline of the thread being read - so this is sixteen tabs across
	// every device, plus whatever `aigem attach` is holding. Refusing one costs
	// the client a retry, which is cheap now that a refusal cannot also spend
	// its way into a rate limit.
	maxSockets = 32

	// maxTrackedAddresses is the hard ceiling on the failure table. Every entry
	// is an address that has failed recently, and an unauthenticated caller
	// decides how many there are, so it is a cap rather than a target: see
	// makeRoom.
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
			l.makeRoom(now)
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

// makeRoom guarantees space for one more address. Caller holds l.mu.
//
// It first drops every address whose burst has come all the way back, since one
// of those is indistinguishable from an address the limiter has never seen.
// When a flood from many addresses at once leaves nothing to collect, it evicts
// anyway: an entry that is merely not inserted is an address with a full burst,
// so declining to track new ones would hand a caller with addresses to spare a
// way never to be limited at all.
//
// It frees a quarter of the table rather than one slot, because this walk is
// O(table) under the lock every request takes - freeing one slot would mean
// walking the whole table per new address for as long as the flood lasts, which
// trades a memory bound for a worse CPU one.
func (l *limiter) makeRoom(now time.Time) {
	keep := make([]string, 0, len(l.spent))
	for key, d := range l.spent {
		l.settle(d, now)
		if d.tokens >= l.burst {
			delete(l.spent, key)
			continue
		}
		keep = append(keep, key)
	}
	room := maxTrackedAddresses - maxTrackedAddresses/4
	if len(l.spent) <= room {
		return
	}
	// The fullest go: they are the closest to being forgotten anyway, and the
	// emptiest are the ones actively failing, which is what this is for.
	slices.SortFunc(keep, func(a, b string) int {
		return cmp.Compare(l.spent[b].tokens, l.spent[a].tokens)
	})
	for _, key := range keep[:len(l.spent)-room] {
		delete(l.spent, key)
	}
}

// clientAddr is the key the limiter counts by.
//
// It is the peer address and nothing else. X-Forwarded-For is written by
// whoever is talking to us, so honouring it would let one client claim a fresh
// bucket per request - the exact opposite of a limit.
//
// Behind a reverse proxy every client is therefore one bucket. That is only
// survivable because a valid credential is checked first and never refused: an
// address at zero tokens still gets in with the right token, so a stranger
// filling the shared bucket costs the operator nothing.
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
