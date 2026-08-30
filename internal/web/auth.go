package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Loopback keeps the network out. It does not keep browsers out: any page in
// any open tab can issue requests to 127.0.0.1, and DNS rebinding defeats the
// same-origin policy by resolving an attacker's hostname to the loopback
// address. Behind this endpoint will sit an agent with bash, filesystem writes
// and the credential store, so a token and an exact-match origin check are here
// before the first route worth attacking rather than after it.

// newToken returns a fresh URL-safe token.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// tokenOK compares in constant time, so a wrong guess tells an attacker nothing
// about how much of it was right.
func tokenOK(want, got string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// requestToken reads the token from the Authorization header, or from the query
// string.
//
// The query form exists because a browser cannot set headers on a websocket
// handshake, and because the page is opened from a link. It is not how a
// browser authenticates afterwards - the cookie is - but it is what the first
// request trades for one.
func requestToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
		return ""
	}
	return r.URL.Query().Get("token")
}

// originOK checks Origin and Host against what this daemon answers to. Both are
// matched exactly: a prefix or suffix test is how "127.0.0.1.example.com" gets
// in. A request with no Origin is not from a browser page - curl, a test - and
// is allowed on the strength of the token alone.
//
// The Origin is matched whole, scheme included. Comparing only the host would
// let a page served over plain HTTP from a name that also has an HTTPS
// deployment pass as that deployment, which is the entire value of the scheme
// being in the header.
//
// X-Forwarded-Proto and X-Forwarded-Host are deliberately not read. They are
// written by whoever is talking to us, and deriving the allowlist from the
// request it is meant to check is not a check.
func (s *Server) originOK(r *http.Request) bool {
	if !slices.Contains(s.allowed.hosts, r.Host) {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	// Normalised the same way the allowlist was, so a browser that spells the
	// default port out is compared against the same string as one that omits it.
	norm, err := normalizeOrigin(origin)
	if err != nil {
		return false
	}
	return slices.Contains(s.allowed.origins, norm)
}

// ---- the browser's credential ----

// A browser authenticates with a cookie, not with the token in its URL.
//
// ?token= is the jupyter pattern, and on loopback it is fine. Behind a reverse
// proxy it is in the access log of every hop, in the Referer of anything the
// page links to, and in the URL of every websocket the page opens. So the page
// trades it once for an opaque cookie: HttpOnly, so a script that got onto the
// page cannot read it back out; SameSite=Strict, so no other site can cause it
// to be sent at all; and kept in a file of its own when the daemon is given
// one, so a restart no longer signs every browser out. The phone this exists
// for is signed back in only by a token that lives in a terminal on another
// machine, so without the file, deploying a new binary logs it out. Revoking a
// session is logging out, or deleting the file while the daemon is stopped.
//
// SameSite=Strict and the exact Origin check are both needed and neither is
// enough. Strict covers the cross-site GET that carries no Origin header at
// all; the Origin check covers the browser whose SameSite support is not what
// this daemon assumed.
//
// The bearer token stays for whatever is not a browser.
const (
	cookieName = "aigem"
	// cookieTTL is how long a browser stays signed in. A month, because the
	// phone this exists for is the device that leaves the tab in the background
	// for weeks; reissuing is one request with a token the page still holds.
	cookieTTL = 30 * 24 * time.Hour
	// maxCookieSessions bounds the table. One operator holds a handful - a
	// laptop, a phone, a tablet - and the cap is only here so that repeated
	// exchanges cannot grow it without end. The one expiring soonest goes
	// first, which is the one issued longest ago.
	maxCookieSessions = 64
	// renewWithin is how close to expiry a cookie has to be before the exchange
	// replaces it rather than keeping it. It is what makes the month a sliding
	// window rather than a deadline: a phone in daily use is never signed out,
	// and a page load still does not mint a session every time - which is what
	// walked the cap and signed the phone out from a laptop.
	renewWithin = 7 * 24 * time.Hour
)

// issueCookie mints a browser session and returns its value.
func (s *Server) issueCookie() (string, error) {
	id, err := newToken()
	if err != nil {
		return "", err
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, exp := range s.cookies {
		if now.After(exp) {
			delete(s.cookies, k)
		}
	}
	for len(s.cookies) >= maxCookieSessions {
		var oldest string
		var at time.Time
		for k, exp := range s.cookies {
			if oldest == "" || exp.Before(at) {
				oldest, at = k, exp
			}
		}
		delete(s.cookies, oldest)
	}
	s.cookies[id] = now.Add(cookieTTL)
	s.saveCookies()
	return id, nil
}

// cookieOK reports whether the request carries a live browser session.
func (s *Server) cookieOK(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.cookies[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.cookies, c.Value)
		s.saveCookies()
		return false
	}
	return true
}

// cookieFresh reports whether the request carries a live cookie that is not yet
// close enough to expiry to be worth replacing.
func (s *Server) cookieFresh(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.cookies[c.Value]
	return ok && time.Until(exp) > renewWithin
}

// secureFor decides whether the cookie is marked Secure.
//
// It is set whenever the request reached us over TLS, or under an origin the
// operator configured as https - which is how it is set behind a proxy that
// terminates TLS, since the hop to us is plain HTTP. It is not set otherwise: a
// Secure cookie on a plain-HTTP deployment is one the browser accepts and never
// sends back, which is a sign-in that fails with nothing to see.
//
// The Origin here has already been matched against the allowlist by the caller,
// so reading it is not trusting the request.
func (s *Server) secureFor(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if o := r.Header.Get("Origin"); o != "" {
		norm, err := normalizeOrigin(o)
		return err == nil && strings.HasPrefix(norm, "https://")
	}
	for _, o := range s.allowed.public {
		if after, ok := strings.CutPrefix(o, "https://"); ok && after == r.Host {
			return true
		}
	}
	return false
}

// handleAuthSession trades a valid token for a cookie. The guard has already
// accepted the request, by either credential, so a page whose cookie is about
// to expire renews it the same way it got one.
//
// A request that already carries a cookie with time left on it keeps it. The
// page runs this exchange on every load, and minting a session each time would
// walk the table cap - sixty-four reloads on a laptop would have signed the
// phone out. Inside renewWithin it is replaced, so a browser in regular use
// never reaches the expiry at all.
func (s *Server) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	if s.cookieFresh(r) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// The one being replaced goes now rather than lingering for the rest of its
	// window: a renewal that left both live would grow the table by one per
	// renewal, which is the accumulation the reuse above exists to stop.
	s.revokeCookie(r)
	id, err := s.issueCookie()
	if err != nil {
		http.Error(w, "could not open a session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    id,
		Path:     "/",
		MaxAge:   int(cookieTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secureFor(r),
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// revokeCookie forgets the browser session this request carries, if any.
func (s *Server) revokeCookie(r *http.Request) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Only a held session is worth a disk write: the cookie exchange runs this
	// on every page load, and a request carrying a value the table never issued
	// must not be able to make the daemon write at all.
	if _, held := s.cookies[c.Value]; held {
		delete(s.cookies, c.Value)
		s.saveCookies()
	}
}

// handleAuthLogout revokes the cookie this request carried, and tells the
// browser to drop it.
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	s.revokeCookie(r)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secureFor(r),
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// Guard wraps a handler so it answers only for a request that passed every
// check, and so its response carries the daemon's security headers. Every route
// that is not the page itself goes through it, so a route added later cannot
// answer under weaker rules than the ones this file sets.
func (s *Server) Guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.guard(w, r) {
			return
		}
		// Held for as long as the handler runs, which for a websocket is as long
		// as the connection lives.
		if isUpgrade(r) {
			if !s.sockets.acquire() {
				w.Header().Set("Retry-After", "5")
				http.Error(w, "too many open connections", http.StatusServiceUnavailable)
				return
			}
			defer s.sockets.release()
		}
		h(w, r)
	}
}

// guard applies every check to a request. It writes the response and reports
// false when the request is refused.
//
// It sets no headers of its own: withSecurityHeaders wraps the whole mux, so a
// refusal written here already carries the policy - and a 401 page is a page a
// browser renders, so that is not optional. See TestRefusalsCarryThePolicy.
func (s *Server) guard(w http.ResponseWriter, r *http.Request) bool {
	if !s.originOK(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return false
	}
	// The credential is checked first, and a good one is never refused.
	//
	// The other order - refuse a blocked address before looking - is the usual
	// one, and it is wrong here. It cost nothing against guessing, because the
	// secret is 32 random bytes compared in constant time and no rate bounds
	// 2^256; and it denied the operator, because clientAddr behind a proxy is
	// the proxy, so ten wrong tokens a minute from anywhere locked out every
	// real client indefinitely. It did not even need an attacker: restarting
	// the daemon revokes every cookie it did not write to a file, the open
	// tab's socket then retried every 5s against a bucket refilling every 6s,
	// and the tab never recovered.
	//
	// What the limiter is worth is refusing a repeat failure before the request
	// reaches anything behind this. It no longer saves the credential check
	// itself, which is the one thing this ordering gives up - a constant-time
	// compare against a header that net/http has already read.
	addr := clientAddr(r)
	if s.cookieOK(r) || tokenOK(s.token, requestToken(r)) {
		s.failures.clear(addr)
		return true
	}
	if wait, blocked := s.failures.blocked(addr); blocked {
		// Deliberately without spending a token: an address that is already
		// blocked must be able to come back, and charging it for being told so
		// is how a lockout becomes permanent.
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(wait.Seconds()+0.5))))
		http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
		return false
	}
	s.failures.fail(addr)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}
