package mcp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// authFlowTimeout bounds the whole interactive authorization (browser + exchange).
const authFlowTimeout = 3 * time.Minute

// oauthHandler implements auth.OAuthHandler for one server with on-disk token
// persistence: a saved token is reused (and refreshed) across runs, so the
// browser flow runs only on first login or after the refresh token is revoked.
type oauthHandler struct {
	server   string
	endpoint string // the MCP server URL (the protected resource)
	client   *http.Client
	openURL  func(string) error // browser opener; overridable in tests

	mu    sync.Mutex
	ts    oauth2.TokenSource
	state *oauthState
}

func newOAuthHandler(server, endpoint string) *oauthHandler {
	return &oauthHandler{server: server, endpoint: endpoint, client: http.DefaultClient, openURL: openBrowser}
}

// TokenSource returns a refreshing token source seeded from disk, or nil when no
// token has been obtained yet (which makes the first request 401 and triggers
// Authorize).
func (h *oauthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ts != nil {
		return h.ts, nil
	}
	if h.state == nil {
		st, err := loadOAuthState(h.server)
		if err != nil {
			return nil, err
		}
		h.state = st
	}
	if h.state != nil && h.state.Token != nil && h.state.TokenEndpoint != "" {
		h.ts = h.persistingSource(ctx, h.state, h.state.Token)
		return h.ts, nil
	}
	return nil, nil
}

// Authorize runs the OAuth 2.0 authorization-code-with-PKCE flow against the
// server's authorization server, discovered from the 401 challenge.
func (h *oauthHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) (err error) {
	defer resp.Body.Close()
	defer func() { _, _ = io.Copy(io.Discard, resp.Body) }()

	ctx, cancel := context.WithTimeout(ctx, authFlowTimeout)
	defer cancel()

	h.mu.Lock()
	st := h.state
	h.mu.Unlock()
	if st == nil {
		if loaded, _ := loadOAuthState(h.server); loaded != nil {
			st = loaded
		} else {
			st = &oauthState{}
		}
	}
	// If a persisted (possibly revoked) client makes the flow fail, drop it so the
	// next attempt re-registers from scratch instead of staying wedged.
	hadClient := st.ClientID != ""
	defer func() {
		if err != nil && hadClient {
			st.ClientID, st.ClientSecret, st.Token = "", "", nil
			_ = saveOAuthState(h.server, st)
		}
	}()

	cb, err := startCallback(st.RedirectURL)
	if err != nil {
		return err
	}
	defer cb.close()

	challenges, err := oauthex.ParseWWWAuthenticate(resp.Header[http.CanonicalHeaderKey("WWW-Authenticate")])
	if err != nil {
		return fmt.Errorf("parse WWW-Authenticate: %w", err)
	}
	prm, err := oauthex.GetProtectedResourceMetadata(ctx, resourceMetadataURL(challenges, h.endpoint), h.endpoint, h.client)
	if err != nil {
		return fmt.Errorf("protected resource metadata: %w", err)
	}
	if len(prm.AuthorizationServers) == 0 {
		return fmt.Errorf("server advertises no authorization servers")
	}
	asm, err := auth.GetAuthServerMetadata(ctx, prm.AuthorizationServers[0], h.client)
	if err != nil {
		return fmt.Errorf("authorization server metadata: %w", err)
	}
	if asm == nil {
		return fmt.Errorf("authorization server %q exposes no metadata", prm.AuthorizationServers[0])
	}

	st.Resource = prm.Resource
	if st.Resource == "" {
		st.Resource = h.endpoint
	}
	st.RedirectURL = cb.redirectURL
	st.AuthEndpoint = asm.AuthorizationEndpoint
	st.TokenEndpoint = asm.TokenEndpoint
	st.Scopes = pickScopes(prm.ScopesSupported, asm.ScopesSupported)

	if st.ClientID == "" {
		if asm.RegistrationEndpoint == "" {
			return fmt.Errorf("server requires a pre-registered client (no dynamic registration endpoint); " +
				"set client_id via config")
		}
		reg, err := oauthex.RegisterClient(ctx, asm.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{
			RedirectURIs:            []string{cb.redirectURL},
			ClientName:              "aigem",
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "none",
		}, h.client)
		if err != nil {
			return fmt.Errorf("dynamic client registration: %w", err)
		}
		st.ClientID, st.ClientSecret = reg.ClientID, reg.ClientSecret
	}

	cfg := h.oauthConfig(st)
	verifier := oauth2.GenerateVerifier()
	state := randState()
	authURL := cfg.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("resource", st.Resource))

	fmt.Fprintf(os.Stderr, "\nMCP %q needs authorization. Opening your browser; if it does not open, visit:\n%s\n",
		h.server, authURL)
	if err := h.openURL(authURL); err != nil {
		fmt.Fprintf(os.Stderr, "(could not open browser automatically: %v)\n", err)
	}

	res, err := cb.wait(ctx)
	if err != nil {
		return fmt.Errorf("waiting for authorization callback: %w", err)
	}
	if res.err != "" {
		return fmt.Errorf("authorization denied: %s", res.err)
	}
	if res.state != state {
		return fmt.Errorf("authorization state mismatch (possible CSRF)")
	}

	clientCtx := context.WithValue(ctx, oauth2.HTTPClient, h.client)
	tok, err := cfg.Exchange(clientCtx, res.code,
		oauth2.VerifierOption(verifier),
		oauth2.SetAuthURLParam("resource", st.Resource))
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}
	st.Token = tok
	if err := saveOAuthState(h.server, st); err != nil {
		return fmt.Errorf("persist token: %w", err)
	}

	h.mu.Lock()
	h.state = st
	h.ts = h.persistingSource(ctx, st, tok)
	h.mu.Unlock()
	return nil
}

