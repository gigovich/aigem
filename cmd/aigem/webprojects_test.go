package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/web"
)

func testProjects(t *testing.T, loadEnv func(context.Context, string) (*runner.Env, error)) *runner.Projects {
	t.Helper()
	if loadEnv == nil {
		loadEnv = func(ctx context.Context, dir string) (*runner.Env, error) {
			env, _, err := runner.Load(ctx, runner.Options{Cwd: dir})
			return env, err
		}
	}
	p, err := runner.NewProjects(runner.ProjectsConfig{LoadEnv: loadEnv})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestTheDaemonsDirectoryIsListedFirstWithNoId(t *testing.T) {
	env, _, err := runner.Load(context.Background(), runner.Options{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(env.Close)
	b := newWebBackend(webBackendConfig{env: env, projects: testProjects(t, nil)})
	added, err := b.AddProject(context.Background(), web.NewProject{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	list, err := b.Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "" || list[0].Dir != env.Cwd || list[0].Name != filepath.Base(env.Cwd) {
		t.Fatalf("list = %+v, want the daemon's directory first", list)
	}
	if list[1].ID != added.ID {
		t.Errorf("list[1] = %+v, want the added project", list[1])
	}
	_, err = b.AddProject(context.Background(), web.NewProject{Dir: env.Cwd})
	var refusal *web.Refusal
	if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "daemon's own directory") {
		t.Errorf("adding the daemon's directory = %v, want a refusal saying so", err)
	}
	_, err = b.AddProject(context.Background(), web.NewProject{Dir: "relative"})
	if !errors.As(err, &refusal) || strings.HasPrefix(err.Error(), "runner:") {
		t.Errorf("a bad directory = %v, want a refusal without the package prefix", err)
	}
	if unavailable := newWebBackend(webBackendConfig{env: env}).Unavailable(); !contains(unavailable, "projects") {
		t.Errorf("a backend with no registry withdraws %v, want projects among them", unavailable)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// testProjectRuns is the daemon's own rule in miniature: a run opened in a
// project retains it, and gives it back when the run closes.
func testProjectRuns(t *testing.T, projects *runner.Projects) *runner.Runs {
	t.Helper()
	env, _, err := runner.Load(context.Background(), runner.Options{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(env.Close)
	runs, err := runner.NewRuns(runner.RunsConfig{
		Open: func(ctx context.Context, req runner.RunRequest) (*runner.Session, runner.Opened, error) {
			reg, err := env.NewTools()
			if err != nil {
				return nil, runner.Opened{}, err
			}
			opened := runner.Opened{Model: "test/model", Root: env.Cwd}
			if req.ProjectID != "" {
				projectEnv, release, err := projects.Retain(ctx, req.ProjectID)
				if err != nil {
					return nil, runner.Opened{}, err
				}
				opened.Root, opened.Release = projectEnv.Cwd, release
			}
			s := runner.NewSession(runner.Spec{
				Mode: req.Mode, Tools: reg, Title: req.Title,
				Backend: llm.NewRef(llm.New("http://127.0.0.1:9", "t")),
			})
			return s, opened, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runs.Close)
	return runs
}

func TestRemovingAProjectWithAnOpenRunIsAConflict(t *testing.T) {
	projects := testProjects(t, nil)
	b := newWebBackend(webBackendConfig{runs: testProjectRuns(t, projects), projects: projects})
	added, err := b.AddProject(context.Background(), web.NewProject{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	run, err := b.OpenRun(context.Background(), web.NewRun{ProjectID: added.ID})
	if err != nil {
		t.Fatal(err)
	}
	if run.ProjectID != added.ID {
		t.Errorf("the run reports project %q, want %q", run.ProjectID, added.ID)
	}
	if err := b.RemoveProject(context.Background(), added.ID); !errors.Is(err, web.ErrConflict) {
		t.Fatalf("remove with an open run = %v, want ErrConflict", err)
	}
	if err := b.CloseRun(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveProject(context.Background(), added.ID); err != nil {
		t.Fatalf("remove after the run closed = %v", err)
	}
	if err := b.RemoveProject(context.Background(), added.ID); !errors.Is(err, web.ErrNoProject) {
		t.Errorf("a second remove = %v, want ErrNoProject", err)
	}
}

// A project the daemon could not load is still a project: the row carries the
// reason, so the person can see what to fix rather than an empty list.
func TestAProjectThatCannotLoadIsStillListedWithItsReason(t *testing.T) {
	failing := func(context.Context, string) (*runner.Env, error) {
		return nil, errors.New("the SessionStart hook exited 1")
	}
	b := newWebBackend(webBackendConfig{projects: testProjects(t, failing)})
	added, err := b.AddProject(context.Background(), web.NewProject{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Skills(context.Background(), added.ID); err == nil {
		t.Fatal("the skills of a project that cannot load = no error, want the load error")
	}
	list, err := b.Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].LoadError == "" || list[0].Created.IsZero() {
		t.Errorf("list = %+v, want the project with its reason and when it was added", list)
	}
}

func TestTheAdapterClassifiesWhatTheProjectRegistryReports(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   error
		want error
	}{
		{"no project", runner.ErrNoProject, web.ErrNoProject},
		{"in use", runner.ErrProjectInUse, web.ErrConflict},
		{"shutting down", runner.ErrProjectsClosed, runner.ErrProjectsClosed},
	} {
		if got := webProjectError(tc.in); !errors.Is(got, tc.want) {
			t.Errorf("%s: webProjectError(%v) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestSkillsAreReadFromTheProjectNamed(t *testing.T) {
	home := t.TempDir()
	env, _, err := runner.Load(context.Background(), runner.Options{Cwd: home})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(env.Close)
	other := t.TempDir()
	writeProjectSkill(t, other, "theirs")
	b := newWebBackend(webBackendConfig{env: env, projects: testProjects(t, nil)})
	added, err := b.AddProject(context.Background(), web.NewProject{Dir: other})
	if err != nil {
		t.Fatal(err)
	}

	mine, err := b.Skills(context.Background(), "")
	if err != nil || mine.Pending != nil {
		t.Fatalf("the daemon's own skills = %+v, %v; want nothing pending", mine, err)
	}
	theirs, err := b.Skills(context.Background(), added.ID)
	if err != nil || theirs.Pending == nil || !contains(theirs.Pending.Names, "theirs") {
		t.Fatalf("the project's skills = %+v, %v; want theirs pending", theirs, err)
	}
	if _, err := b.Skills(context.Background(), "PRJ-9"); !errors.Is(err, web.ErrNoProject) {
		t.Errorf("an unknown project = %v, want ErrNoProject", err)
	}
	if _, err := b.Commands(context.Background(), added.ID); err != nil {
		t.Errorf("commands for the project = %v", err)
	}
}

func TestOpenRunResolvesTheProjectBeforeTheModel(t *testing.T) {
	failing := func(context.Context, string) (*runner.Env, error) {
		return nil, errors.New("the SessionStart hook exited 1")
	}
	projects := testProjects(t, failing)
	added, err := projects.Add(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	rt := &webRuntime{models: defaultModelRegistry(), projects: projects}
	_, _, err = rt.openRun(context.Background(), runner.RunRequest{ProjectID: "PRJ-9"})
	if !errors.Is(err, runner.ErrNoProject) {
		t.Errorf("an unknown project = %v, want ErrNoProject", err)
	}
	_, _, err = rt.openRun(context.Background(), runner.RunRequest{ProjectID: added.ID})
	if err == nil || !strings.Contains(err.Error(), "SessionStart hook exited 1") {
		t.Errorf("a project that cannot load = %v, want the load error", err)
	}
	if v, _ := projects.Get(added.ID); v.LoadError == "" {
		t.Error("the failure was not recorded on the project")
	}
}
