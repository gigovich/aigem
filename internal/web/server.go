// Package web serves the browser UI and the API behind it.
//
// The daemon binds loopback only. Reaching it from another device is a separate
// decision made outside this process - `tailscale serve` in front of it is the
// supported shape - because an address the network can reach needs an origin
// check, and an origin check needs the public URL stated by a person rather
// than read out of a request header anyone can write.
package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Config configures the daemon.
type Config struct {
	// Addr is the listen address. It defaults to a loopback port chosen by the
	// kernel, and a non-loopback address is refused.
	Addr string
	// Assets serves the built UI. A daemon without one still serves /healthz and
	// answers every page with a 501 that says which build step is missing.
	Assets http.Handler
}

// Server is a running daemon.
type Server struct {
	assets http.Handler
	hasUI  bool
	ln     net.Listener
	http   *http.Server
}

// New binds the listener and builds the routes. The daemon is not serving until
// Serve is called, but Addr and URL are usable as soon as this returns, so the
// caller can print the link before the first request arrives.
func New(cfg Config) (*Server, error) {
	addr := cfg.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if err := checkBind(addr); err != nil {
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
	if err := checkBound(ln.Addr()); err != nil {
		_ = ln.Close()
		return nil, err
	}

	// hasUI is read before the fallback below replaces a nil handler, so it
	// reports whether this daemon was given a UI rather than whether it has
	// something to answer with - which is always.
	s := &Server{assets: cfg.Assets, hasUI: cfg.Assets != nil}
	if s.assets == nil {
		s.assets = noAssets()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	// The catch-all below matches every method, so the mux never reports a method
	// mismatch on its own. A more specific pattern has to say so.
	mux.HandleFunc("/healthz", methodNotAllowed("GET, HEAD"))
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

// URL is what a person opens.
func (s *Server) URL() string { return "http://" + s.ln.Addr().String() + "/" }

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// This server's own state, not the binary's: a caller can build one with no
	// assets out of a binary that has them, and answering from the embedded FS
	// would promise a UI that this daemon serves 501 for.
	writeJSON(w, map[string]any{"ok": true, "ui": s.hasUI})
}

// checkBind refuses an address the network can reach. Serving one means
// answering requests whose origin nothing in this process can verify, and the
// machinery that would verify it - a stated public origin and a token check -
// does not exist yet. Refusing is the honest answer until it does.
func checkBind(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("web: listen address %q: %w", addr, err)
	}
	if isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("web: refusing to serve on %s: the daemon binds loopback only; "+
		"put `tailscale serve` or another reverse proxy in front of it to reach it "+
		"from elsewhere", addr)
}

// checkBound is the guarantee checkBind can only approximate: whatever the
// address resolved to, the socket has to be on a loopback interface.
func checkBound(addr net.Addr) error {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("web: refusing to serve on %s: it is not a TCP address, "+
			"and the loopback rule cannot be checked against it", addr)
	}
	if tcp.IP.IsLoopback() {
		return nil
	}
	return fmt.Errorf("web: refusing to serve on %s: it resolved to an address the "+
		"network can reach; the daemon binds loopback only", addr)
}

func methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

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
