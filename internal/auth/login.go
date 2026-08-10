package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// loginTimeout bounds the whole interactive browser + exchange flow.
const loginTimeout = 3 * time.Minute

// LoginChatGPT runs the ChatGPT (Codex public client) authorization-code-with-
// PKCE flow: it serves the fixed loopback redirect, opens the browser, exchanges
// the code, and decodes the id_token for the chatgpt_account_id. The returned
// Record is ready to persist.
func LoginChatGPT(ctx context.Context, allowStdinPaste bool) (Record, error) {
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()

	cfg := oauthConfig()
	verifier := oauth2.GenerateVerifier()
	// The state is generated before the listener, because the listener needs it:
	// it is what tells the provider's redirect apart from any other page's
	// request to the same well-known loopback URL.
	state, err := randState()
	if err != nil {
		return Record{}, fmt.Errorf("generate state: %w", err)
	}
	cb, err := startCallback(chatGPTRedirect, state, allowStdinPaste)
	if err != nil {
		return Record{}, err
	}
	defer cb.close()
	authURL := cfg.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		// The Codex client requires this extra parameter to mint an API-capable
		// token tied to the ChatGPT account.
		oauth2.SetAuthURLParam("id_token_add_organizations", "true"))

	fmt.Fprintf(os.Stderr, "\nOpening your browser to authorize aigem with ChatGPT.\n"+
		"Warning: ChatGPT subscription mode uses OpenAI's undocumented Codex backend; "+
		"the API-key path is the OpenAI-supported alternative.\n"+
		"If it does not open, visit this URL:\n\n%s\n\n", authURL)
	if err := openBrowser(authURL); err != nil {
		fmt.Fprintf(os.Stderr, "(could not open browser automatically: %v)\n", err)
	}
	fmt.Fprintln(os.Stderr, "Waiting for authorization (or paste the redirected URL here)...")

	res, err := cb.wait(ctx)
	if err != nil {
		return Record{}, fmt.Errorf("waiting for authorization: %w", err)
	}
	if res.err != "" {
		return Record{}, fmt.Errorf("authorization denied: %s", res.err)
	}
	if !stateOK(res, state) {
		return Record{}, fmt.Errorf("authorization state mismatch (possible CSRF)")
	}

	clientCtx := context.WithValue(ctx, oauth2.HTTPClient, http.DefaultClient)
	tok, err := cfg.Exchange(clientCtx, res.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Record{}, fmt.Errorf("token exchange: %w", err)
	}
	rec := Record{Kind: KindOAuth, Token: tok}
	if idTok, ok := tok.Extra("id_token").(string); ok {
		rec.AccountID = accountIDFromIDToken(idTok)
	}
	return rec, nil
}

// xaiLoginTimeout bounds the whole device-code flow. Device codes live for
// minutes (the server states the exact expiry in its response, which the poll
// honors); this is only the outer safety net.
const xaiLoginTimeout = 15 * time.Minute

// xaiDiscovery fetches the OIDC discovery document and returns the token and
// device-authorization endpoints. Both must stay on the xAI issuer host over
// https - a discovery document must not be able to redirect the flow (and the
// tokens it mints) to another host. Missing fields fall back to the compiled
// defaults.
func xaiDiscovery(ctx context.Context) (tokenURL, deviceURL string) {
	tokenURL, deviceURL = xaiTokenURLFallback, xaiDeviceAuthURL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, xaiDiscoveryURL, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var doc struct {
		TokenEndpoint               string `json:"token_endpoint"`
		DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc) != nil {
		return
	}
	if xaiEndpointOK(doc.TokenEndpoint) {
		tokenURL = doc.TokenEndpoint
	}
	if xaiEndpointOK(doc.DeviceAuthorizationEndpoint) {
		deviceURL = doc.DeviceAuthorizationEndpoint
	}
	return
}

// xaiEndpointOK accepts only https URLs on the xAI issuer host.
func xaiEndpointOK(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	iss, _ := url.Parse(xaiIssuer)
	return u.Scheme == "https" && iss != nil && u.Host == iss.Host
}

// LoginXAIDevice runs the xAI Grok subscription (Grok CLI public client)
// device-code flow: it requests a device code, shows the verification URL for
// the user to approve in any browser (works headless - the browser can be on a
// different machine), and polls for the token. The returned Record is ready to
// persist and carries the discovered token endpoint for later refreshes.
func LoginXAIDevice(ctx context.Context) (Record, error) {
	ctx, cancel := context.WithTimeout(ctx, xaiLoginTimeout)
	defer cancel()

	tokenURL, deviceURL := xaiDiscovery(ctx)
	cfg := &oauth2.Config{
		ClientID: xaiClientID,
		Endpoint: oauth2.Endpoint{DeviceAuthURL: deviceURL, TokenURL: tokenURL},
		Scopes:   xaiScopes,
	}
	da, err := cfg.DeviceAuth(ctx)
	if err != nil {
		return Record{}, fmt.Errorf("xai device-code request: %w", err)
	}
	verifyURL := da.VerificationURIComplete
	if verifyURL == "" {
		verifyURL = da.VerificationURI
	}
	fmt.Fprintf(os.Stderr, "\nAuthorize aigem with your Grok subscription (SuperGrok / X Premium+).\n"+
		"Open this URL in any browser and approve access:\n\n  %s\n\n", verifyURL)
	if da.VerificationURIComplete == "" {
		fmt.Fprintf(os.Stderr, "Code: %s\n\n", da.UserCode)
	} else if da.UserCode != "" {
		fmt.Fprintf(os.Stderr, "Confirm the code matches: %s\n\n", da.UserCode)
	}
	if err := openBrowser(verifyURL); err == nil {
		fmt.Fprintln(os.Stderr, "(opened in your browser)")
	}
	fmt.Fprintln(os.Stderr, "Waiting for approval...")

	tok, err := cfg.DeviceAccessToken(ctx, da)
	if err != nil {
		return Record{}, fmt.Errorf("waiting for xai authorization: %w", err)
	}
	return Record{Kind: KindOAuth, Token: tok, TokenURL: tokenURL}, nil
}

