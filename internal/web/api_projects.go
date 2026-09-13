package web

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// The projects API: the registry a page lists, the one it adds to, and the
// repositories a project holds. A project is a directory on the daemon's
// machine that the signed-in person typed, and the sandbox roots at it as it
// roots at the daemon's own directory today.

type ProjectsBackend interface {
	// Projects lists the daemon's own directory first, with an empty id, then
	// the registry in the order projects were added.
	Projects(ctx context.Context) ([]Project, error)
	// AddProject registers a directory; a refusal is a *Refusal.
	AddProject(ctx context.Context, req NewProject) (Project, error)
	// RemoveProject forgets a project. ErrNoProject for an unknown id, a
	// Conflict while it has an open run. It never removes files.
	RemoveProject(ctx context.Context, id string) error
	// ProjectRepos discovers the project's git checkouts on demand.
	ProjectRepos(ctx context.Context, id string) ([]Repository, error)
}

type Project struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Dir     string    `json:"dir"`
	Created time.Time `json:"created,omitzero"`
	// LoadError is why the project's environment could not be loaded, when it
	// could not. Runs cannot open in it until a load succeeds.
	LoadError string `json:"loadError,omitempty"`
}

type NewProject struct {
	Dir  string `json:"dir"`
	Name string `json:"name,omitempty"`
}

type Repository struct {
	// Name is the directory under the project, or empty for the project
	// directory itself.
	Name string `json:"name"`
	Dir  string `json:"dir"`
	// Main is the branch a run merges into: main or master, empty when the
	// checkout has neither.
	Main string `json:"main,omitempty"`
}

// ErrNoProject is returned for an id no project answers to.
var ErrNoProject = errors.New("web: no such project")

// ErrConflict is returned for an operation the collection's state refuses -
// forgetting a project while it has an open run. Conflict wraps it with the
// sentence a person reads, the way Busy does for ErrBusy.
var ErrConflict = errors.New("web: the operation conflicts with the current state")

func Conflict(reason string) error { return &conflict{reason: reason} }

type conflict struct{ reason string }

func (c *conflict) Error() string { return c.reason }
func (c *conflict) Unwrap() error { return ErrConflict }

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[ProjectsBackend](s, w, "projects")
	if !ok {
		return
	}
	items, err := b.Projects(r.Context())
	if err != nil {
		writeRunError(w, "listing projects", err)
		return
	}
	if items == nil {
		items = []Project{}
	}
	writeJSON(w, items)
}

func (s *Server) handleAddProject(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[ProjectsBackend](s, w, "projects")
	if !ok {
		return
	}
	var req NewProject
	if err := decodeJSON(w, r, &req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Dir == "" {
		http.Error(w, "dir is required", http.StatusBadRequest)
		return
	}
	p, err := b.AddProject(r.Context(), req)
	if err != nil {
		writeRunError(w, "adding a project", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, p)
}

func (s *Server) handleRemoveProject(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[ProjectsBackend](s, w, "projects")
	if !ok {
		return
	}
	if err := b.RemoveProject(r.Context(), r.PathValue("id")); err != nil {
		writeRunError(w, "removing a project", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleProjectRepos(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[ProjectsBackend](s, w, "projects")
	if !ok {
		return
	}
	repos, err := b.ProjectRepos(r.Context(), r.PathValue("id"))
	if err != nil {
		writeRunError(w, "listing a project's repositories", err)
		return
	}
	if repos == nil {
		repos = []Repository{}
	}
	writeJSON(w, repos)
}
