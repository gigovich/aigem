package web

import (
	"net/http"
	"strings"
	"testing"
)

// The headers are applied as one wrapper around the mux, so a refusal carries
// them by construction rather than by each refusing path remembering to. A 401
// page is still a page a browser renders, and the one that arrives when
// something is already wrong is the last one that should be unbounded.
func TestRefusalsCarryThePolicy(t *testing.T) {
	srv := newTestServer(t, Config{})
	for _, c := range []struct {
		what   string
		set    func(*http.Request)
		status int
	}{
		{"a wrong token", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer wrong")
		}, http.StatusUnauthorized},
		{"a forged origin", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+srv.token)
			r.Header.Set("Origin", "https://evil.test")
		}, http.StatusForbidden},
		{"a forged host", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+srv.token)
			r.Host = "evil.test"
		}, http.StatusForbidden},
	} {
		res := exchangeWith(t, srv, c.set)
		if res.StatusCode != c.status {
			t.Errorf("%s answered %d, want %d", c.what, res.StatusCode, c.status)
		}
		if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("the refusal for %s carries no Content-Security-Policy", c.what)
		}
		if res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("the refusal for %s does not refuse content sniffing", c.what)
		}
	}
}