// stateOK enforces CSRF protection on the callback. The browser always echoes
// the state we sent, so the HTTP path must match it exactly - an empty/absent
// state there is a forged callback (login CSRF / token fixation). The
// user-initiated stdin paste is trusted: a pasted bare code carries no state, but
// a pasted full URL bearing a state must still match.
func stateOK(res callbackResult, expected string) bool {
	if res.viaPaste {
		return res.state == "" || res.state == expected
	}
	return res.state == expected
}

// accountIDFromIDToken extracts chatgpt_account_id from the id_token's
// "https://api.openai.com/auth" claim. It returns "" on any decode failure.
func accountIDFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Auth.ChatGPTAccountID
}

// ---- loopback callback ----
//
// This parallels internal/mcp/oauth.go's callback but is kept separate on
// purpose: the ChatGPT flow binds the pre-registered fixed redirect
// (localhost:1455, no ephemeral fallback), adds a stdin-paste fallback for
// headless use, and enforces a stricter CSRF rule for the HTTP path. The MCP
// flow uses an ephemeral port with a listener-derived redirect for dynamic
// client registration. The security models differ, so they are not shared.

type callbackResult struct {
	code, state, err string
	viaPaste         bool // delivered by the stdin-paste fallback, not the HTTP callback
}

type callbackServer struct {
	srv     *http.Server
	results chan callbackResult
}

const callbackHTML = `<!doctype html><meta charset=utf-8><title>aigem</title>
<body style="font-family:sans-serif;text-align:center;margin-top:4rem">
<h2>Authorization complete</h2><p>You can close this tab and return to aigem.</p></body>`

// startCallback binds the loopback redirect from the registered redirect URL.
// The port is fixed (1455) because the public client pre-registers it; if it is
// busy the flow cannot complete and a clear error is returned.
func startCallback(redirect, expectState string, allowStdinPaste bool) (*callbackServer, error) {
	u, err := url.Parse(redirect)
	if err != nil {
		return nil, fmt.Errorf("bad redirect %q: %w", redirect, err)
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	// Both families or neither. "localhost" resolves to one or the other
	// depending on the browser and the host, so proceeding on a partial bind
	// leaves whoever holds the other one to receive the authorization code -
	// silently, while this flow waits out its timeout. PKCE keeps the code from
	// being redeemed, but a squatted 1455 is exactly the condition to refuse on.
	var listeners []net.Listener
	var bindErrs []string
	for _, addr := range []string{"127.0.0.1:" + u.Port(), "[::1]:" + u.Port()} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			bindErrs = append(bindErrs, addr+": "+err.Error())
			continue
		}
		listeners = append(listeners, ln)
	}
	if len(bindErrs) > 0 {
		for _, ln := range listeners {
			_ = ln.Close()
		}
		return nil, fmt.Errorf("bind callback %s: %s (is another login in progress?)",
			u.Host, strings.Join(bindErrs, "; "))
	}
	cs := &callbackServer{results: make(chan callbackResult, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code") == "" && q.Get("error") == "" {
			http.Error(w, "missing authorization code", http.StatusBadRequest)
			return
		}
		// The state is checked here, not after the wait. This endpoint is a
		// fixed, well-known loopback URL that any page in any other tab can hit
		// with an <img> - no token, no CORS, nothing to stop it. Latching the
		// first arrival and checking afterwards meant one such request killed
		// every login attempt with a state mismatch, indefinitely.
		if expectState != "" && q.Get("state") != expectState {
			http.Error(w, "unexpected authorization state", http.StatusBadRequest)
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
	for _, ln := range listeners {
		go func(ln net.Listener) { _ = cs.srv.Serve(ln) }(ln)
	}
	// Reading os.Stdin would steal input from the Bubble Tea TUI, so the paste
	// fallback is enabled only for the CLI login path.
	if allowStdinPaste {
		go cs.readPasted()
	}
	return cs, nil
}

// readPasted lets a headless user paste the redirected URL (or a bare code) on
// stdin when the browser cannot reach the loopback server (e.g. over SSH).
func (cs *callbackServer) readPasted() {
	buf := make([]byte, 4096)
	n, _ := os.Stdin.Read(buf)
	cs.paste(string(buf[:n]))
}

// paste delivers a redirect URL or bare code the user brought back by hand,
// from wherever they typed it: a terminal reading stdin, or a browser field on
// a phone that could never have reached this machine's loopback. It reports
// whether anything usable was found. The delivered result is marked as pasted,
// which is what the CSRF rule keys off - a bare code carries no state, and one
// that does carry a state must still match.
func (cs *callbackServer) paste(raw string) bool {
	line := strings.TrimSpace(raw)
	if line == "" {
		return false
	}
	res := callbackResult{code: line, viaPaste: true}
	if u, err := url.Parse(line); err == nil && u.Query().Get("code") != "" {
		res = callbackResult{code: u.Query().Get("code"), state: u.Query().Get("state"), viaPaste: true}
	}
	select {
	case cs.results <- res:
		return true
	default:
		return false // something already answered this login
	}
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

// randState returns a cryptographically random CSRF state value; an entropy
// failure is propagated so a predictable (zero) state is never used.
func randState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// openBrowser launches the platform browser at u.
func openBrowser(u string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{u}
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", u}
	default:
		cmd, args = "xdg-open", []string{u}
	}
	return exec.Command(cmd, args...).Start()
}
