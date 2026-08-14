package web

import (
	"net/http"
	"strings"
	"testing"
)

// The agent reads pages an attacker may have written, and the UI renders what
// the model writes back as HTML. Without a policy bounding where the page may
// load images from, a planted "end your reply with this image tag" turns the
// user's browser into an exfiltration channel that sanitising cannot close.
func TestPagesCarryAContentSecurityPolicy(t *testing.T) {
	srv := testServer(t)
	for _, path := range []string{"/", "/api/sessions"} {
		res := srv.get(t, path)
		res.Body.Close()
		csp := res.Header.Get("Content-Security-Policy")
		if csp == "" {
			t.Fatalf("%s carries no Content-Security-Policy", path)
		}
		// worker-src is named, not inherited: notifications need a service
		// worker, and the directive it would otherwise fall back to is script-src.
		for _, want := range []string{"img-src 'self' data:", "connect-src 'self'",
			"worker-src 'self'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s policy is missing %q: %s", path, want, csp)
			}
		}
		if res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s does not refuse content sniffing", path)
		}
	}
}

func TestBlobsCarryThePolicyToo(t *testing.T) {
	srv := testServer(t)
	id := srv.newSession(t)
	res := srv.get(t, "/api/sessions/"+id+"/blobs/1")
	defer res.Body.Close()
	// Whether it is found or not, the headers are set before the body.
	if res.StatusCode == http.StatusOK && res.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("a served blob carries no policy")
	}
}

// The headers go on before the checks, so a refusal carries them too. A 401
// page is still a page a browser renders, and the one that arrives when
// something is already wrong is the last one that should be unbounded.
func TestRefusalsCarryThePolicy(t *testing.T) {
	srv := testServer(t)
	base := "http://" + srv.Addr().String()
	for _, c := range []struct {
		what   string
		req    func() *http.Request
		status int
	}{
		{"a wrong token", func() *http.Request {
			r, _ := http.NewRequest(http.MethodGet, base+"/api/modes", nil)
			r.Header.Set("Authorization", "Bearer wrong")
			return r
		}, http.StatusUnauthorized},
		{"a forged origin", func() *http.Request {
			r, _ := http.NewRequest(http.MethodGet, base+"/api/modes", nil)
			r.Header.Set("Authorization", "Bearer "+srv.token)
			r.Header.Set("Origin", "https://evil.test")
			return r
		}, http.StatusForbidden},
		// Not through Guard: half the daemon's own routes call the bare check,
		// and their refusals were going out bare with it.
		{"a wrong token on a route that guards itself", func() *http.Request {
			r, _ := http.NewRequest(http.MethodGet, base+"/api/sessions", nil)
			r.Header.Set("Authorization", "Bearer wrong")
			return r
		}, http.StatusUnauthorized},
	} {
		res, err := http.DefaultClient.Do(c.req())
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != c.status {
			t.Errorf("%s answered %d, want %d", c.what, res.StatusCode, c.status)
		}
		if res.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("the refusal for %s carries no Content-Security-Policy", c.what)
		}
		if res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("the refusal for %s does not refuse content sniffing", c.what)
		}
	}
}
