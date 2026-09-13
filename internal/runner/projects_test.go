package runner_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/store"
)

func newProjects(t *testing.T, path string, notify func(runner.ProjectView)) *runner.Projects {
	t.Helper()
	var file *store.File[[]runner.Project]
	if path != "" {
		file = store.New[[]runner.Project](path)
	}
	p, err := runner.NewProjects(runner.ProjectsConfig{Store: file, Notify: notify})
	if err != nil {
		t.Fatalf("NewProjects: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func addProject(t *testing.T, p *runner.Projects, dir, name string) runner.ProjectView {
	t.Helper()
	v, err := p.Add(dir, name)
	if err != nil {
		t.Fatalf("Add(%q): %v", dir, err)
	}
	return v
}

func TestAProjectIsListedUnderTheIdItWasGiven(t *testing.T) {
	p := newProjects(t, "", nil)
	dir := t.TempDir()
	v := addProject(t, p, dir, "")
	if v.ID != "PRJ-1" || v.Name != filepath.Base(dir) || v.Dir != dir || v.Created.IsZero() {
		t.Fatalf("Add = %+v, want PRJ-1 named after its directory", v)
	}
	got, err := p.Get("PRJ-1")
	if err != nil || got.Dir != dir {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if list := p.List(); len(list) != 1 || list[0].ID != "PRJ-1" {
		t.Errorf("List = %+v, want the one project", list)
	}
	if named := addProject(t, p, t.TempDir(), "given"); named.Name != "given" || named.ID != "PRJ-2" {
		t.Errorf("a named project = %+v", named)
	}
}

func TestAProjectSurvivesARestartAndItsIdIsNeverReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	dir := t.TempDir()
	first := newProjects(t, path, nil)
	addProject(t, first, dir, "")
	if err := first.Remove("PRJ-1"); err != nil {
		t.Fatal(err)
	}
	addProject(t, first, t.TempDir(), "kept")

	second := newProjects(t, path, nil)
	list := second.List()
	if len(list) != 1 || list[0].ID != "PRJ-2" || list[0].Name != "kept" {
		t.Fatalf("after restart List = %+v, want PRJ-2 alone", list)
	}
	if v := addProject(t, second, t.TempDir(), ""); v.ID != "PRJ-3" {
		t.Errorf("the next id after a restart = %q, want PRJ-3", v.ID)
	}
}

func TestAddingRefusesWhatIsNotAProjectDirectory(t *testing.T) {
	p := newProjects(t, "", nil)
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	addProject(t, p, dir, "")
	for name, bad := range map[string]string{
		"relative": "relative/path",
		"missing":  filepath.Join(dir, "missing"),
		"a file":   file,
		"twice":    dir,
	} {
		_, err := p.Add(bad, "")
		if !errors.Is(err, runner.ErrBadProject) {
			t.Errorf("%s: Add(%q) = %v, want ErrBadProject", name, bad, err)
		}
	}
	if list := p.List(); len(list) != 1 {
		t.Errorf("a refused add left a row behind: %+v", list)
	}
}

func TestRemovingForgetsTheProject(t *testing.T) {
	p := newProjects(t, "", nil)
	addProject(t, p, t.TempDir(), "")
	if err := p.Remove("PRJ-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get("PRJ-1"); !errors.Is(err, runner.ErrNoProject) {
		t.Errorf("Get after Remove = %v, want ErrNoProject", err)
	}
	if err := p.Remove("PRJ-1"); !errors.Is(err, runner.ErrNoProject) {
		t.Errorf("a second Remove = %v, want ErrNoProject", err)
	}
}

func TestEveryChangeIsAnnounced(t *testing.T) {
	var seen []string
	p := newProjects(t, "", func(v runner.ProjectView) { seen = append(seen, v.ID) })
	addProject(t, p, t.TempDir(), "")
	if err := p.Remove("PRJ-1"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "PRJ-1" || seen[1] != "PRJ-1" {
		t.Errorf("announced %v, want PRJ-1 on add and on remove", seen)
	}
}

// gitInit makes dir a checkout whose only branch is named branch. Skipped
// where git is not installed: discovery shells out to it for the branch name.
func gitInit(t *testing.T, dir, branch string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", branch},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "root"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestRepositoriesAreTheCheckoutsOneLevelDown(t *testing.T) {
	root := t.TempDir()
	gitInit(t, filepath.Join(root, "web"), "master")
	gitInit(t, filepath.Join(root, "api"), "main")
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p := newProjects(t, "", nil)
	addProject(t, p, root, "")

	repos, err := p.Repositories("PRJ-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []runner.Repository{
		{Name: "api", Dir: filepath.Join(root, "api"), Main: "main"},
		{Name: "web", Dir: filepath.Join(root, "web"), Main: "master"},
	}
	if len(repos) != 2 || repos[0] != want[0] || repos[1] != want[1] {
		t.Errorf("Repositories = %+v, want %+v", repos, want)
	}
	if _, err := p.Repositories("PRJ-9"); !errors.Is(err, runner.ErrNoProject) {
		t.Errorf("an unknown project = %v, want ErrNoProject", err)
	}
}

func TestAProjectThatIsItselfACheckoutIsListedFirstWithNoName(t *testing.T) {
	root := t.TempDir()
	gitInit(t, root, "main")
	gitInit(t, filepath.Join(root, "sub"), "trunk")
	p := newProjects(t, "", nil)
	addProject(t, p, root, "")

	repos, err := p.Repositories("PRJ-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[0].Name != "" || repos[0].Dir != root || repos[0].Main != "main" {
		t.Fatalf("Repositories = %+v, want the project itself first", repos)
	}
	// Neither main nor master: the branch a run would merge into is unknown.
	if repos[1].Name != "sub" || repos[1].Main != "" {
		t.Errorf("a checkout with neither main nor master = %+v, want an empty Main", repos[1])
	}
}
