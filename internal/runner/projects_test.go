package runner_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/store"
)

func newProjects(t *testing.T, path string, notify func(runner.ProjectView)) *runner.Projects {
	t.Helper()
	var file *store.File[runner.ProjectTable]
	if path != "" {
		file = store.New[runner.ProjectTable](path)
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

func TestTheNextIdSurvivesRemovingTheHighestProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	first := newProjects(t, path, nil)
	addProject(t, first, t.TempDir(), "")
	addProject(t, first, t.TempDir(), "")
	if err := first.Remove("PRJ-2"); err != nil {
		t.Fatal(err)
	}

	second := newProjects(t, path, nil)
	if v := addProject(t, second, t.TempDir(), ""); v.ID != "PRJ-3" {
		t.Errorf("the next id = %q, want PRJ-3: the counter outlives the row it counted", v.ID)
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
	target := t.TempDir()
	gitInit(t, target, "main")
	if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	p := newProjects(t, "", nil)
	addProject(t, p, root, "")

	repos, err := p.Repositories(context.Background(), "PRJ-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []runner.Repository{
		{Name: "api", Dir: filepath.Join(root, "api"), Main: "main"},
		{Name: "linked", Dir: filepath.Join(root, "linked"), Main: "main"},
		{Name: "web", Dir: filepath.Join(root, "web"), Main: "master"},
	}
	if len(repos) != 3 || repos[0] != want[0] || repos[1] != want[1] || repos[2] != want[2] {
		t.Errorf("Repositories = %+v, want %+v", repos, want)
	}
	if _, err := p.Repositories(context.Background(), "PRJ-9"); !errors.Is(err, runner.ErrNoProject) {
		t.Errorf("an unknown project = %v, want ErrNoProject", err)
	}
}

func TestAProjectThatIsItselfACheckoutIsListedFirstWithNoName(t *testing.T) {
	root := t.TempDir()
	gitInit(t, root, "main")
	gitInit(t, filepath.Join(root, "sub"), "trunk")
	p := newProjects(t, "", nil)
	addProject(t, p, root, "")

	repos, err := p.Repositories(context.Background(), "PRJ-1")
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

// loader counts how many environments it built, and can be made to fail, to
// wait, or to announce its arrival, which is how the tests below see the
// registry share, record, retry and abandon a load.
type loader struct {
	loads   atomic.Int64
	fail    atomic.Bool
	gate    chan struct{}
	arrived chan struct{}
	built   atomic.Pointer[runner.Env]
}

func (l *loader) load() func(context.Context, string) (*runner.Env, error) {
	return func(ctx context.Context, dir string) (*runner.Env, error) {
		if l.arrived != nil {
			l.arrived <- struct{}{}
		}
		if l.gate != nil {
			<-l.gate
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l.loads.Add(1)
		if l.fail.Load() {
			return nil, errors.New("the SessionStart hook exited 1")
		}
		env, _, err := runner.Load(ctx, runner.Options{Cwd: dir})
		if env != nil {
			l.built.Store(env)
		}
		return env, err
	}
}

func newLoadingProjects(t *testing.T, l *loader, notify func(runner.ProjectView)) *runner.Projects {
	t.Helper()
	p, err := runner.NewProjects(runner.ProjectsConfig{LoadEnv: l.load(), Notify: notify})
	if err != nil {
		t.Fatalf("NewProjects: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestAnEnvironmentIsLoadedOnceAndShared(t *testing.T) {
	l := &loader{}
	p := newLoadingProjects(t, l, nil)
	addProject(t, p, t.TempDir(), "")
	first, err := p.Env(context.Background(), "PRJ-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Env(context.Background(), "PRJ-1")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || l.loads.Load() != 1 {
		t.Errorf("two calls built %d environments, want one shared", l.loads.Load())
	}
	if _, err := p.Env(context.Background(), "PRJ-9"); !errors.Is(err, runner.ErrNoProject) {
		t.Errorf("an unknown project = %v, want ErrNoProject", err)
	}
}

func TestAFailedLoadIsRecordedAnnouncedAndTriedAgain(t *testing.T) {
	l := &loader{}
	l.fail.Store(true)
	var announced []string
	p := newLoadingProjects(t, l, func(v runner.ProjectView) { announced = append(announced, v.LoadError) })
	addProject(t, p, t.TempDir(), "")

	_, err := p.Env(context.Background(), "PRJ-1")
	if err == nil || !strings.Contains(err.Error(), "SessionStart hook exited 1") {
		t.Fatalf("Env = %v, want the load error", err)
	}
	if v, _ := p.Get("PRJ-1"); !strings.Contains(v.LoadError, "SessionStart hook") {
		t.Errorf("LoadError = %q, want the load error recorded on the project", v.LoadError)
	}
	if len(announced) != 2 || announced[1] == "" {
		t.Errorf("announced %q, want the add and then the failure", announced)
	}

	l.fail.Store(false)
	if _, err := p.Env(context.Background(), "PRJ-1"); err != nil {
		t.Fatalf("the retry = %v, want success", err)
	}
	if v, _ := p.Get("PRJ-1"); v.LoadError != "" {
		t.Errorf("LoadError after a successful retry = %q, want cleared", v.LoadError)
	}
}

func TestCallersThatArriveTogetherShareOneLoad(t *testing.T) {
	l := &loader{gate: make(chan struct{}), arrived: make(chan struct{}, 1)}
	p := newLoadingProjects(t, l, nil)
	addProject(t, p, t.TempDir(), "")

	var wg sync.WaitGroup
	envs := make([]*runner.Env, 3)
	wg.Add(1)
	go func() {
		defer wg.Done()
		envs[0], _ = p.Env(context.Background(), "PRJ-1")
	}()
	<-l.arrived
	for i := 1; i < len(envs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			envs[i], _ = p.Env(context.Background(), "PRJ-1")
		}()
	}
	close(l.gate)
	wg.Wait()
	if l.loads.Load() != 1 {
		t.Errorf("%d loads, want 1", l.loads.Load())
	}
	for i, e := range envs {
		if e == nil || e != envs[0] {
			t.Errorf("caller %d got %p, want the one shared environment %p", i, e, envs[0])
		}
	}
}

func TestAWaiterStopsWhenItsContextEnds(t *testing.T) {
	l := &loader{gate: make(chan struct{}), arrived: make(chan struct{}, 1)}
	p := newLoadingProjects(t, l, nil)
	addProject(t, p, t.TempDir(), "")

	var wg sync.WaitGroup
	var leaderEnv *runner.Env
	var leaderErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		leaderEnv, leaderErr = p.Env(context.Background(), "PRJ-1")
	}()
	<-l.arrived

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Env(ctx, "PRJ-1"); !errors.Is(err, context.Canceled) {
		t.Errorf("a waiter with a cancelled context = %v, want context.Canceled", err)
	}

	close(l.gate)
	wg.Wait()
	if leaderErr != nil {
		t.Fatalf("the leader = %v, want success", leaderErr)
	}
	if leaderEnv == nil {
		t.Error("the leader got no environment")
	}
}

func TestALoadThatOutlivesRemoveOrCloseIsClosed(t *testing.T) {
	t.Run("Remove", func(t *testing.T) {
		l := &loader{gate: make(chan struct{}), arrived: make(chan struct{}, 1)}
		p := newLoadingProjects(t, l, nil)
		addProject(t, p, t.TempDir(), "")

		var wg sync.WaitGroup
		var err error
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err = p.Env(context.Background(), "PRJ-1")
		}()
		<-l.arrived
		if rmErr := p.Remove("PRJ-1"); rmErr != nil {
			t.Fatal(rmErr)
		}
		close(l.gate)
		wg.Wait()

		if !errors.Is(err, runner.ErrNoProject) {
			t.Errorf("Env after Remove mid-load = %v, want ErrNoProject", err)
		}
		env := l.built.Load()
		if env == nil {
			t.Fatal("the loader built no environment")
		}
		if _, err := env.NewTools(); err == nil {
			t.Error("the abandoned environment is still open")
		}
	})

	t.Run("Close", func(t *testing.T) {
		l := &loader{gate: make(chan struct{}), arrived: make(chan struct{}, 1)}
		p := newLoadingProjects(t, l, nil)
		addProject(t, p, t.TempDir(), "")

		var wg sync.WaitGroup
		var err error
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err = p.Env(context.Background(), "PRJ-1")
		}()
		<-l.arrived
		p.Close()
		close(l.gate)
		wg.Wait()

		if !errors.Is(err, runner.ErrProjectsClosed) {
			t.Errorf("Env after Close mid-load = %v, want ErrProjectsClosed", err)
		}
		env := l.built.Load()
		if env == nil {
			t.Fatal("the loader built no environment")
		}
		if _, err := env.NewTools(); err == nil {
			t.Error("the abandoned environment is still open")
		}
	})
}

func TestRemovingOrClosingReleasesTheEnvironment(t *testing.T) {
	l := &loader{}
	p := newLoadingProjects(t, l, nil)
	addProject(t, p, t.TempDir(), "")
	addProject(t, p, t.TempDir(), "")
	removed, err := p.Env(context.Background(), "PRJ-1")
	if err != nil {
		t.Fatal(err)
	}
	kept, err := p.Env(context.Background(), "PRJ-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Remove("PRJ-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := removed.NewTools(); err == nil {
		t.Error("the removed project's environment is still open")
	}
	if _, err := kept.NewTools(); err != nil {
		t.Errorf("the other project's environment was closed too: %v", err)
	}

	p.Close()
	if _, err := kept.NewTools(); err == nil {
		t.Error("Close left an environment open")
	}
	if _, err := p.Env(context.Background(), "PRJ-2"); !errors.Is(err, runner.ErrProjectsClosed) {
		t.Errorf("Env after Close = %v, want ErrProjectsClosed", err)
	}
}

func TestARetainedEnvironmentHoldsTheProjectOpen(t *testing.T) {
	l := &loader{}
	p := newLoadingProjects(t, l, nil)
	addProject(t, p, t.TempDir(), "")

	env, release, err := p.Retain(context.Background(), "PRJ-1")
	if err != nil || env == nil {
		t.Fatalf("Retain = %v, %v", env, err)
	}
	second, releaseSecond, err := p.Retain(context.Background(), "PRJ-1")
	if err != nil || second != env {
		t.Fatalf("a second Retain = %v, %v, want the one shared environment", second, err)
	}
	if err := p.Remove("PRJ-1"); !errors.Is(err, runner.ErrProjectInUse) {
		t.Fatalf("Remove while retained = %v, want ErrProjectInUse", err)
	}
	releaseSecond()
	releaseSecond()
	if err := p.Remove("PRJ-1"); !errors.Is(err, runner.ErrProjectInUse) {
		t.Fatalf("Remove after a release called twice = %v, want ErrProjectInUse: it decremented twice", err)
	}
	release()
	if err := p.Remove("PRJ-1"); err != nil {
		t.Fatalf("Remove after the last release = %v", err)
	}
	if _, err := env.NewTools(); err == nil {
		t.Error("the released environment is still open")
	}
}

func TestALoadOutlivesTheRequestThatStartedIt(t *testing.T) {
	l := &loader{gate: make(chan struct{}), arrived: make(chan struct{}, 1)}
	p := newLoadingProjects(t, l, nil)
	addProject(t, p, t.TempDir(), "")

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var leaderEnv *runner.Env
	var leaderErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		leaderEnv, leaderErr = p.Env(ctx, "PRJ-1")
	}()
	<-l.arrived
	cancel()
	close(l.gate)
	wg.Wait()

	if leaderErr != nil || leaderEnv == nil {
		t.Fatalf("the leader = %v, %v, want the loaded environment", leaderEnv, leaderErr)
	}
	again, err := p.Env(context.Background(), "PRJ-1")
	if err != nil || again != leaderEnv {
		t.Fatalf("the next call = %v, %v, want the same environment", again, err)
	}
	if l.loads.Load() != 1 {
		t.Errorf("%d loads, want 1", l.loads.Load())
	}
	if v, _ := p.Get("PRJ-1"); v.LoadError != "" {
		t.Errorf("LoadError = %q, want empty", v.LoadError)
	}
}

func TestAWaiterTakesTheFailedLoadsAnswerRatherThanRetrying(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &loader{gate: make(chan struct{}), arrived: make(chan struct{}, 1)}
		l.fail.Store(true)
		p := newLoadingProjects(t, l, nil)
		addProject(t, p, t.TempDir(), "")

		var wg sync.WaitGroup
		errs := make([]error, 2)
		call := func(i int) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = p.Env(context.Background(), "PRJ-1")
			}()
		}
		call(0)
		<-l.arrived
		l.arrived = nil
		call(1)
		// The waiter is parked on the load before the gate opens, so what it
		// does next is its answer to a load that failed and not a race.
		synctest.Wait()
		close(l.gate)
		wg.Wait()

		for i, err := range errs {
			if err == nil || !strings.Contains(err.Error(), "SessionStart hook exited 1") {
				t.Errorf("caller %d = %v, want the load error", i, err)
			}
		}
		if l.loads.Load() != 1 {
			t.Errorf("%d loads, want 1: the waiter retried a load that had just failed", l.loads.Load())
		}
	})
}
