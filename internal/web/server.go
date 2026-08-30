// Package web serves the browser UI and the API behind it.
//
// The daemon binds loopback unless it is told the public origin it will be
// reached under. That is what an origin check can be made of, and nothing in a
// request can supply it - X-Forwarded-Host is written by whoever is talking to
// us - so the operator states it with --origin, or the bind is refused. Putting
// `tailscale serve` or another reverse proxy in front of a loopback daemon
// stays the shape that needs no flag at all.
package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gigovich/aigem/internal/store"
)

// Config configures the daemon.
type Config struct {
	// Addr is the listen address. It defaults to a loopback port chosen by the
	// kernel, and a non-loopback address is refused unless Origins says which
	// name the daemon answers to.
	Addr string
	// Origins are the public origins this daemon is reached at, scheme and all
	// ("https://aigem.example.ts.net"). Behind a reverse proxy the bind address
	// is not the name requests arrive under, so the operator states it here.
	Origins []string
	// Token authenticates every request except the page itself and /healthz.
	// One is generated when empty.
	Token string
	// CookieFile is where the browser sessions are kept across restarts. Empty
	// keeps them in memory, so a restart signs every browser out.
	CookieFile string
	// Assets serves the built UI. A daemon without one still serves /healthz and
	// answers every page with a 501 that says which build step is missing.
	Assets http.Handler
}

// Server is a running daemon.
type Server struct {
	token   string
	allowed allowlist
	assets  http.Handler
	hasUI   bool
	ln      net.Listener
	http    *http.Server

	failures *limiter
	sockets  sockets

	mu sync.Mutex
	// cookies are the live browser sessions and when each expires. They are
	// written to cookieStore on every change when there is one, and held in
	// memory alone when there is not.
	cookies map[string]time.Time
	// cookieGen numbers the snapshots handed to persist, which writes them in
	// order and drops one a newer write has overtaken. See pendingLocked.
	cookieGen   uint64
	cookieStore *store.File[cookieFile]
	closed      bool

	// saveMu serializes the cookie file itself, held across the disk write that
	// s.mu deliberately is not.
	saveMu   sync.Mutex
	savedGen uint64
}

