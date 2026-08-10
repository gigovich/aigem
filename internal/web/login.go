package web

import (
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/llm"
)

// Logging in through the browser is the point at which this endpoint stops
// being "an agent with a shell" and becomes "an agent with a shell and the
// credential store". Nothing new is opened up for it - the token and the origin
// check were there from the first commit - but it is why they were.

// flowView is a login in progress as the API reports it. No token, no code
// exchange, nothing that would be a credential: what a client needs is where to
// send the user and whether it worked.
type flowView struct {
	ID string `json:"id"`
	// URL is what the user opens; Code is the device code to confirm alongside
	// it, when the flow has one.
	URL      string `json:"url"`
	Code     string `json:"code,omitempty"`
	Provider string `json:"provider"`
	// Paste says the redirect cannot be relied on to come back to this machine,
	// so the page should offer somewhere to put it.
	Paste  bool   `json:"paste"`
	State  string `json:"state"`
	Error  string `json:"error,omitempty"`
	Expiry string `json:"expiry,omitempty"`
}

type loginFlow struct {
	id      string
	flow    *auth.Flow
	started time.Time
}

func (s *Server) loginRoutes() {
	s.mux.HandleFunc("GET /api/models", s.handleModels)
	s.mux.HandleFunc("POST /api/auth/login/{provider}", s.handleLoginBegin)
	s.mux.HandleFunc("GET /api/auth/login/{flow}", s.handleLoginStatus)
	s.mux.HandleFunc("POST /api/auth/login/{flow}/paste", s.handleLoginPaste)
	s.mux.HandleFunc("DELETE /api/auth/login/{flow}", s.handleLoginCancel)
}

// modelView is one entry in the model list, with whether it can actually be
// used - a list that does not say which models are reachable makes the user
// find out by picking one.
type modelView struct {
	Ref           string `json:"ref"`
	Provider      string `json:"provider"`
	Authenticated bool   `json:"authenticated"`
	NeedsAuth     bool   `json:"needs_auth"`
	Active        bool   `json:"active"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	if s.models == nil {
		writeJSON(w, []modelView{})
		return
	}
	active := ""
	if s.backend != nil {
		active = s.backend.Model().Ref()
	}
	out := []modelView{}
	for _, mi := range s.models.Models() {
		ref := mi.Ref()
		v := modelView{Ref: ref, Active: ref == active}
		if p, _, err := s.models.Resolve(ref); err == nil {
			v.Provider = p.ID
			v.NeedsAuth = p.NeedsAuth()
			v.Authenticated = !v.NeedsAuth || auth.IsAuthenticated(p.ID)
		}
		out = append(out, v)
	}
	writeJSON(w, out)
}

func (s *Server) handleLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	provider := r.PathValue("provider")
	// Only the providers with an interactive flow. An API key is not something
	// to walk a user through: it is pasted once, and `aigem auth login` is where
	// that belongs.
	if provider != llm.OpenAIProviderID && provider != llm.XAIProviderID {
		http.Error(w, provider+" has no browser login; add an API key with `aigem auth login "+
			provider+"`", http.StatusBadRequest)
		return
	}
	flow, err := auth.Begin(r.Context(), provider)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.mu.Lock()
	s.flowSeq++
	f := &loginFlow{id: "f-" + strconv.Itoa(s.flowSeq), flow: flow, started: time.Now()}
	s.flows[f.id] = f
	s.mu.Unlock()

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, view(f))
}

func view(f *loginFlow) flowView {
	state, err := f.flow.Status()
	v := flowView{
		ID: f.id, URL: f.flow.URL, Code: f.flow.Code, Provider: f.flow.Provider,
		Paste: f.flow.AcceptsPaste, State: string(state),
	}
	if err != nil {
		v.Error = err.Error()
	}
	return v
}

func (s *Server) lookupFlow(w http.ResponseWriter, r *http.Request) (*loginFlow, bool) {
	s.mu.Lock()
	f, ok := s.flows[r.PathValue("flow")]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return nil, false
	}
	return f, true
}

func (s *Server) handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	f, ok := s.lookupFlow(w, r)
	if !ok {
		return
	}
	writeJSON(w, view(f))
}

func (s *Server) handleLoginPaste(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	f, ok := s.lookupFlow(w, r)
	if !ok {
		return
	}
	// A redirect URL, not a document: anything longer than this is not one.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
	if err != nil {
		http.Error(w, "could not read the pasted value", http.StatusBadRequest)
		return
	}
	if err := f.flow.Paste(string(body)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, view(f))
}

func (s *Server) handleLoginCancel(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	f, ok := s.lookupFlow(w, r)
	if !ok {
		return
	}
	f.flow.Cancel()
	s.mu.Lock()
	delete(s.flows, f.id)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
