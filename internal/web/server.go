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
	"sync"
	"time"
)

// Config configures the daemon.
type Config struct {
	// Addr is the listen address. It defaults to a loopback port chosen by the
	// kernel, and a non-loopback address is refused.
	Addr string
	// Assets serves the built UI. A build without one still runs the API, which
	// is what the daemon is tested against.
	Assets http.Handler
}

// Server is a running daemon.
type Server struct {
	assets http.Handler
	ln     net.Listener
	http   *http.Server

	mu     sync.Mutex
	closed bool
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

	s := &Server{assets: cfg.Assets}
	if s.assets == nil {
		s.assets = noAssets()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("/", s.assets)

	s.ln = ln
	s.http = &http.Server{
		Handler: mux,
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
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.http.Close()
}

// Addr is the bound address, with the port the kernel chose when none was given.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// URL is what a person opens.
func (s *Server) URL() string { return "http://" + s.ln.Addr().String() + "/" }

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "ui": HasAssets()})
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

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func securityHeaders(h http.Header) {
	// worker-src is named rather than left to fall back to default-src: the
	// fallback chain for it is worker-src, child-src, script-src, default-src,
	// so tightening script-src later would silently take the service worker
	// down with it.
	h.Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data:; connect-src 'self'; worker-src 'self'; "+
			"frame-ancestors 'none'; base-uri 'none'; object-src 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

func writeJSON(w http.ResponseWriter, v any) {
	securityHeaders(w.Header())
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status is already written by now, so there is nowhere to report
		// this but the connection, which the client will see as a short read.
		_ = err
	}
}