// New binds the listener and builds the routes. The daemon is not serving until
// Serve is called, but Addr, Token and URL are usable as soon as this returns,
// so the caller can print the link before the first request arrives.
func New(cfg Config) (*Server, error) {
	addr := cfg.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	token := cfg.Token
	if token == "" {
		t, err := newToken()
		if err != nil {
			return nil, fmt.Errorf("web: could not generate a token: %w", err)
		}
		token = t
	}
	// Before the listener, so a refusal to serve does not leave a port bound.
	if err := checkBind(addr, cfg.Origins); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("web: listen on %s: %w", addr, err)
	}
	// checkBind reads the address as written; this reads what the kernel
	// actually gave us. "localhost" goes through the system resolver, and a host
	// whose resolver answers with a routable address would otherwise pass the
	// string test and bind where the network can reach.
	if err := checkBound(ln.Addr(), cfg.Origins); err != nil {
		_ = ln.Close()
		return nil, err
	}
	allowed, err := hostsFor(ln.Addr(), cfg.Origins)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}

	// hasUI is read before the fallback below replaces a nil handler, so it
	// reports whether this daemon was given a UI rather than whether it has
	// something to answer with - which is always.
	cookies := cookieStoreFor(cfg.CookieFile)
	s := &Server{
		token:       token,
		allowed:     allowed,
		assets:      cfg.Assets,
		hasUI:       cfg.Assets != nil,
		failures:    newLimiter(),
		cookies:     loadCookies(cookies),
		cookieStore: cookies,
	}
	if s.assets == nil {
		s.assets = noAssets()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	// The catch-all below matches every method, so the mux never reports a method
	// mismatch on its own. A more specific pattern has to say so.
	mux.HandleFunc("/healthz", methodNotAllowed("GET, HEAD"))
	// The exchange is guarded like every other route, so it accepts either
	// credential: a page with a cookie about to lapse renews it the same way it
	// got one, and a page with only the URL token gets its first.
	mux.HandleFunc("POST /api/auth/session", s.Guard(s.handleAuthSession))
	mux.HandleFunc("DELETE /api/auth/session", s.Guard(s.handleAuthLogout))
	mux.HandleFunc("/api/auth/session", methodNotAllowed("POST, DELETE"))
	mux.Handle("/", s.assets)

	s.ln = ln
	s.http = &http.Server{
		// One wrapper rather than a call per handler: the page and the bundle are
		// the responses the policy exists for, and they are served by
		// http.FileServerFS, which will never call anything of ours. Every route
		// added later is covered by construction.
		Handler: withSecurityHeaders(mux),
		// net/http answers "OPTIONS *" itself, without ever calling Handler, so
		// that one response would leave without the policy.
		DisableGeneralOptionsHandler: true,
		// A websocket hijacks the connection before any of these apply, so they
		// bound the plain HTTP surface only. ReadHeaderTimeout is the one that
		// matters here: without it a connection that opens and says nothing
		// holds a slot indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s, nil
}

// Serve runs until Close. A closed server is not an error.
func (s *Server) Serve() error {
	err := s.http.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops the daemon. Calling it twice is safe.
func (s *Server) Close() error {
	s.mu.Lock()
	// The table goes with the process. The file is deliberately left alone:
	// stopping the daemon is not a revocation when it has one, which is the
	// whole point of it having one.
	s.closed = true
	s.cookies = map[string]time.Time{}
	s.mu.Unlock()

	err := s.http.Close()
	// http.Server only knows about listeners Serve registered, so a Server that
	// was built and then abandoned - an error on a later step, a test that never
	// serves - would otherwise hold the port for the life of the process.
	if lerr := s.ln.Close(); lerr != nil && err == nil && !errors.Is(lerr, net.ErrClosed) {
		err = lerr
	}
	return err
}

// Addr is the bound address, with the port the kernel chose when none was given.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Token is the credential every request must carry.
func (s *Server) Token() string { return s.token }

// SignInURL is what a person opens: the address with the token already in it,
// in the style of jupyter, so there is no separate step of pasting a secret.
// The page trades it for a cookie on its first load and takes it back out of
// the address bar, so it is a secret in transit rather than one in browser
// history.
//
// The name is the warning. It returns a credential in a string, so a caller
// that prints it is publishing the token - which the terminal that started the
// daemon is entitled to, and a log file, a status output or an error message is
// not. Everything that is not signing a person in wants Base.
//
// A configured origin wins over the bind address. Behind a proxy the daemon is
// bound to a loopback port nobody outside the machine can reach, and printing
// that is printing a link that does not work.
func (s *Server) SignInURL() string { return s.Base() + "?token=" + s.token }

// Base is the daemon's address with no credential in it.
func (s *Server) Base() string {
	base := "http://" + s.ln.Addr().String()
	if len(s.allowed.public) > 0 {
		base = s.allowed.public[0]
	}
	return base + "/"
}

// handleHealth answers without a credential: it is a liveness probe, and the
// caller that needs it most is the one that has not signed in yet. It reports
// nothing an unauthenticated caller could not learn by asking for the page.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// This server's own state, not the binary's: a caller can build one with no
	// assets out of a binary that has them, and answering from the embedded FS
	// would promise a UI that this daemon serves 501 for.
	writeJSON(w, map[string]any{"ok": true, "ui": s.hasUI})
}

// allowlist is what this daemon answers to. The two lists are checked against
// two different headers and are not interchangeable: Host says which name the
// request arrived under, and Origin says which page sent it. A rebinding attack
// supplies an attacker's name in both, so both are matched exactly.
type allowlist struct {
	// hosts are accepted Host values, "host:port" as Go writes them.
	hosts []string
	// origins are accepted Origin values, scheme and all. A default port is
	// absent, because that is how a browser writes one.
	origins []string
	// public are the configured origins alone, in the order given, without the
	// loopback entries. The first is the address worth printing.
	public []string
}

// checkBind refuses an address the network can reach without being told the
// name it will be reached under.
//
// Serving anyway is worse than not serving: every allowlist the daemon could
// derive on its own names the bind address, a request from a phone arrives with
// the proxy's name, and the 403 that follows reads as a broken server rather
// than as a missing flag.
func checkBind(addr string, origins []string) error {
	if len(origins) > 0 {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("web: listen address %q: %w", addr, err)
	}
	if isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("web: refusing to serve on %s without --origin: an address the "+
		"network can reach needs the public URL it is reached at (for example "+
		"--origin https://aigem.example.ts.net), because that is the only thing an "+
		"origin check can be made of; the alternative is a loopback bind with "+
		"`tailscale serve` or another reverse proxy in front of it", addr)
}

// checkBound is the guarantee checkBind can only approximate: whatever the
// address resolved to, a daemon with no stated origin has to be on a loopback
// interface.
func checkBound(addr net.Addr, origins []string) error {
	if len(origins) > 0 {
		return nil
	}
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("web: refusing to serve on %s: it is not a TCP address, "+
			"and the loopback rule cannot be checked against it", addr)
	}
	if tcp.IP.IsLoopback() {
		return nil
	}
	return fmt.Errorf("web: refusing to serve on %s: it resolved to an address the "+
		"network can reach; without --origin the daemon binds loopback only", addr)
}

