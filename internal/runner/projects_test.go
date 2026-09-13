package runner_test

import (
	"errors"
	"os"
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
