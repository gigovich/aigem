package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/gitx"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/pathutil"
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

// testRoot is a canonical root with the named sub-directories made under it.
func testRoot(t *testing.T, dirs ...string) string {
	t.Helper()
	root, err := pathutil.Canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestOnlyDirectoriesUnderTheRootCanBeAdded(t *testing.T) {
	parent := testRoot(t, "root/app", "root/same", "rootless")
	root := filepath.Join(parent, "root")
	if err := os.Symlink(filepath.Join(parent, "rootless"), filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "same"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	b := newWebBackend(webBackendConfig{root: root, projects: testProjects(t, nil)})
	ctx := context.Background()

	if list, err := b.Projects(ctx); err != nil || len(list) != 0 {
		t.Fatalf("list = %+v, %v; want no projects", list, err)
	}
	added, err := b.AddProject(ctx, web.NewProject{Dir: "app"})
	if err != nil || added.Dir != filepath.Join(root, "app") {
		t.Fatalf("a relative dir = %+v, %v; want it joined to the root", added, err)
	}
	if _, err := b.AddProject(ctx, web.NewProject{Dir: root}); err != nil {
		t.Errorf("the root itself = %v, want it added", err)
	}
	if _, err := b.AddProject(ctx, web.NewProject{Dir: "link"}); err != nil {
		t.Fatal(err)
	}
	var refusal *web.Refusal
	if _, err := b.AddProject(ctx, web.NewProject{Dir: "same"}); !errors.As(err, &refusal) {
		t.Errorf("a directory added through a link and then directly = %v, want a refusal", err)
	}
	for _, dir := range []string{"..", "../rootless", "out", filepath.Join(parent, "rootless"), "missing"} {
		_, err := b.AddProject(ctx, web.NewProject{Dir: dir})
		if !errors.As(err, &refusal) || strings.HasPrefix(err.Error(), "runner:") {
			t.Errorf("%s = %v, want a refusal without the package prefix", dir, err)
		}
	}
	if unavailable := newWebBackend(webBackendConfig{}).Unavailable(); !contains(unavailable, "projects") {
		t.Errorf("a backend with no registry withdraws %v, want projects among them", unavailable)
	}
}

func TestDirsListsVisibleSubdirectoriesUnderTheRoot(t *testing.T) {
	root := testRoot(t, "b/x", "a/.git", ".hidden", "c")
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "c"), filepath.Join(root, "in")); err != nil {
		t.Fatal(err)
	}
	b := newWebBackend(webBackendConfig{root: root, projects: testProjects(t, nil)})
	ctx := context.Background()

	got, err := b.Dirs(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range got.Dirs {
		names = append(names, d.Name)
	}
	if got.Root != root || got.Path != "" || strings.Join(names, " ") != "a b c in" || !got.Dirs[0].Git ||
		got.Dirs[1].Git || got.Truncated {
		t.Errorf("listing = %+v", got)
	}
	if got, err := b.Dirs(ctx, "b"); err != nil || got.Path != "b" || len(got.Dirs) != 1 {
		t.Errorf("b = %+v, %v; want x", got, err)
	}
	var refusal *web.Refusal
	for _, p := range []string{"..", "out", "/", "missing"} {
		if _, err := b.Dirs(ctx, p); !errors.As(err, &refusal) {
			t.Errorf("%s = %v, want a refusal", p, err)
		}
	}
}

func TestDirsStopsAtTheCap(t *testing.T) {
	root := testRoot(t)
	for i := range maxDirs + 1 {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("d%04d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b := newWebBackend(webBackendConfig{root: root, projects: testProjects(t, nil)})
	got, err := b.Dirs(context.Background(), "")
	if err != nil || len(got.Dirs) != maxDirs || !got.Truncated {
		t.Errorf("listing = %d dirs, truncated %v, %v; want %d and truncated",
			len(got.Dirs), got.Truncated, err, maxDirs)
	}
}

func TestWebRootIsCanonicalAndADirectory(t *testing.T) {
	dir := testRoot(t, "real")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "real"), link); err != nil {
		t.Fatal(err)
	}
	if got, err := webRoot(link); err != nil || got != filepath.Join(dir, "real") {
		t.Errorf("webRoot(link) = %q, %v; want the target", got, err)
	}
	t.Chdir(filepath.Join(dir, "real"))
	if got, err := webRoot(""); err != nil || got != filepath.Join(dir, "real") {
		t.Errorf("webRoot(\"\") = %q, %v; want the working directory", got, err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{file, filepath.Join(dir, "missing")} {
		if _, err := webRoot(bad); err == nil {
			t.Errorf("webRoot(%s) succeeded, want a refusal", bad)
		}
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
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	projects := testProjects(t, nil)
	root := testRoot(t)
	b := newWebBackend(webBackendConfig{root: root, runs: testProjectRuns(t, projects), projects: projects})
	added, err := b.AddProject(context.Background(), web.NewProject{Dir: root})
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
	if err := b.RemoveRun(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveProject(context.Background(), added.ID); err != nil {
		t.Fatalf("remove after the run was deleted = %v", err)
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
	root := testRoot(t)
	b := newWebBackend(webBackendConfig{root: root, projects: testProjects(t, failing)})
	added, err := b.AddProject(context.Background(), web.NewProject{Dir: root})
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
	root := testRoot(t, "other")
	other := filepath.Join(root, "other")
	writeProjectSkill(t, other, "theirs")
	b := newWebBackend(webBackendConfig{root: root, projects: testProjects(t, nil)})
	added, err := b.AddProject(context.Background(), web.NewProject{Dir: other})
	if err != nil {
		t.Fatal(err)
	}

	var refusal *web.Refusal
	if _, err := b.Skills(context.Background(), ""); !errors.As(err, &refusal) {
		t.Fatalf("skills with no project = %v, want a refusal", err)
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
	rt := &webRuntime{models: sharedModels(defaultModelRegistry()), projects: projects}
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

func TestUpdatingAProjectSetsItsDispatcher(t *testing.T) {
	ctx := context.Background()
	root := testRoot(t)
	b := newWebBackend(webBackendConfig{root: root, projects: testProjects(t, nil)})
	added, err := b.AddProject(ctx, web.NewProject{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	slots, paused := 2, true
	got, err := b.UpdateProject(ctx, added.ID, web.ProjectPatch{Slots: &slots})
	if err != nil || got.Slots != 2 || got.Paused {
		t.Fatalf("slots = %+v, %v", got, err)
	}
	if got, err = b.UpdateProject(ctx, added.ID, web.ProjectPatch{Paused: &paused}); err != nil ||
		got.Slots != 2 || !got.Paused {
		t.Fatalf("pause = %+v, %v", got, err)
	}
	if list, _ := b.Projects(ctx); list[len(list)-1].Slots != 2 || !list[len(list)-1].Paused {
		t.Errorf("listed = %+v", list)
	}
	bad := 9
	_, err = b.UpdateProject(ctx, added.ID, web.ProjectPatch{Slots: &bad})
	var refusal *web.Refusal
	if !errors.As(err, &refusal) || err.Error() != "slots must be between 0 and 8" {
		t.Errorf("slots 9 = %v, want a refusal without the package prefix", err)
	}
	if _, err := b.UpdateProject(ctx, "PRJ-9", web.ProjectPatch{Slots: &slots}); !errors.Is(err,
		web.ErrNoProject) {
		t.Errorf("unknown project = %v, want web.ErrNoProject", err)
	}
}

func TestAddProjectCanInitAGitRepository(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	root := testRoot(t, "plain", "fresh")
	b := newWebBackend(webBackendConfig{root: root, projects: testProjects(t, nil)})
	ctx := context.Background()
	if _, err := b.AddProject(ctx, web.NewProject{Dir: "plain"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "plain", ".git")); err == nil {
		t.Error("a project added without gitInit became a repository")
	}
	if _, err := b.AddProject(ctx, web.NewProject{Dir: "fresh", GitInit: true}); err != nil {
		t.Fatal(err)
	}
	if got := gitx.MainBranch(ctx, filepath.Join(root, "fresh")); got != "main" {
		t.Errorf("main branch = %q, want main", got)
	}
}
