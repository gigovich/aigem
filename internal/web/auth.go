package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log/slog"
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
	if !slices.Contains(s.allowed.hosts, normalizeHost(r.Host)) {
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
//
// A session that could not be written to disk still works until this daemon
// stops, so the write failure is logged rather than refused: turning a disk
// problem into a sign-in that fails with nothing to see helps nobody.
func (s *Server) issueCookie() (string, error) {
	id, err := newToken()
	if err != nil {
		return "", err
	}
	now := time.Now()
	var dropped []string
	s.mu.Lock()
	for k, exp := range s.cookies {
		if now.After(exp) {
			delete(s.cookies, k)
			dropped = append(dropped, k)
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
		dropped = append(dropped, oldest)
	}
	s.cookies[id] = now.Add(cookieTTL)
	change := s.pendingLocked(cookieChange{
		add:    map[string]time.Time{id: s.cookies[id]},
		remove: dropped,
	})
	s.mu.Unlock()

	if err := s.persist(change); err != nil {
		slog.Warn("could not record the browser session; a restart will sign it out",
			"path", s.cookieStore.Path(), "err", err)
	}
	return id, nil
}

// cookieOK reports whether the request carries a live browser session.
//
// This runs on every request, so the disk write an expiry triggers happens
// after s.mu is released: holding the daemon's only mutex across two fsyncs and
// a lock file another process may hold would queue every concurrent request
// behind it.
func (s *Server) cookieOK(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	s.mu.Lock()
	exp, ok := s.cookies[c.Value]
	if !ok {
		s.mu.Unlock()
		return false
	}
	if !time.Now().After(exp) {
		s.mu.Unlock()
		return true
	}
	delete(s.cookies, c.Value)
	change := s.pendingLocked(cookieChange{remove: []string{c.Value}})
	s.mu.Unlock()

	if err := s.persist(change); err != nil {
		slog.Warn("could not record an expired browser session being dropped",
			"path", s.cookieStore.Path(), "err", err)
	}
	return false
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
// It is set whenever the request reached us over TLS, or arrived under a name
// the operator configured as https - which is how it is set behind a proxy that
// terminates TLS, since the hop to us is plain HTTP. It is not set otherwise: a
// Secure cookie on a plain-HTTP deployment is one the browser accepts and never
// sends back, which is a sign-in that fails with nothing to see.
//
// The stated origin is checked before the request's own Origin header, and that
// order is load-bearing. The loopback origins are on the allowlist by
// construction, so a request carrying Host: the-https-name and Origin:
// http://127.0.0.1:7777 passes the guard - and reading the header first would
// then hand an https deployment a cookie with no Secure flag. A proxy that
// rewrites Origin to the backend's own is not hypothetical: this repository
// ships one for the dev server.
//
// Both values have already been matched against the allowlist by the caller, so
// reading either is not trusting the request.
func (s *Server) secureFor(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host := normalizeHost(r.Host)
	for _, o := range s.allowed.public {
		if after, ok := strings.CutPrefix(o, "https://"); ok && after == host {
			return true
		}
	}
	if o := r.Header.Get("Origin"); o != "" {
		norm, err := normalizeOrigin(o)
		return err == nil && strings.HasPrefix(norm, "https://")
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
	// renewal, which is the accumulation the reuse above exists to stop. A
	// failure to record that removal is not worth refusing the renewal over -
	// the worst case is a restart honouring a cookie the browser has replaced.
	if err := s.revokeCookie(r); err != nil {
		slog.Warn("could not record a replaced browser session", "err", err)
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

// revokeCookie forgets the browser session this request carries, if any, and
// reports whether that removal reached the disk.
//
// The error matters here in a way it does not when issuing. A revocation that
// only cleared the table is undone by the next restart, so a caller that
// answered "signed out" would have said something untrue - which is the one
// thing an operator who has lost a phone cannot afford.
func (s *Server) revokeCookie(r *http.Request) error {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	s.mu.Lock()
	// Checked before the table, not after: Close empties the table under this
	// same mutex, so a request arriving during shutdown would otherwise find
	// nothing held, return nil, and be answered "signed out" while the file keeps
	// the session for the next start.
	if s.closed && s.cookieStore != nil {
		s.mu.Unlock()
		return errors.New("the daemon is shutting down and cannot record the sign-out")
	}
	// Only a held session is worth a disk write: the cookie exchange runs this
	// on every page load, and a request carrying a value the table never issued
	// must not be able to make the daemon write at all.
	_, held := s.cookies[c.Value]
	if !held {
		s.mu.Unlock()
		return nil
	}
	delete(s.cookies, c.Value)
	change := s.pendingLocked(cookieChange{remove: []string{c.Value}})
	s.mu.Unlock()

	return s.persist(change)
}

// handleAuthLogout revokes the cookie this request carried, and tells the
// browser to drop it.
//
// A revocation the daemon could not record is a failure, not a detail: the
// browser would drop its cookie, the file would keep the session, and the next
// restart would honour it again with nothing left that could present it.
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.revokeCookie(r); err != nil {
		slog.Warn("could not record a sign-out", "err", err)
		http.Error(w, "signed out here, but the daemon could not record it; "+
			"the session may come back when it restarts", http.StatusInternalServerError)
		return
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
// check. Every route goes through it except the page, which a browser fetches
// before it can hold any credential, and /healthz, which is a liveness probe -
// so a route added later must be wrapped here or it answers under weaker rules
// than the ones this file sets.
func (s *Server) Guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.guard(w, r) {
			return
		}
		// Held for as long as the handler runs. A websocket handler has to block
		// for the life of its connection for that to bound anything: one that
		// hijacks and returns, leaving goroutines behind, releases the slot
		// immediately and makes this cap decorative.
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
