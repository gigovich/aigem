// Package web serves the browser front-end and the protocol behind it. The
// daemon owns the sessions rather than any one client: a turn started on a
// laptop has to still be running when a phone attaches to it ten minutes later,
// which a session living in a terminal process cannot do.
package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/uisession"
)

// Factory builds a session for a new conversation. It is injected rather than
// built here so this package stays out of model resolution, tool registries and
// system prompts - all of which the command that starts the daemon already
// assembles.
type Factory func(spec Spec) (*uisession.Local, error)

// Spec describes a session to create.
type Spec struct {
	Cwd     string `json:"cwd,omitempty"`
	Model   string `json:"model,omitempty"`
	Profile string `json:"profile,omitempty"`
}

// Config describes the daemon.
type Config struct {
	// Addr is the listen address. It defaults to loopback; see auth.go for why
	// anything else is a separate decision.
	Addr string
	// Origins are the public origins this daemon is reached at, scheme and all
	// ("https://aigem.example.ts.net"). Behind a reverse proxy the bind address
	// is not the name requests arrive under, and nothing in a request can be
	// trusted to supply it - X-Forwarded-Host is written by whoever is talking
	// to us. So the operator states it, and a non-loopback bind without one is
	// refused rather than served behind a check nobody can pass.
	Origins []string
	// Token authenticates every request. One is generated when empty.
	Token   string
	Factory Factory
	// Assets serves the built UI. A build without one still runs the protocol,
	// which is what the daemon is tested against.
	Assets http.Handler
	// Models and Backend let the daemon report which models exist and which are
	// reachable, and say which one is in use.
	Models  *llm.Registry
	Backend *llm.Ref
	// MaxSessions bounds how many conversations this daemon holds at once; zero
	// is unlimited. It exists because sessions sharing one tool registry are not
	// safely independent - see the comment on handleCreate.
	MaxSessions int
	// Mount lets another package add routes to this daemon. It is called once
	// with the mux and a wrapper that applies the origin check, the token check
	// and the security headers, so a mounted API cannot answer under weaker
	// rules than the ones here. The bot fleet's chat API arrives this way: one
	// listener, one token and one CSP rather than two servers with two answers
	// about who may connect.
	//
	// Routes are mounted before the "/" asset handler, so a mounted pattern wins
	// over the catch-all.
	Mount func(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc)
}

// Server is the daemon.
type Server struct {
	token       string
	allowed     allowlist
	factory     Factory
	assets      http.Handler
	maxSessions int
	mount       func(*http.ServeMux, func(http.HandlerFunc) http.HandlerFunc)
	mux         *http.ServeMux

	ln   net.Listener
	http *http.Server

	models  *llm.Registry
	backend *llm.Ref

	failures *limiter
	sockets  sockets

	mu       sync.Mutex
	sessions map[string]*entry
	// cookies are the live browser sessions and when each expires. In memory
	// on purpose: a restart revokes every one, and reissuing is one request.
	cookies map[string]time.Time
	seq     int
	flows   map[string]*loginFlow
	flowSeq int
	closed  bool
}

type entry struct {
	id      string
	spec    Spec
	started time.Time
	sess    *uisession.Local
}

// New builds the daemon and binds its listener, so the address and token are
// known before it starts serving and can be printed or written to the state
// file without a race.
func New(cfg Config) (*Server, error) {
	// A daemon needs something to serve, but not necessarily sessions: the bot
	// fleet hosts the chat API through Mount and creates no conversations.
	if cfg.Factory == nil && cfg.Mount == nil {
		return nil, errors.New("web: no session factory and nothing mounted")
	}
	addr := cfg.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	token := cfg.Token
	if token == "" {
		t, err := newToken()
		if err != nil {
			return nil, err
		}
		token = t
	}
	// Before the listener, so a refusal to serve does not leave a port bound.
	if err := checkBind(addr, cfg.Origins); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	allowed, err := hostsFor(ln.Addr(), cfg.Origins)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	s := &Server{
		token:       token,
		factory:     cfg.Factory,
		assets:      cfg.Assets,
		maxSessions: cfg.MaxSessions,
		mount:       cfg.Mount,
		mux:         http.NewServeMux(),
		ln:          ln,
		sessions:    map[string]*entry{},
		flows:       map[string]*loginFlow{},
		cookies:     map[string]time.Time{},
		failures:    newLimiter(),
		models:      cfg.Models,
		backend:     cfg.Backend,
	}
	s.allowed = allowed
	s.routes()
	s.http = &http.Server{Handler: s.mux, ReadHeaderTimeout: 10 * time.Second}
	return s, nil
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

// checkBind refuses to serve on an address the network can reach without being
// told the name it will be reached under.
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
	return fmt.Errorf("web: refusing to serve on %s without --origin: "+
		"an address the network can reach needs the public URL it is reached at "+
		"(for example --origin https://aigem.example.ts.net), because that is the "+
		"only thing an origin check can be made of", addr)
}

