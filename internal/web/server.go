// Package web serves the browser front-end and the protocol behind it. The
// daemon owns the sessions rather than any one client: a turn started on a
// laptop has to still be running when a phone attaches to it ten minutes later,
// which a session living in a terminal process cannot do.
package web

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sort"
	"strconv"
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
}

// Server is the daemon.
type Server struct {
	token        string
	allowedHosts []string
	factory      Factory
	assets       http.Handler
	maxSessions  int
	mux          *http.ServeMux

	ln   net.Listener
	http *http.Server

	models  *llm.Registry
	backend *llm.Ref

	mu       sync.Mutex
	sessions map[string]*entry
	seq      int
	flows    map[string]*loginFlow
	flowSeq  int
	closed   bool
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
	if cfg.Factory == nil {
		return nil, errors.New("web: no session factory")
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
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		token:       token,
		factory:     cfg.Factory,
		assets:      cfg.Assets,
		maxSessions: cfg.MaxSessions,
		mux:         http.NewServeMux(),
		ln:          ln,
		sessions:    map[string]*entry{},
		flows:       map[string]*loginFlow{},
		models:      cfg.Models,
		backend:     cfg.Backend,
	}
	s.allowedHosts = hostsFor(ln.Addr())
	s.routes()
	s.http = &http.Server{Handler: s.mux, ReadHeaderTimeout: 10 * time.Second}
	return s, nil
}

// hostsFor lists the Host values this daemon answers to: the address it is
// bound to, and the loopback names that resolve to it. Anything else is a
// request that reached us under a name we did not claim, which is what a DNS
// rebinding attack looks like.
func hostsFor(addr net.Addr) []string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil
	}
	out := []string{net.JoinHostPort(host, port)}
	if host == "127.0.0.1" || host == "::1" || host == "0.0.0.0" || host == "::" {
		for _, h := range []string{"127.0.0.1", "[::1]", "localhost"} {
			out = append(out, h+":"+port)
		}
	}
	return out
}

// Addr is the bound address.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Token is the credential every request must carry.
func (s *Server) Token() string { return s.token }

// URL is what a person opens: the address with the token already in it, in the
// style of jupyter, so there is no separate step of pasting a secret.
func (s *Server) URL() string {
	return "http://" + s.ln.Addr().String() + "/?token=" + s.token
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
	s.mux.HandleFunc("GET /api/sessions", s.handleList)
	s.mux.HandleFunc("POST /api/sessions", s.handleCreate)
	s.mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDelete)
	s.mux.HandleFunc("GET /api/sessions/{id}/events", s.handleEvents)
	s.mux.HandleFunc("GET /api/sessions/{id}/blobs/{seq}", s.handleBlob)
	s.mux.HandleFunc("GET /api/sessions/{id}/socket", s.handleSocket)
	s.loginRoutes()
	s.mux.HandleFunc("/", s.handleAssets)
}

// handleAssets serves the built UI, or explains its absence. A plain `go build`
// produces a binary with no assets on purpose - `go install` is the documented
// way to get aigem and must keep working without a node toolchain - so this
// says what to do rather than serving a blank page.
func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	if s.assets == nil {
		http.Error(w, "this build has no web UI (built without `make web`).\n"+
			"Download a release binary, or run `make web && make build`.\n",
			http.StatusNotImplemented)
		return
	}
	s.assets.ServeHTTP(w, r)
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
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status is already written by now, so there is nowhere to report
		// this but the connection, which the client will see as a short read.
		_ = err
	}
}
