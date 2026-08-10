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
		for _, want := range []string{"img-src 'self' data:", "connect-src 'self'", "frame-ancestors 'none'"} {
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
