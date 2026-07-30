package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// fakeAuthServer is an in-process OAuth authorization server + protected
// resource for exercising the full flow.
type fakeAuthServer struct {
	srv          *httptest.Server
	base         string
	challenges   map[string]string // code -> code_challenge
	accessCount  int               // tokens minted (to observe refresh)
	failExchange bool              // simulate a revoked client: reject code exchange
}

func newFakeAuthServer(t *testing.T) *fakeAuthServer {
	t.Helper()
	f := &fakeAuthServer{challenges: map[string]string{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"resource":              f.base + "/mcp",
			"authorization_servers": []string{f.base},
			"scopes_supported":      []string{"mcp"},
		})
	})
	asMeta := func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                f.base,
			"authorization_endpoint":                f.base + "/authorize",
			"token_endpoint":                        f.base + "/token",
			"registration_endpoint":                 f.base + "/register",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"},
			"scopes_supported":                      []string{"mcp"},
		})
	}
	mux.HandleFunc("/.well-known/oauth-authorization-server", asMeta)
	mux.HandleFunc("/.well-known/openid-configuration", asMeta)

	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"client_id": "test-client"})
	})

	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := "auth-code-1"
		f.challenges[code] = q.Get("code_challenge")
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+q.Get("state"), http.StatusFound)
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if f.failExchange {
				http.Error(w, "invalid_client", http.StatusBadRequest)
				return
			}
			code := r.Form.Get("code")
			want := f.challenges[code]
			if got := s256(r.Form.Get("code_verifier")); got != want {
				http.Error(w, "bad pkce", http.StatusBadRequest)
				return
			}
		case "refresh_token":
			if r.Form.Get("refresh_token") != "refresh-1" {
				http.Error(w, "bad refresh", http.StatusBadRequest)
				return
			}
		default:
			http.Error(w, "bad grant", http.StatusBadRequest)
			return
		}
		f.accessCount++
		writeJSON(w, map[string]any{
			"access_token":  "access-" + itoa(f.accessCount),
			"token_type":    "Bearer",
			"refresh_token": "refresh-1",
			"expires_in":    3600,
		})
	})

	f.srv = httptest.NewServer(mux)
	f.base = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

// challenge401 builds a synthetic 401 response that points at the fake server's
// resource metadata, as the MCP transport would receive.
func (f *fakeAuthServer) challenge401() (*http.Request, *http.Response) {
	req, _ := http.NewRequest(http.MethodPost, f.base+"/mcp", http.NoBody)
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header: http.Header{
			"Www-Authenticate": []string{`Bearer resource_metadata="` + f.base + `/.well-known/oauth-protected-resource"`},
		},
		Body:    http.NoBody,
		Request: req,
	}
	return req, resp
}

func autoBrowser(u string) error {
	resp, err := http.Get(u) // follows the AS redirect into the loopback callback
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func TestOAuthFullFlowAndReuse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := newFakeAuthServer(t)
	ctx := context.Background()

	h := newOAuthHandler("acme", f.base+"/mcp")
	h.openURL = autoBrowser

	// No token yet -> TokenSource is nil.
	if ts, _ := h.TokenSource(ctx); ts != nil {
		t.Fatal("expected nil token source before authorization")
	}

	req, resp := f.challenge401()
	if err := h.Authorize(ctx, req, resp); err != nil {
		t.Fatalf("authorize: %v", err)
	}

	ts, err := h.TokenSource(ctx)
	if err != nil || ts == nil {
		t.Fatalf("token source after auth: ts=%v err=%v", ts, err)
	}
	tok, err := ts.Token()
	if err != nil || tok.AccessToken != "access-1" {
		t.Fatalf("token = %+v, err=%v", tok, err)
	}

	// Persisted to disk with the registered client.
	st, err := loadOAuthState("acme")
	if err != nil || st == nil || st.ClientID != "test-client" || st.Token == nil {
		t.Fatalf("persisted state = %+v, err=%v", st, err)
	}

	// A fresh handler reuses the saved token without any browser interaction.
	h2 := newOAuthHandler("acme", f.base+"/mcp")
	h2.openURL = func(string) error { t.Fatal("reuse must not open a browser"); return nil }
	ts2, _ := h2.TokenSource(ctx)
	if ts2 == nil {
		t.Fatal("expected reused token source")
	}
	tok2, err := ts2.Token()
	if err != nil || tok2.AccessToken != "access-1" {
		t.Fatalf("reused token = %+v, err=%v", tok2, err)
	}

	if minted := f.accessCount; minted != 1 {
		t.Fatalf("expected exactly 1 token minted, got %d", minted)
	}
}

func TestOAuthRefreshPersists(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := newFakeAuthServer(t)
	ctx := context.Background()

	// Seed an expired token so the next read forces a refresh.
	st := &oauthState{
		Resource:      f.base + "/mcp",
		AuthEndpoint:  f.base + "/authorize",
		TokenEndpoint: f.base + "/token",
		ClientID:      "test-client",
		Token: &oauth2.Token{
			AccessToken:  "stale",
			TokenType:    "Bearer",
			RefreshToken: "refresh-1",
			Expiry:       time.Now().Add(-time.Hour),
		},
	}
	if err := saveOAuthState("acme", st); err != nil {
		t.Fatal(err)
	}

	h := newOAuthHandler("acme", f.base+"/mcp")
	ts, _ := h.TokenSource(ctx)
	if ts == nil {
		t.Fatal("expected token source from seeded state")
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tok.AccessToken == "stale" {
		t.Fatal("token was not refreshed")
	}

	// The refreshed token is written back to disk.
	reloaded, _ := loadOAuthState("acme")
	if reloaded.Token.AccessToken != tok.AccessToken {
		t.Fatalf("refreshed token not persisted: disk=%q live=%q", reloaded.Token.AccessToken, tok.AccessToken)
	}
}

func TestRevokedClientIsClearedForRecovery(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := newFakeAuthServer(t)
	f.failExchange = true // the persisted client is no longer accepted
	ctx := context.Background()

	// Seed a persisted (now-dead) client without a token.
	if err := saveOAuthState("acme", &oauthState{ClientID: "dead-client"}); err != nil {
		t.Fatal(err)
	}

	h := newOAuthHandler("acme", f.base+"/mcp")
	h.openURL = autoBrowser
	if _, err := h.TokenSource(ctx); err != nil { // loads state from disk
		t.Fatal(err)
	}
	req, resp := f.challenge401()
	if err := h.Authorize(ctx, req, resp); err == nil {
		t.Fatal("expected authorize to fail with a revoked client")
	}

	// The dead client must be cleared so the next attempt re-registers.
	st, _ := loadOAuthState("acme")
	if st != nil && st.ClientID == "dead-client" {
		t.Fatal("revoked client_id was not cleared after failure")
	}
}

func TestLogoutClearsToken(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := saveOAuthState("acme", &oauthState{ClientID: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := Logout("acme"); err != nil {
		t.Fatal(err)
	}
	st, err := loadOAuthState("acme")
	if err != nil {
		t.Fatal(err)
	}
	if st != nil {
		t.Fatal("token still present after logout")
	}
	if err := Logout("acme"); err != nil {
		t.Fatalf("logout of missing server should be a no-op: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