// isLoopbackHost reports whether a bind host keeps the daemon off the network.
// The wildcard addresses are deliberately not loopback: binding one is how the
// daemon becomes reachable from the LAN in the first place.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// hostsFor builds the allowlist. Configured origins REPLACE the derived list
// rather than extending it: behind a proxy the bind address is not the name we
// are reached under, and leaving it allowed only widens what a rebinding attack
// may claim to be.
//
// The loopback names survive that replacement, and so do their origins. The
// hosts are what lets curl and `aigem chat` keep working against a proxied
// daemon; the origins are what lets the operator open the same daemon in a
// browser on the machine itself. Neither weakens anything - a page on an
// attacker's host can send its own Origin, and it will never be one of these.
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
		out.origins = append(out.origins, norm)
		out.public = append(out.public, norm)
		u, _ := url.Parse(norm)
		out.hosts = append(out.hosts, u.Host)
	}

	local := []string{net.JoinHostPort(host, port)}
	if isLoopbackHost(host) || host == "0.0.0.0" || host == "::" {
		for _, h := range []string{"127.0.0.1", "[::1]", "localhost"} {
			local = append(local, h+":"+port)
		}
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
// matches - the failure this whole change exists to stop being mysterious.
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
	return u.Scheme + "://" + host, nil
}

// Addr is the bound address.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Token is the credential every request must carry.
func (s *Server) Token() string { return s.token }

// URL is what a person opens: the address with the token already in it, in the
// style of jupyter, so there is no separate step of pasting a secret.
//
// A configured origin wins over the bind address. Behind a proxy the daemon is
// bound to a loopback port nobody outside the machine can reach, and printing
// that is printing a link that does not work.
func (s *Server) URL() string {
	base := "http://" + s.ln.Addr().String()
	if len(s.allowed.public) > 0 {
		base = s.allowed.public[0]
	}
	return base + "/?token=" + s.token
}

// Serve runs until Close.
func (s *Server) Serve() error {
	err := s.http.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops serving and ends every session, so nothing is left parked on an
// approval no one can answer.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	all := make([]*entry, 0, len(s.sessions))
	for _, e := range s.sessions {
		all = append(all, e)
	}
	s.sessions = map[string]*entry{}
	flows := s.flows
	s.flows = map[string]*loginFlow{}
	// Every browser session goes with the process that issued it. They are in
	// memory precisely so that a restart is a revocation.
	s.cookies = map[string]time.Time{}
	s.mu.Unlock()

	for _, f := range flows {
		f.flow.Cancel()
	}
	for _, e := range all {
		_ = e.sess.Save()
		e.sess.Close()
	}
	return s.http.Close()
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	// Through Guard rather than the bare check, so a refusal carries the
	// security headers too - which is the whole reason Guard sets them first.
	s.mux.HandleFunc("GET /api/modes", s.Guard(s.handleModes))
	// The exchange is guarded like everything else, so it accepts either
	// credential: a page with a cookie about to lapse renews it the same way it
	// got one, and a page with only the URL token gets its first.
	s.mux.HandleFunc("POST /api/auth/session", s.Guard(s.handleAuthSession))
	s.mux.HandleFunc("DELETE /api/auth/session", s.Guard(s.handleAuthLogout))
	// Without a factory there are no conversations to list, create or attach to,
	// and handleCreate would have nothing to call. Leaving the routes off is
	// what makes a mount-only daemon answer 404 there instead of panicking.
	if s.factory != nil {
		s.mux.HandleFunc("GET /api/sessions", s.handleList)
		s.mux.HandleFunc("POST /api/sessions", s.handleCreate)
		s.mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDelete)
		s.mux.HandleFunc("GET /api/sessions/{id}/events", s.handleEvents)
		s.mux.HandleFunc("GET /api/sessions/{id}/blobs/{seq}", s.handleBlob)
		s.mux.HandleFunc("GET /api/sessions/{id}/socket", s.Guard(s.handleSocket))
		s.artifactRoutes()
	}
	s.loginRoutes()
	s.usageRoutes()
	if s.mount != nil {
		s.mount(s.mux, s.Guard)
	}
	s.mux.HandleFunc("/", s.handleAssets)
}