// hostsFor builds the allowlist. Configured origins REPLACE the derived list
// rather than extending it: behind a proxy the bind address is not the name we
// are reached under, and leaving it allowed only widens what a rebinding attack
// may claim to be.
//
// The loopback names survive that replacement, and so do their origins. The
// hosts are what lets curl keep working against a proxied daemon; the origins
// are what lets the operator open the same daemon in a browser on the machine
// itself. Neither weakens anything - a page on an attacker's host can send its
// own Origin, and it will never be one of these.
func hostsFor(addr net.Addr, origins []string) (allowlist, error) {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return allowlist{}, fmt.Errorf("web: bound address %q: %w", addr, err)
	}
	var out allowlist
	for _, o := range origins {
		norm, err := normalizeOrigin(o)
		if err != nil {
			return allowlist{}, err
		}
		u, err := url.Parse(norm)
		if err != nil {
			return allowlist{}, fmt.Errorf("web: origin %q: %w", o, err)
		}
		out.origins = append(out.origins, norm)
		out.public = append(out.public, norm)
		out.hosts = append(out.hosts, u.Host)
	}

	var local []string
	if len(origins) == 0 {
		// Nothing was stated, so the address this socket is on is the only name
		// there is - and checkBind has already refused anything but loopback.
		// With an origin stated it is deliberately left out: a daemon bound to a
		// LAN address and declared https-only must not also answer to
		// http://192.168.1.5:7777, which is what keeping it here used to do.
		local = append(local, net.JoinHostPort(host, port))
	}
	// The loopback names survive a stated origin - they are what lets curl on the
	// machine keep working against a proxied daemon, and they are names no page
	// on an attacker's host can send. Only the ones this socket actually answers
	// on, though: allowlisting [::1] on a daemon bound to 127.0.0.1 alone names
	// an origin any other local process is free to serve.
	switch {
	case host == "0.0.0.0" || host == "::":
		local = append(local, "127.0.0.1:"+port, "[::1]:"+port, "localhost:"+port)
	case isLoopbackHost(host):
		local = append(local, net.JoinHostPort(host, port), "localhost:"+port)
	}
	for _, h := range local {
		if !slices.Contains(out.hosts, h) {
			out.hosts = append(out.hosts, h)
		}
		// Only http: nothing terminates TLS on the loopback interface here, and
		// an https origin for it would be one this daemon can never serve.
		if o := "http://" + h; !slices.Contains(out.origins, o) {
			out.origins = append(out.origins, o)
		}
	}
	return out, nil
}

