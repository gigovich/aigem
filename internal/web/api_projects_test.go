package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// projectsBackend is the fake behind the project routes: a map, and the last
// project each skills call was asked about, so a test can tell the query
// string reached the seam.
type projectsBackend struct {
	*fakeBackend
	projects []Project
	repos    map[string][]Repository
	openRun  string
	addErr   error
	askedFor []string
}

// Every method takes the fake's lock: the handlers run on the server's
// goroutines, and a test reads what they recorded.
func (b *projectsBackend) Projects(context.Context) ([]Project, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.projects, nil
}

func (b *projectsBackend) AddProject(_ context.Context, req NewProject) (Project, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.addErr != nil {
		return Project{}, b.addErr
	}
	p := Project{ID: "PRJ-1", Name: req.Name, Dir: req.Dir}
	if p.Name == "" {
		p.Name = "thing"
	}
	b.projects = append(b.projects, p)
	return p, nil
}

func (b *projectsBackend) RemoveProject(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id == b.openRun {
		return Conflict("this project has an open run; close it first")
	}
	for i, p := range b.projects {
		if p.ID == id {
			b.projects = append(b.projects[:i], b.projects[i+1:]...)
			return nil
		}
	}
	return ErrNoProject
}

func (b *projectsBackend) ProjectRepos(_ context.Context, id string) ([]Repository, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range b.projects {
		if p.ID == id {
			return b.repos[id], nil
		}
	}
	return nil, ErrNoProject
}

func (b *projectsBackend) asked(what string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.askedFor = append(b.askedFor, what)
}

func (b *projectsBackend) Skills(_ context.Context, project string) (Skills, error) {
	b.asked("skills:" + project)
	return Skills{}, nil
}

func (b *projectsBackend) Skill(_ context.Context, project, name string) (Skill, error) {
	b.asked("skill:" + project + ":" + name)
	return Skill{SkillSummary: SkillSummary{Name: name}}, nil
}

func (b *projectsBackend) TrustSkills(_ context.Context, project string) (SkillApproval, error) {
	b.asked("trust:" + project)
	return SkillApproval{}, nil
}

func (b *projectsBackend) Commands(_ context.Context, project string) ([]Command, error) {
	b.asked("commands:" + project)
	return nil, nil
}

func newProjectsServer(t *testing.T) (*Server, *projectsBackend) {
	t.Helper()
	b := &projectsBackend{
		fakeBackend: &fakeBackend{},
		projects:    []Project{{Name: "work", Dir: "/home/dev/work"}},
		repos:       map[string][]Repository{"PRJ-1": {{Name: "api", Dir: "/p/api", Main: "main"}}},
	}
	srv := newTestServer(t, Config{Backend: b})
	return srv, b
}

func TestTheFeatureMapNamesProjects(t *testing.T) {
	srv, _ := newProjectsServer(t)
	_, body := getMeta(t, srv)
	if !body.Features["projects"] {
		t.Errorf("features = %v, want projects among them", body.Features)
	}
}

