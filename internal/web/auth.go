package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
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
// string. Browsers cannot set headers on a websocket handshake, which is the
// only reason the query form exists; on loopback that is acceptable, and it is
// worth revisiting before the daemon listens anywhere else, since query strings
// leak into logs and referrers.
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
func (s *Server) originOK(r *http.Request) bool {
	if !s.hostAllowed(r.Host) {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return s.hostAllowed(u.Host)
}

func (s *Server) hostAllowed(host string) bool {
	if host == "" {
		return false
	}
	for _, a := range s.allowedHosts {
		if host == a {
			return true
		}
	}
	return false
}

// guard applies both checks to an ordinary request. It writes the response and
// reports false when the request is refused.
func (s *Server) guard(w http.ResponseWriter, r *http.Request) bool {
	if !s.originOK(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return false
	}
	if !tokenOK(s.token, requestToken(r)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}
