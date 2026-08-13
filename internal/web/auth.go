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
// address. Behind this endpoint sits an agent with bash, filesystem writes and
// the credential store, so a token and an exact-match origin check are here
// from the first commit rather than added once that endpoint is worth
// attacking.

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
// handshake. It is no longer how a browser authenticates one - the cookie is,
// and a browser sends that on a handshake by itself - but it stays for the
// first request a page makes, which is the exchange that gets it the cookie,
// and for a client that cannot hold one.
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
// in. A request with no Origin is not from a browser page - curl, the attach
// client, a test - and is allowed on the strength of the token alone.
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
// to be sent at all; and held in memory, so a restart of the daemon revokes
// every one of them.
//
// SameSite=Strict and the exact Origin check are both needed and neither is
// enough. Strict covers the cross-site GET that carries no Origin header at
// all; the Origin check covers the browser whose SameSite support is not what
// this daemon assumed.
//
// The bearer token stays for the CLI - `aigem chat`, `aigem attach`. Those are
// not browsers and nothing forges a request from them.
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
		return false
	}
	return true
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
// A request that already carries a live cookie keeps it. The page runs this
// exchange on every load, and minting a session each time would walk the table
// cap - sixty-four reloads on a laptop would have signed the phone out.
func (s *Server) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	if s.cookieOK(r) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
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

// handleAuthLogout revokes the cookie this request carried, and tells the
// browser to drop it.
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		s.mu.Lock()
		delete(s.cookies, c.Value)
		s.mu.Unlock()
	}
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
// check, and so its response carries the daemon's security headers. It is what
// Config.Mount is handed: a package adding routes here must not be able to
// answer under weaker rules than the ones this file sets.
//
// The headers go on before the checks so a refusal carries them too.
func (s *Server) Guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.guard(w, r) {
			return
		}
		// Held for as long as the handler runs, which for a websocket is as long
		// as the connection lives - both socket handlers block until their
		// client goes away.
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
// The headers go on here rather than in Guard, because half the daemon's own
// routes call this directly: a refusal from one of those was carrying no CSP
// and no nosniff, which made Config.Mount's promise that nothing can answer
// under weaker rules untrue of the daemon itself.
func (s *Server) guard(w http.ResponseWriter, r *http.Request) bool {
	securityHeaders(w.Header())
	if !s.originOK(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return false
	}
	// The credential is checked first, and a good one is never refused.
	//
	// The other order - refuse a blocked address before looking - is the usual
	// one, and it is wrong here. It cost nothing against guessing, because the
	// secret is 32 random bytes compared in constant time and no rate bounds
	// 2^256; and it denied the operator, because `clientAddr` behind a proxy is
	// the proxy, so ten wrong tokens a minute from anywhere locked out every
	// real client indefinitely. It did not even need an attacker: restarting
	// the fleet revokes every cookie, the open tab's socket then retried every
	// 5s against a bucket refilling every 6s, and the tab never recovered.
	//
	// What the limiter is worth is refusing a *repeat failure* without doing
	// any further work for it. That is all it does now.
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