// normalizeOrigin turns what the operator typed into the exact string a browser
// puts in the Origin header, and refuses anything that is not one.
//
// A URL with a path is the common mistake, and accepting it would mean an
// operator who pasted the address bar gets an allowlist entry that never
// matches - the failure this whole flag exists to stop being mysterious.
func normalizeOrigin(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("web: origin %q: %w", raw, err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("web: origin %q needs an http:// or https:// scheme", raw)
	case u.Host == "":
		return "", fmt.Errorf("web: origin %q names no host", raw)
	case u.User != nil:
		return "", fmt.Errorf("web: origin %q must not carry credentials", raw)
	case strings.HasSuffix(u.Host, ":"):
		return "", fmt.Errorf("web: origin %q ends in a colon with no port", raw)
	case u.Path != "" && u.Path != "/", u.RawQuery != "", u.Fragment != "":
		return "", fmt.Errorf("web: origin %q must be a scheme and a host and nothing else "+
			"(for example https://aigem.example.ts.net)", raw)
	}
	// Lowercased, and without the default port: that is how a browser writes an
	// Origin, and an allowlist entry spelled any other way is one no request can
	// ever match - a 403 that reads as a broken server, which is the whole
	// failure this flag exists to prevent.
	host := strings.ToLower(u.Host)
	if p := u.Port(); (u.Scheme == "http" && p == "80") || (u.Scheme == "https" && p == "443") {
		host = strings.ToLower(u.Hostname())
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	}
	// A browser sends an internationalised name in its punycode form, so an
	// allowlist entry holding the unicode one matches nothing, ever. Converting
	// here would mean depending on golang.org/x/net/idna for one flag; saying so
	// costs the operator one lookup and cannot be wrong.
	for _, r := range host {
		if r > unicode.MaxASCII {
			return "", fmt.Errorf("web: origin %q is not ASCII: give it in the punycode "+
				"form a browser sends (xn--...), or nothing will ever match it", raw)
		}
	}
	return u.Scheme + "://" + host, nil
}

// normalizeHost puts a Host header into the spelling the allowlist holds. A
// hostname is case-insensitive to the resolver and may carry a trailing root
// dot, and some clients send both; refusing either would be a 403 that reads as
// a broken server, which is the failure the allowlist exists to avoid.
func normalizeHost(raw string) string {
	h := strings.ToLower(strings.TrimSpace(raw))
	host, port, err := net.SplitHostPort(h)
	if err != nil {
		return strings.TrimSuffix(h, ".")
	}
	return net.JoinHostPort(strings.TrimSuffix(host, "."), port)
}

func methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// isLoopbackHost reports whether a bind host keeps the daemon off the network.
// The wildcard addresses are deliberately not loopback: binding one is how the
// daemon becomes reachable from the LAN in the first place.
func isLoopbackHost(host string) bool {
	// Hostnames are case-insensitive to the resolver, so refusing "Localhost"
	// would be a fail-closed wart rather than a control.
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// withSecurityHeaders applies the policy to every response, including the ones
// served straight out of the embedded filesystem.
func withSecurityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w.Header())
		h.ServeHTTP(w, r)
	})
}

func securityHeaders(h http.Header) {
	// The agent reads pages an attacker may have written and the UI renders model
	// output, so this is not defence in depth - it is what stops injected markup
	// from reaching anywhere. img-src and form-action are the load-bearing ones:
	// an <img> to an outside host and a <form> posting to one are both
	// exfiltration with no script involved. form-action does not fall back to
	// default-src, so it has to be named.
	h.Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data:; connect-src 'self'; "+
			"form-action 'none'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// The status is already written by now, so a failure here has nowhere to go
	// but the connection, which the client sees as a short read.
	_ = json.NewEncoder(w).Encode(v)
}
