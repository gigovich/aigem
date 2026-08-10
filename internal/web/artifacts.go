package web

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gigovich/aigem/internal/llm"
)

// What the browser can show that a terminal cannot: the whole of what a session
// changed, side by side, and what it has spent doing so. Both are read-only
// views over state the agent already keeps - nothing here is a new source of
// truth, which is why neither needs a new event.

// artifactView is one file a session touched.
type artifactView struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
	Old     string `json:"old,omitempty"`
	New     string `json:"new,omitempty"`
}

func (s *Server) artifactRoutes() {
	s.mux.HandleFunc("GET /api/sessions/{id}/artifacts", s.handleArtifacts)
	s.mux.HandleFunc("GET /api/usage", s.handleUsage)
}

// handleArtifacts lists the files a conversation changed, with the content
// before and after. The "before" is as it was when this session first touched
// the file, not before the last edit, so the diff is the session's whole effect
// - which is the question someone reviewing a run is actually asking.
//
// Content is included only when a path is named, because the list is opened far
// more often than any one diff is read and a session that rewrote a large tree
// would otherwise ship all of it to draw a filename.
func (s *Server) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	e, ok := s.lookup(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	want := r.URL.Query().Get("path")
	root := ""
	if e.spec.Cwd != "" {
		root = e.spec.Cwd
	}

	out := []artifactView{}
	for path, c := range e.sess.Artifacts() {
		v := artifactView{Path: relTo(root, path), Created: c.Created}
		if want != "" && want != v.Path {
			continue
		}
		if want != "" {
			v.Old, v.New = c.Old, c.New
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	writeJSON(w, out)
}

// relTo shortens an absolute path against the session's root. A path outside it
// is left absolute rather than turned into a trail of "..", which reads as an
// escape when it is really a file the user approved by name.
func relTo(root, path string) string {
	if root == "" {
		return path
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return filepath.ToSlash(rel)
}

// usageView is what the account has spent and how close it is to its limits.
type usageView struct {
	Provider string     `json:"provider"`
	Limits   llm.Limits `json:"limits"`
}

// handleUsage reports the quota readings taken from real responses. They are
// persisted as they arrive, so this costs no request to the provider - which is
// the only reason it is worth showing continuously rather than on demand.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r) {
		return
	}
	byProvider := llm.LoadLimits()
	out := make([]usageView, 0, len(byProvider))
	for provider, l := range byProvider {
		out = append(out, usageView{Provider: provider, Limits: l})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