// handleAssets serves the built UI, or explains its absence. A plain `go build`
// produces a binary with no assets on purpose - `go install` is the documented
// way to get aigem and must keep working without a node toolchain - so this
// says what to do rather than serving a blank page.
// securityHeaders bounds what the page may load and where it may talk to.
//
// This is not defence in depth, it is the control that closes a live hole. The
// agent reads pages an attacker may have written, and the UI renders what the
// model writes back as HTML. A planted instruction to end a reply with an image
// tag pointing at the attacker's host would otherwise make the user's browser
// GET whatever the model encoded into the URL - conversation content, file
// contents, anything it had read. Sanitising the HTML does not help: the tag is
// legitimate. img-src is the load-bearing directive; the rest is cheap.
func securityHeaders(h http.Header) {
	h.Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data:; connect-src 'self'; "+
			"frame-ancestors 'none'; base-uri 'none'; object-src 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w.Header())
	if s.assets == nil {
		http.Error(w, "this build has no web UI (built without `make web`).\n"+
			"Download a release binary, or run `make web && make build`.\n",
			http.StatusNotImplemented)
		return
	}
	s.assets.ServeHTTP(w, r)
}

// Modes says which halves of the product a daemon serves.
type Modes struct {
	Sessions bool `json:"sessions"`
	Chat     bool `json:"chat"`
}

// handleModes reports them. One bundle serves both modes, and it must not offer
// a switch to one that answers 404: `aigem web run` has no fleet to talk to and
// `aigem bot start` creates no sessions. Asking the daemon is the only way the
// page can know which it is talking to, since both are served from this origin
// by the same binary.
func (s *Server) handleModes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, Modes{Sessions: s.factory != nil, Chat: s.mount != nil})
}

// ---- sessions ----

// View is a session as the API reports it.
type View struct {
	ID      string    `json:"id"`
	Title   string    `json:"title,omitempty"`
	Model   string    `json:"model,omitempty"`
	Cwd     string    `json:"cwd,omitempty"`
	Started time.Time `json:"started"`
	Running bool      `json:"running"`
	Seq     uint64    `json:"seq"`
}

func (e *entry) view() View {
	meta := e.sess.Meta()
	return View{
		ID: e.id, Title: meta.Title, Model: meta.Model, Cwd: e.spec.Cwd,
		Started: e.started, Running: e.sess.Running(), Seq: e.sess.Seq(),
	}
}

func (s *Server) lookup(id string) (*entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[id]
	return e, ok
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	s.mu.Lock()
	out := make([]View, 0, len(s.sessions))
	for _, e := range s.sessions {
		out = append(out, e.view())
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	writeJSON(w, out)
}

// handleCreate opens a conversation.
//
// MaxSessions is not a resource limit. Sessions built against one tool registry
// are not independent: registering the delegation tool binds it to the
// confirmation function of the session that registered it last, so a tool call
// in one conversation would ask a different conversation's clients for
// approval. Until a session can be given a registry of its own - which means
// resolving skills, project instructions and path grants per root - a caller
// that wants a second conversation gets a clear refusal instead of a
// cross-wired one.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	s.mu.Lock()
	full := s.maxSessions > 0 && len(s.sessions) >= s.maxSessions
	s.mu.Unlock()
	if full {
		http.Error(w, "this daemon holds one conversation at a time; "+
			"close the open one first", http.StatusConflict)
		return
	}
	var spec Spec
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&spec); err != nil {
			http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	sess, err := s.factory(spec)
	if err != nil {
		http.Error(w, "create session: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		sess.Close()
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	s.seq++
	// The daemon's handle for a conversation is not the conversation's own id:
	// that one is minted by its first turn, and a session has to be addressable
	// before it has had one.
	e := &entry{id: "s-" + strconv.Itoa(s.seq), spec: spec, started: time.Now(), sess: sess}
	s.sessions[e.id] = e
	s.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, e.view())
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	e, ok := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := e.sess.Save(); err != nil {
		// The conversation still ends; say so rather than failing the request,
		// which would leave the caller thinking it is still there.
		w.Header().Set("X-Aigem-Warning", "save failed: "+err.Error())
	}
	e.sess.Close()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	e, ok := s.lookup(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	evs, err := e.sess.Replay(sinceParam(r))
	if err != nil {
		// Not an error the client can retry around: it has to start over.
		http.Error(w, err.Error(), http.StatusGone)
		return
	}
	writeJSON(w, evs)
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	e, ok := s.lookup(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if err != nil {
		http.Error(w, "bad seq", http.StatusBadRequest)
		return
	}
	body, err := e.sess.Blob(seq)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	securityHeaders(w.Header())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

func sinceParam(r *http.Request) uint64 {
	n, err := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	if err != nil {
		return 0
	}
	return n
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