// oauthConfig builds the oauth2 config from discovered/persisted endpoints.
func (h *oauthHandler) oauthConfig(st *oauthState) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     st.ClientID,
		ClientSecret: st.ClientSecret,
		Endpoint:     oauth2.Endpoint{AuthURL: st.AuthEndpoint, TokenURL: st.TokenEndpoint},
		RedirectURL:  st.RedirectURL,
		Scopes:       st.Scopes,
	}
}

// persistingSource wraps the refreshing token source so each new token (after a
// refresh) is written back to disk.
func (h *oauthHandler) persistingSource(ctx context.Context, st *oauthState, tok *oauth2.Token) oauth2.TokenSource {
	clientCtx := context.WithValue(context.WithoutCancel(ctx), oauth2.HTTPClient, h.client)
	base := h.oauthConfig(st).TokenSource(clientCtx, tok)
	return oauth2.ReuseTokenSource(tok, &savingSource{server: h.server, st: st, base: base})
}

// savingSource persists a token whenever it changes (e.g. on refresh).
type savingSource struct {
	server string
	mu     sync.Mutex
	st     *oauthState
	base   oauth2.TokenSource
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	tok, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Token == nil || tok.AccessToken != s.st.Token.AccessToken ||
		(tok.RefreshToken != "" && tok.RefreshToken != s.st.Token.RefreshToken) {
		s.st.Token = tok
		_ = saveOAuthState(s.server, s.st)
	}
	return tok, nil
}

// ---- discovery + callback helpers ----

// resourceMetadataURL returns the protected-resource-metadata URL from the 401
// challenge, or the well-known location on the endpoint's origin.
func resourceMetadataURL(challenges []oauthex.Challenge, endpoint string) string {
	for _, c := range challenges {
		if c.Scheme == "bearer" {
			if u := c.Params["resource_metadata"]; u != "" {
				return u
			}
		}
	}
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource"
	}
	return endpoint
}

func pickScopes(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

func randState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

const callbackHTML = `<!doctype html><meta charset=utf-8><title>aigem</title>
<body style="font-family:sans-serif;text-align:center;margin-top:4rem">
<h2>Authorization complete</h2><p>You can close this tab and return to aigem.</p></body>`

type callbackResult struct {
	code  string
	state string
	err   string
}

// callbackServer is the transient loopback HTTP server that receives the OAuth
// redirect.
type callbackServer struct {
	redirectURL string
	srv         *http.Server
	results     chan callbackResult
}

// startCallback binds a loopback-only listener and serves the callback. It
// reuses preferred's port/path when given (so a persisted client's registered
// redirect still matches), but always binds 127.0.0.1 and falls back to an
// ephemeral port if the preferred one is busy. The host from preferred is never
// trusted - the code must only ever be delivered over loopback.
func startCallback(preferred string) (*callbackServer, error) {
	port, path := "0", "/oauth/callback"
	if preferred != "" {
		if u, err := url.Parse(preferred); err == nil {
			if _, p, err := net.SplitHostPort(u.Host); err == nil && p != "" {
				port = p
			}
			if u.Path != "" {
				path = u.Path
			}
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil && port != "0" {
		ln, err = net.Listen("tcp", "127.0.0.1:0") // preferred port taken; pick a free one
	}
	if err != nil {
		return nil, fmt.Errorf("bind oauth callback: %w", err)
	}
	cs := &callbackServer{results: make(chan callbackResult, 1), redirectURL: "http://" + ln.Addr().String() + path}
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code") == "" && q.Get("error") == "" {
			http.Error(w, "missing authorization code", http.StatusBadRequest)
			return
		}
		select {
		case cs.results <- callbackResult{code: q.Get("code"), state: q.Get("state"), err: q.Get("error")}:
		default:
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, callbackHTML)
	})
	cs.srv = &http.Server{Handler: mux}
	go func() { _ = cs.srv.Serve(ln) }()
	return cs, nil
}

func (cs *callbackServer) wait(ctx context.Context) (callbackResult, error) {
	select {
	case res := <-cs.results:
		return res, nil
	case <-ctx.Done():
		return callbackResult{}, ctx.Err()
	}
}

func (cs *callbackServer) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = cs.srv.Shutdown(ctx)
}

// openBrowser launches the platform browser at url and also echoes it so a
// headless user can copy it.
func openBrowser(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{url}
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		cmd, args = "xdg-open", []string{url}
	}
	return exec.Command(cmd, args...).Start()
}