func TestProjectsAreListedAndAddedAndTheDaemonsSentenceIsShown(t *testing.T) {
	srv, b := newProjectsServer(t)
	list := decode[[]Project](t, api(t, srv, http.MethodGet, "/api/projects", ""))
	if len(list) != 1 || list[0].ID != "" || list[0].Name != "work" {
		t.Fatalf("list = %+v, want the daemon's directory with an empty id", list)
	}

	res := api(t, srv, http.MethodPost, "/api/projects", `{"dir":"/home/dev/thing"}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", res.StatusCode, readBody(t, res))
	}
	if got := decode[Project](t, res); got.ID != "PRJ-1" || got.Dir != "/home/dev/thing" {
		t.Errorf("added = %+v", got)
	}

	res = api(t, srv, http.MethodPost, "/api/projects", `{"name":"no dir"}`)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(readBody(t, res), "dir is required") {
		t.Errorf("a body with no dir = %d, want 400 saying dir is required", res.StatusCode)
	}

	b.mu.Lock()
	b.addErr = Refuse(errors.New("cannot add that project: /nope is not a directory"))
	b.mu.Unlock()
	res = api(t, srv, http.MethodPost, "/api/projects", `{"dir":"/nope"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a refusal = %d, want 400", res.StatusCode)
	}
	if body := readBody(t, res); !strings.Contains(body, "/nope is not a directory") {
		t.Errorf("body = %q, want the daemon's own sentence", body)
	}
}

func TestRemovingAProjectAnswersForEachState(t *testing.T) {
	srv, b := newProjectsServer(t)
	api(t, srv, http.MethodPost, "/api/projects", `{"dir":"/home/dev/thing"}`)
	b.mu.Lock()
	b.openRun = "PRJ-1"
	b.mu.Unlock()

	res := api(t, srv, http.MethodDelete, "/api/projects/PRJ-1", "")
	if res.StatusCode != http.StatusConflict || !strings.Contains(readBody(t, res), "open run") {
		t.Fatalf("with an open run = %d, want 409 saying why", res.StatusCode)
	}

	b.mu.Lock()
	b.openRun = ""
	b.mu.Unlock()
	if res := api(t, srv, http.MethodDelete, "/api/projects/PRJ-1", ""); res.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.StatusCode)
	}
	if res := api(t, srv, http.MethodDelete, "/api/projects/PRJ-1", ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("a second delete = %d, want 404", res.StatusCode)
	}
}

func TestProjectReposAreAnArrayAndAnUnknownProjectIs404(t *testing.T) {
	srv, _ := newProjectsServer(t)
	api(t, srv, http.MethodPost, "/api/projects", `{"dir":"/home/dev/thing"}`)
	repos := decode[[]Repository](t, api(t, srv, http.MethodGet, "/api/projects/PRJ-1/repos", ""))
	if len(repos) != 1 || repos[0].Name != "api" || repos[0].Main != "main" {
		t.Errorf("repos = %+v", repos)
	}
	if res := api(t, srv, http.MethodGet, "/api/projects/PRJ-9/repos", ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown project = %d, want 404", res.StatusCode)
	}
}

func TestTheProjectRoutesRefuseOtherMethodsAndNeedTheFeature(t *testing.T) {
	srv, _ := newProjectsServer(t)
	for path, method := range map[string]string{
		"/api/projects":             http.MethodPut,
		"/api/projects/PRJ-1":       http.MethodGet,
		"/api/projects/PRJ-1/repos": http.MethodPost,
	} {
		if res := api(t, srv, method, path, ""); res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", method, path, res.StatusCode)
		}
	}
	plain := newTestServer(t, Config{})
	if res := api(t, plain, http.MethodGet, "/api/projects", ""); res.StatusCode != http.StatusNotImplemented {
		t.Errorf("a backend without the seam = %d, want 501", res.StatusCode)
	}
}

func TestTheSkillsAndCommandsRoutesCarryTheProject(t *testing.T) {
	srv, b := newProjectsServer(t)
	api(t, srv, http.MethodGet, "/api/skills?project=PRJ-1", "")
	api(t, srv, http.MethodGet, "/api/skills/review?project=PRJ-1", "")
	api(t, srv, http.MethodPost, "/api/skills/trust", `{"project":"PRJ-1"}`)
	api(t, srv, http.MethodPost, "/api/skills/trust", `{}`)
	api(t, srv, http.MethodGet, "/api/commands?project=PRJ-1", "")
	api(t, srv, http.MethodGet, "/api/commands", "")
	want := []string{
		"skills:PRJ-1", "skill:PRJ-1:review", "trust:PRJ-1", "trust:", "commands:PRJ-1", "commands:",
	}
	b.mu.Lock()
	got := append([]string(nil), b.askedFor...)
	b.mu.Unlock()
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("the seam was asked %v, want %v", got, want)
	}
}

func TestOpeningARunCarriesItsProject(t *testing.T) {
	srv := newTestServer(t, Config{})
	res := api(t, srv, http.MethodPost, "/api/runs", `{"projectId":"PRJ-1"}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", res.StatusCode, readBody(t, res))
	}
	if run := decode[Run](t, res); run.ProjectID != "PRJ-1" {
		t.Errorf("projectId = %q, want PRJ-1 echoed on the record", run.ProjectID)
	}
}
