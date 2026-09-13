# Projects Phase Two, Sub-project A: Projects - Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** A person adds a project (a directory on the daemon's machine) from the browser, the
daemon keeps a registry of them, loads one environment per project on first use, and runs,
skills and the Worktrees screen are scoped to the project chosen in the sidebar.

**Architecture:** The registry is a new `runner.Projects` beside `runner.Runs`, persisted through
`store.File[[]Project]` and holding one lazily loaded `runner.Env` per project. `internal/web`
gains a `ProjectsBackend` seam, four routes, the `projects` feature key and the `project.updated`
control frame; the skills and commands seams take a project id. `cmd/aigem` wires the registry
and resolves a run's environment from its project. The client keeps its layering
(`lib -> state -> ui -> shell -> screens`): the active project is one store value saved in
`localStorage`, the sidebar's Projects block lists the registry, a dialog adds one, and the
Worktrees screen lists a project's repositories.

**Tech Stack:** Go 1.22+ (`internal/runner`, `internal/web`, `cmd/aigem`), React 19 +
TypeScript + Vite + Vitest + Testing Library under `internal/web/_ui`.

**Spec:** `docs/superpowers/specs/2026-09-13-projects-phase-two-design.md`, section A. Sub-projects
B (Tickets) and C (Runs on tickets) get their own plans after this one ships.

## Global Constraints

- No code comments unless they explain hidden logic; then one or two lines.
- Line length: Go and TS up to 120 characters; Markdown up to 100 with hard breaks.
- Hyphens only, never em dashes, in code, docs, tests and commit messages.
- After UI edits run `make web-check` (eslint, tsc, vitest); it must be clean. Never run
  `npx prettier` in `internal/web/_ui`.
- After Go edits run `gofmt -l internal cmd` (must print nothing), `go test ./...`,
  `golangci-lint run`.
- Tasks 1 to 5 change nothing the daemon serves yet; the first deploy is Task 6. From there,
  every task ends deployed: `make web && make install && systemctl --user restart aigem-web`,
  then `curl -s https://tba.tail74d52.ts.net/healthz` answers `{"ok":true}`. Tasks that change
  the API are checked with `curl` against the live daemon; tasks that change the page are
  checked headlessly with playwright. Playwright lives in a scratch directory with a signed-in
  `state.json`; launch with `chromium.launch({ channel: 'chrome' })` (the playwright CDN is
  unreachable here, the system Chrome works). If it is gone, `npm i playwright` in a scratch
  directory and sign in once by opening `https://tba.tail74d52.ts.net/?token=<token>` (token
  from `journalctl --user -u aigem-web --no-pager | grep -o 'token=[^ ]*' | tail -1 | cut -d= -f2`),
  then save `storageState`. The token is also the `Authorization: Bearer <token>` credential
  for `curl`.
- The daemon runs from `~/work`. Close any run a check creates (`DELETE /api/runs/{id}`) and
  remove any project a check adds (`DELETE /api/projects/{id}`).
- Commit on `main` and `git push origin main` at the end of every task.
- Review subagents must run in worktree isolation (`isolation: "worktree"`): a reviewer in the
  working tree stashes and silently reverts in-flight edits.
- Spec values, verbatim: project ids are `PRJ-1`, `PRJ-2`, ...; the registry file is
  `$XDG_STATE_HOME/aigem/projects.json`; a project is a directory whose repositories are the
  direct child directories containing `.git`, plus the directory itself when it is a checkout;
  `Repository.Main` is `main` or `master`; the daemon's own directory is listed as
  `{"id":"","name":...}`; feature key `projects`; control frame `project.updated`;
  `POST /api/projects` answers 201, 400 for a path that is not an absolute existing directory or
  is already registered; `DELETE /api/projects/{id}` answers 409 while the project has an open
  run and never removes files; `POST /api/runs` accepts `projectId`; `POST /api/skills/trust`
  gains a `project` field; the sidebar selection is saved in `localStorage` per browser; route
  `/projects/{id}` selects.

## Decisions this plan makes where the spec is silent

- The daemon's own directory has id `""` and therefore no URL of its own: `GET /api/projects/{id}/
  repos` cannot name it (Go's mux does not match an empty segment). The Worktrees screen shows an
  empty state for it. B and C keep tickets and worktrees to registered projects for the same
  reason (`projects/{projectID}/tickets.json` needs an id).
- "Loading an environment happens outside the request that triggered it the way OpenRun does"
  is read as: outside the registry's lock, in the calling goroutine, with callers that arrive
  together sharing one load. A load that fails is recorded on the project, announced, and tried
  again by the next caller.
- A project's environment is loaded with no `--trust-project-*` flags: hooks and MCP servers of
  a project added from the browser stay withheld; skills go through the web trust flow, per
  project.
- `Repository.Name` is `""` for the project directory itself, which is what B's `Ticket.Repo`
  "empty means the project itself" expects.
- A `skills.updated` frame does not name a project; a page refetches the catalogue of the
  project it shows.

## File structure

Create:
- `internal/runner/projects.go` - the registry: records, ids, persistence, per-project
  environments, discovery of repositories.
- `internal/runner/projects_test.go`
- `internal/web/api_projects.go` - the seam, the wire types, four handlers.
- `internal/web/api_projects_test.go`
- `cmd/aigem/webprojects.go` - the adapter half: registry wiring, `envFor`, error mapping.
- `cmd/aigem/webprojects_test.go`
- `internal/web/_ui/src/screens/NewProjectDialog.tsx`
- `internal/web/_ui/src/screens/Worktrees.tsx`
- `internal/web/_ui/src/screens/projects.test.tsx`

Modify:
- `internal/runner/runs.go` - `ProjectID` on `RunRequest` and `Run`.
- `internal/web/backend.go`, `api_runs.go`, `api_phase1.go`, `meta.go`, `server.go`
- `internal/web/backend_test.go`, `api_phase1_test.go`
- `cmd/aigem/webbackend.go`, `webphase1.go`, `webruns.go`, `webcmd.go`
- `internal/web/_ui/src/lib/wire.ts`, `api.ts`, `route.ts`
- `internal/web/_ui/src/state/app.ts`, `app.test.ts`
- `internal/web/_ui/src/shell/Sidebar.tsx`, `src/App.tsx`
- `internal/web/_ui/src/screens/Chat.tsx`, `Skills.tsx`, `Placeholders.tsx`
- `internal/web/_ui/src/test/harness.tsx`
- `docs/web.md`, `CHANGELOG.md`

---

### Task 1: The project registry

**Files:**
- Create: `internal/runner/projects.go`
- Test: `internal/runner/projects_test.go`
- Read only: `internal/runner/runs.go:100-260` (the table shape this copies),
  `internal/store/store.go:82-160` (`File.Load`, `File.Save`).

**Interfaces:**
- Consumes: `store.New[[]runner.Project](path)`, `(*store.File[T]).Load()`, `Save(v)`.
- Produces:
  - `type Project struct { ID, Name, Dir string; Created time.Time }` with json tags `id`,
    `name`, `dir`, `created`.
  - `type ProjectView struct { Project; LoadError string }`.
  - `type ProjectsConfig struct { Store *store.File[[]Project]; LoadEnv func(context.Context,
    string) (*Env, error); Notify func(ProjectView); Now func() time.Time }`.
  - `func NewProjects(cfg ProjectsConfig) (*Projects, error)`
  - `func (p *Projects) List() []ProjectView`, `Get(id string) (ProjectView, error)`,
    `Add(dir, name string) (ProjectView, error)`, `Remove(id string) error`.
  - `var ErrNoProject`, `ErrBadProject`.
  - Task 3 adds `Env`, `Close`; Task 2 adds `Repositories`.

- [ ] **Step 1: Write the failing tests**

Create `internal/runner/projects_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/runner/ -run 'Project' -count=1`
Expected: FAIL to compile with `undefined: runner.NewProjects`.

- [ ] **Step 3: Write the registry**

Create `internal/runner/projects.go`:

```go
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/store"
)

var (
	ErrNoProject  = errors.New("runner: no such project")
	ErrBadProject = errors.New("runner: cannot add that project")
)

const projectIDPrefix = "PRJ-"

type Project struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Dir     string    `json:"dir"`
	Created time.Time `json:"created"`
}

type ProjectView struct {
	Project
	LoadError string
}

type ProjectsConfig struct {
	Store   *store.File[[]Project]
	LoadEnv func(ctx context.Context, dir string) (*Env, error)
	Notify  func(ProjectView)
	Now     func() time.Time
}

type Projects struct {
	loadEnv func(context.Context, string) (*Env, error)
	notify  func(ProjectView)
	now     func() time.Time

	mu     sync.Mutex
	file   *store.File[[]Project]
	byID   map[string]*project
	order  []string
	next   int
	closed bool
}

type project struct {
	rec     Project
	env     *Env
	loadErr string
	loading chan struct{}
}

func NewProjects(cfg ProjectsConfig) (*Projects, error) {
	p := &Projects{
		loadEnv: cfg.LoadEnv, notify: cfg.Notify, now: cfg.Now,
		file: cfg.Store, byID: map[string]*project{},
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.notify == nil {
		p.notify = func(ProjectView) {}
	}
	if p.loadEnv == nil {
		p.loadEnv = func(context.Context, string) (*Env, error) {
			return nil, errors.New("runner: this registry cannot load environments")
		}
	}
	if cfg.Store == nil {
		return p, nil
	}
	saved, err := cfg.Store.Load()
	if err != nil {
		return nil, fmt.Errorf("runner: could not read the project table: %w", err)
	}
	for _, rec := range saved {
		if rec.ID == "" || p.byID[rec.ID] != nil {
			continue
		}
		p.byID[rec.ID] = &project{rec: rec}
		p.order = append(p.order, rec.ID)
		if n := projectNumber(rec.ID); n > p.next {
			p.next = n
		}
	}
	return p, nil
}

func projectNumber(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, projectIDPrefix))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (p *Projects) List() []ProjectView {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ProjectView, 0, len(p.order))
	for _, id := range p.order {
		out = append(out, p.viewLocked(p.byID[id]))
	}
	return out
}

func (p *Projects) Get(id string) (ProjectView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr := p.byID[id]
	if pr == nil {
		return ProjectView{}, ErrNoProject
	}
	return p.viewLocked(pr), nil
}

func (p *Projects) Add(dir, name string) (ProjectView, error) {
	if !filepath.IsAbs(dir) {
		return ProjectView{}, fmt.Errorf("%w: %q is not an absolute path", ErrBadProject, dir)
	}
	dir = filepath.Clean(dir)
	info, err := os.Stat(dir)
	if err != nil {
		return ProjectView{}, fmt.Errorf("%w: %v", ErrBadProject, err)
	}
	if !info.IsDir() {
		return ProjectView{}, fmt.Errorf("%w: %s is not a directory", ErrBadProject, dir)
	}
	if name == "" {
		name = filepath.Base(dir)
	}

	p.mu.Lock()
	for _, id := range p.order {
		if p.byID[id].rec.Dir == dir {
			p.mu.Unlock()
			return ProjectView{}, fmt.Errorf("%w: %s is already project %s", ErrBadProject, dir, id)
		}
	}
	p.next++
	rec := Project{ID: projectIDPrefix + strconv.Itoa(p.next), Name: name, Dir: dir, Created: p.now()}
	p.byID[rec.ID] = &project{rec: rec}
	p.order = append(p.order, rec.ID)
	if err := p.saveLocked(); err != nil {
		delete(p.byID, rec.ID)
		p.order = p.order[:len(p.order)-1]
		p.mu.Unlock()
		return ProjectView{}, err
	}
	p.mu.Unlock()

	v := ProjectView{Project: rec}
	p.notify(v)
	return v, nil
}

func (p *Projects) Remove(id string) error {
	p.mu.Lock()
	pr := p.byID[id]
	if pr == nil {
		p.mu.Unlock()
		return ErrNoProject
	}
	at := slices.Index(p.order, id)
	delete(p.byID, id)
	p.order = slices.Delete(slices.Clone(p.order), at, at+1)
	if err := p.saveLocked(); err != nil {
		p.byID[id] = pr
		p.order = slices.Insert(p.order, at, id)
		p.mu.Unlock()
		return err
	}
	env := pr.env
	pr.env = nil
	p.mu.Unlock()

	if env != nil {
		env.Close()
	}
	p.notify(ProjectView{Project: pr.rec})
	return nil
}

func (p *Projects) viewLocked(pr *project) ProjectView {
	return ProjectView{Project: pr.rec, LoadError: pr.loadErr}
}

// saveLocked writes the table and returns the error: unlike a run, a project is
// nothing but its record, so a record that could not be written is an add that
// did not happen.
func (p *Projects) saveLocked() error {
	if p.file == nil {
		return nil
	}
	table := make([]Project, 0, len(p.order))
	for _, id := range p.order {
		table = append(table, p.byID[id].rec)
	}
	if err := p.file.Save(table); err != nil {
		return fmt.Errorf("runner: could not write the project table: %w", err)
	}
	return nil
}

// Close releases every loaded environment. Task 3 fills it in.
func (p *Projects) Close() {}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/runner/ -run 'Project' -count=1 -v`
Expected: all five PASS.

- [ ] **Step 5: Format, lint, full test**

Run: `gofmt -l internal cmd && golangci-lint run && go test ./internal/runner/ -count=1`
Expected: gofmt prints nothing, lint clean, tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/runner/projects.go internal/runner/projects_test.go
git commit -m "feat(runner): a registry of projects, persisted beside the run table"
git push origin main
```

---

### Task 2: Repository discovery

**Files:**
- Modify: `internal/runner/projects.go`
- Test: `internal/runner/projects_test.go`

**Interfaces:**
- Produces:
  - `type Repository struct { Name, Dir, Main string }` with json tags `name`, `dir`,
    `main,omitempty`.
  - `func (p *Projects) Repositories(id string) ([]Repository, error)` - `ErrNoProject` for an
    unknown id; the project directory itself first (Name `""`) when it is a checkout, then each
    direct child directory holding `.git`, in name order.

- [ ] **Step 1: Write the failing tests**

Append to `internal/runner/projects_test.go` (add `"os/exec"` to the imports):

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/runner/ -run 'Repositor|Checkout' -count=1`
Expected: FAIL to compile with `p.Repositories undefined`.

- [ ] **Step 3: Write discovery**

Append to `internal/runner/projects.go` (add `"os/exec"` to the imports):

```go
type Repository struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
	Main string `json:"main,omitempty"`
}

// Repositories discovers the project's git checkouts on demand: the project
// directory itself when it is one, then each direct child that is, by name.
func (p *Projects) Repositories(id string) ([]Repository, error) {
	v, err := p.Get(id)
	if err != nil {
		return nil, err
	}
	var out []Repository
	if isCheckout(v.Dir) {
		out = append(out, Repository{Dir: v.Dir, Main: mainBranch(v.Dir)})
	}
	entries, err := os.ReadDir(v.Dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		dir := filepath.Join(v.Dir, e.Name())
		if e.IsDir() && isCheckout(dir) {
			out = append(out, Repository{Name: e.Name(), Dir: dir, Main: mainBranch(dir)})
		}
	}
	return out, nil
}

func isCheckout(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// mainBranch is the branch a run will merge into, read once per discovery.
// Empty when the checkout has neither name.
func mainBranch(dir string) string {
	for _, b := range []string{"main", "master"} {
		if exec.Command("git", "-C", dir, "show-ref", "--verify", "--quiet", "refs/heads/"+b).Run() == nil {
			return b
		}
	}
	return ""
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/runner/ -run 'Repositor|Checkout' -count=1 -v`
Expected: both PASS (or SKIP with "git is not installed", which must not be the case on this
machine - `which git` prints a path).

- [ ] **Step 5: Format, lint, commit**

```bash
gofmt -l internal cmd && golangci-lint run && go test ./internal/runner/ -count=1
git add internal/runner/projects.go internal/runner/projects_test.go
git commit -m "feat(runner): discover a project's repositories and their main branch"
git push origin main
```

---

### Task 3: One environment per project, loaded on first use

**Files:**
- Modify: `internal/runner/projects.go`
- Test: `internal/runner/projects_test.go`
- Read only: `internal/runner/env.go:275-310` (`Env.Close`, `Env.NewTools` and its
  "environment is closed" error), `internal/runner/env_test.go:34-42` (`load`).

**Interfaces:**
- Consumes: `ProjectsConfig.LoadEnv`.
- Produces:
  - `func (p *Projects) Env(ctx context.Context, id string) (*Env, error)` - loads once, shares
    the load between callers arriving together, records a failure as `ProjectView.LoadError`
    and announces it, retries on the next call; `ErrNoProject` for an unknown id;
    `ErrProjectsClosed` after `Close`.
  - `func (p *Projects) Close()` - closes every loaded environment; idempotent.
  - `var ErrProjectsClosed`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/runner/projects_test.go` (add `"context"`, `"sync"`, `"sync/atomic"` and
`"strings"` to the imports):

```go
// loader counts how many environments it built, and can be made to fail or to
// wait, which is how the tests below see the registry share, record and retry.
type loader struct {
	loads atomic.Int64
	fail  atomic.Bool
	gate  chan struct{}
}

func (l *loader) load(t *testing.T) func(context.Context, string) (*runner.Env, error) {
	return func(ctx context.Context, dir string) (*runner.Env, error) {
		if l.gate != nil {
			<-l.gate
		}
		l.loads.Add(1)
		if l.fail.Load() {
			return nil, errors.New("the SessionStart hook exited 1")
		}
		env, _, err := runner.Load(ctx, runner.Options{Cwd: dir})
		return env, err
	}
}

func newLoadingProjects(t *testing.T, l *loader, notify func(runner.ProjectView)) *runner.Projects {
	t.Helper()
	p, err := runner.NewProjects(runner.ProjectsConfig{LoadEnv: l.load(t), Notify: notify})
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
	l := &loader{gate: make(chan struct{})}
	p := newLoadingProjects(t, l, nil)
	addProject(t, p, t.TempDir(), "")

	var wg sync.WaitGroup
	envs := make([]*runner.Env, 3)
	for i := range envs {
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/runner/ -run 'Environment|Load|Together' -count=1`
Expected: FAIL to compile with `p.Env undefined` and `runner.ErrProjectsClosed` undefined.

- [ ] **Step 3: Write the environment lifecycle**

In `internal/runner/projects.go`, add to the `var (...)` block:

```go
	ErrProjectsClosed = errors.New("runner: the project registry is closed")
```

Replace the `Close` stub and add `Env`:

```go
// Env is the project's environment, loaded on first use and kept for the
// daemon's life.
//
// The load runs without the lock, as OpenRun does: it dials MCP servers and
// runs the SessionStart hook. Callers that arrive together share one load. A
// load that failed is recorded on the project, announced, and tried again by
// the next caller.
func (p *Projects) Env(ctx context.Context, id string) (*Env, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrProjectsClosed
		}
		pr := p.byID[id]
		if pr == nil {
			p.mu.Unlock()
			return nil, ErrNoProject
		}
		if pr.env != nil {
			env := pr.env
			p.mu.Unlock()
			return env, nil
		}
		if pr.loading != nil {
			wait := pr.loading
			p.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		pr.loading = done
		dir, name := pr.rec.Dir, pr.rec.Name
		p.mu.Unlock()

		env, err := p.loadEnv(ctx, dir)

		p.mu.Lock()
		pr.loading = nil
		close(done)
		if p.byID[id] != pr || p.closed {
			p.mu.Unlock()
			if env != nil {
				env.Close()
			}
			if p.closed {
				return nil, ErrProjectsClosed
			}
			return nil, ErrNoProject
		}
		if err != nil {
			pr.loadErr = err.Error()
			v := p.viewLocked(pr)
			p.mu.Unlock()
			p.notify(v)
			return nil, fmt.Errorf("could not load project %s: %w", name, err)
		}
		pr.env = env
		announce := pr.loadErr != ""
		pr.loadErr = ""
		v := p.viewLocked(pr)
		p.mu.Unlock()
		if announce {
			p.notify(v)
		}
		return env, nil
	}
}

// Close releases every loaded environment. It runs after the run registry has
// closed, so no session still needs one. Calling it twice is safe.
func (p *Projects) Close() {
	p.mu.Lock()
	p.closed = true
	var envs []*Env
	for _, pr := range p.byID {
		if pr.env != nil {
			envs = append(envs, pr.env)
			pr.env = nil
		}
	}
	p.mu.Unlock()
	for _, e := range envs {
		e.Close()
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/runner/ -run 'Project|Environment|Load|Together' -count=1 -race -v`
Expected: all PASS, no race reported.

- [ ] **Step 5: Format, lint, commit**

```bash
gofmt -l internal cmd && golangci-lint run && go test ./internal/runner/ -count=1
git add internal/runner/projects.go internal/runner/projects_test.go
git commit -m "feat(runner): load one environment per project on first use"
git push origin main
```

---

### Task 4: A run remembers its project

**Files:**
- Modify: `internal/runner/runs.go:103-135` (`Run`, `RunRequest`), `:361-372` (`Create`
  building `rec`).
- Test: `internal/runner/runs_test.go`

**Interfaces:**
- Produces: `RunRequest.ProjectID string`; `Run.ProjectID string` with json tag
  `projectId,omitempty` (empty means the daemon's own directory). Copied into the record by
  `Create`; the registry does not resolve it - that is `Open`'s job.

- [ ] **Step 1: Write the failing test**

Append to `internal/runner/runs_test.go`:

```go
// The project is the request's to name and the record's to keep: which
// environment it resolves to is Open's business, and the table only has to
// hand it back, after a restart included.
func TestARunRemembersItsProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.json")
	runs := newRuns(t, path, nil, nil)
	v := create(t, runs, runner.RunRequest{ProjectID: "PRJ-3"})
	if v.ProjectID != "PRJ-3" {
		t.Fatalf("ProjectID = %q, want PRJ-3", v.ProjectID)
	}
	if home := create(t, runs, runner.RunRequest{}); home.ProjectID != "" {
		t.Errorf("a run with no project = %q, want empty", home.ProjectID)
	}
	runs.Close()

	again := newRuns(t, path, nil, nil)
	got, err := again.Get(v.ID)
	if err != nil || got.ProjectID != "PRJ-3" {
		t.Errorf("after a restart ProjectID = %q, %v; want PRJ-3", got.ProjectID, err)
	}
}
```

If `filepath` is not already imported in that file, add `"path/filepath"`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/runner/ -run TestARunRemembersItsProject -count=1`
Expected: FAIL to compile with `unknown field ProjectID`.

- [ ] **Step 3: Add the field**

In `internal/runner/runs.go`, in `type Run struct` after `SessionID`:

```go
	// ProjectID names the project the run works in; empty is the daemon's own
	// directory, which is a project with no record.
	ProjectID string `json:"projectId,omitempty"`
```

In `type RunRequest struct` after `Model`:

```go
	// ProjectID selects the environment the run opens in. Open resolves it.
	ProjectID string
```

In `Create`, the `rec := Run{...}` literal gains `ProjectID: req.ProjectID,` after `SessionID:
meta.ID,`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/runner/ -run TestARunRemembersItsProject -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Format, lint, commit**

```bash
gofmt -l internal cmd && golangci-lint run && go test ./internal/runner/ -count=1
git add internal/runner/runs.go internal/runner/runs_test.go
git commit -m "feat(runner): a run record names its project"
git push origin main
```

---

### Task 5: The projects API in `internal/web`

**Files:**
- Create: `internal/web/api_projects.go`, `internal/web/api_projects_test.go`
- Modify: `internal/web/backend.go:159-190` (`Run`, `NewRun`), `internal/web/api_runs.go:290-320`
  (`writeRunError`), `internal/web/api_phase1.go:45-62` (`SkillsBackend`, `CommandsBackend`),
  `:283-352` (`handleSkills`, `handleSkill`, `handleSkillTrust`, `handleCommands`),
  `internal/web/meta.go:25-54` (`featuresFor`), `internal/web/server.go:187-240` (`routes`),
  `internal/web/backend_test.go:97-119` (`fakeBackend.OpenRun`),
  `internal/web/api_phase1_test.go:22-66` (`phaseBackend`), `docs/web.md`.

**Interfaces:**
- Consumes: `backendOf`, `decodeJSON`, `writeJSON`, `writeJSONStatus`, `writeRunError`,
  `unprefixed`, `Refuse`, `methodNotAllowed`, `s.api`.
- Produces (the wire B and C build on):
  - `type ProjectsBackend interface { Projects(ctx) ([]Project, error); AddProject(ctx,
    NewProject) (Project, error); RemoveProject(ctx, id string) error; ProjectRepos(ctx, id
    string) ([]Repository, error) }`
  - `type Project struct { ID, Name, Dir string; Created time.Time; LoadError string }` json
    `id`, `name`, `dir`, `created,omitzero`, `loadError,omitempty`.
  - `type NewProject struct { Dir string; Name string }` json `dir`, `name,omitempty`.
  - `type Repository struct { Name, Dir, Main string }` json `name`, `dir`, `main,omitempty`.
  - `var ErrNoProject` (404), `var ErrConflict` and `func Conflict(reason string) error` (409,
    the reason as the body).
  - `SkillsBackend.Skills(ctx, project string)`, `Skill(ctx, project, name string)`,
    `TrustSkills(ctx, project string)`; `CommandsBackend.Commands(ctx, project string)`. The
    routes read `?project=` and the trust body's `project`.
  - `Run.ProjectID` and `NewRun.ProjectID`, json `projectId,omitempty`.
  - Feature key `projects`; routes `GET|POST /api/projects`, `DELETE /api/projects/{id}`,
    `GET /api/projects/{id}/repos`.

- [ ] **Step 1: Write the failing tests**

Create `internal/web/api_projects_test.go`:

```go
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
		"/api/projects":            http.MethodPut,
		"/api/projects/PRJ-1":      http.MethodGet,
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/web/ -run 'Project|CarryTheProject|CarriesItsProject' -count=1`
Expected: FAIL to compile (`undefined: Project`, `Conflict`, `ErrNoProject`).

- [ ] **Step 3: Write the seam and the handlers**

Create `internal/web/api_projects.go`:

```go
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
```

In `internal/web/api_runs.go`, `writeRunError` gains two cases before `case errors.As(err,
&refusal)`:

```go
	case errors.Is(err, ErrNoProject):
		http.Error(w, "no such project", http.StatusNotFound)
	case errors.Is(err, ErrConflict):
		http.Error(w, unprefixed(err.Error()), http.StatusConflict)
```

In `internal/web/backend.go`:
- `type Run struct` gains, after `SessionID`:
  `ProjectID string `json:"projectId,omitempty"``.
- `type NewRun struct` gains, after `Model`, replacing the comment that begins "There is
  deliberately no Root here" (delete that whole comment):

```go
	// ProjectID names the project the run works in; empty is the daemon's own
	// directory. The run is rooted at the project directory - a request still
	// cannot name a directory of its own, only a project the person added.
	ProjectID string `json:"projectId,omitempty"`
```

In `internal/web/api_phase1.go`:
- `SkillsBackend` becomes:

```go
type SkillsBackend interface {
	Skills(ctx context.Context, project string) (Skills, error)
	Skill(ctx context.Context, project, name string) (Skill, error)
	TrustSkills(ctx context.Context, project string) (SkillApproval, error)
}

type CommandsBackend interface {
	Commands(ctx context.Context, project string) ([]Command, error)
}
```

- `handleSkills`: `v, err := b.Skills(r.Context(), r.URL.Query().Get("project"))`.
- `handleSkill`:
  `v, err := b.Skill(r.Context(), r.URL.Query().Get("project"), r.PathValue("name"))`.
- `handleSkillTrust`: replace `var req struct{}` and its comment with

```go
	// The one option is which project. An empty body and {} mean the daemon's
	// own directory; any other field, a second document or an oversized body is
	// still a malformed request.
	var req struct {
		Project string `json:"project,omitempty"`
	}
```

  and call `b.TrustSkills(r.Context(), req.Project)`.
- `handleCommands`: `v, err := b.Commands(r.Context(), r.URL.Query().Get("project"))`.

In `internal/web/meta.go`, `featuresFor` gains, after the `ModelsBackend` check:

```go
	if _, ok := b.(ProjectsBackend); ok {
		out["projects"] = true
	}
```

In `internal/web/server.go`, `routes()` gains, after the `/api/activity` lines and before
`s.mux.Handle("/", s.assets)`:

```go
	s.api("GET /api/projects", s.handleProjects)
	s.api("POST /api/projects", s.handleAddProject)
	s.mux.HandleFunc("/api/projects", methodNotAllowed("GET, HEAD, POST"))
	s.api("DELETE /api/projects/{id}", s.handleRemoveProject)
	s.mux.HandleFunc("/api/projects/{id}", methodNotAllowed("DELETE"))
	s.api("GET /api/projects/{id}/repos", s.handleProjectRepos)
	s.mux.HandleFunc("/api/projects/{id}/repos", methodNotAllowed("GET, HEAD"))
```

In `internal/web/backend_test.go`, `fakeBackend.OpenRun`'s `run := Run{...}` literal gains
`ProjectID: req.ProjectID,`.

In `internal/web/api_phase1_test.go`, `phaseBackend`'s four methods take the new parameter:

```go
func (b *phaseBackend) Skills(context.Context, string) (Skills, error) { return b.skills, nil }
func (b *phaseBackend) Skill(_ context.Context, _ string, name string) (Skill, error) {
```

(keep each body as it is), `TrustSkills(context.Context, string)`, `Commands(context.Context,
string)`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/web/ -count=1`
Expected: PASS, including `TestTheFeatureMapNamesEverySeamTheBackendImplements` unchanged (the
phase-one fake does not implement the projects seam, so its list is still exact).

- [ ] **Step 5: Document the wire**

In `docs/web.md`:

1. In the "What it serves" table, after the `GET /api/activity` row:

```
| `GET /api/projects` | the registry, the daemon's own directory first with an empty id |
| `POST /api/projects` | `{"dir":"/abs/path","name":"optional"}`; 201, or 400 with why not |
| `DELETE /api/projects/{id}` | forgets the project; 409 while it has an open run; removes no files |
| `GET /api/projects/{id}/repos` | the git checkouts under the project, discovered on demand |
```

2. Replace the paragraph beginning "The screens are sessions, models, skills, activity" with:

```
The screens are sessions, models, skills, activity, worktrees and a run viewer,
plus a command palette and a quick chat. Tickets and tasks are drawn as empty
states that say what they are waiting for: they arrive with the next phase.
```

3. In "The feature map", change the key list to
   `` `controlSocket`, `runs`, `models`, `providerLogin`, `skills`, `commands`, `usage`,
   `activity` and `projects` `` and add after "Without a state directory there is no activity
   feed": "and no project registry;".

4. In "The control stream", the sentence listing kinds gains, before "and `activity.updated`":
   "`project.updated`, carrying the project that was added, removed or whose environment
   failed to load;".

5. Add a section before "## Runs":

```
## Projects

A project is a directory on the daemon's machine, registered by the signed-in
person and kept in `$XDG_STATE_HOME/aigem/projects.json`. Its repositories are
the direct child directories holding a `.git`, plus the directory itself when it
is a checkout; `main` on each is the branch a run will merge into, `main` or
`master`, and absent when the checkout has neither. The daemon's own directory
is a project with no record: it is listed first with an empty `id`, and it is
what a run, the skills catalogue and the command catalogue mean when they name
no project.

Every project has one environment - its skills, hooks, MCP servers and system
prompt - loaded the first time something needs it and kept for the daemon's
life. Loading dials the project's MCP servers and runs its SessionStart hook,
so the first run opened in a project takes as long as those take. A load that
fails is recorded on the project as `loadError`, announced as
`project.updated`, and tried again by the next request; until one succeeds the
project cannot open runs. A project added from the browser is loaded with no
`--trust-project-*` flag: its hooks and MCP servers stay withheld, and its
skills go through `POST /api/skills/trust` with `{"project":"PRJ-1"}`.

`POST /api/runs` takes `projectId`, and a run record carries it. `GET
/api/skills`, `GET /api/skills/{name}` and `GET /api/commands` take
`?project=`; without it they answer for the daemon's own directory.

`DELETE /api/projects/{id}` forgets the project and closes its environment. It
is refused with 409 while the project has an open run, and it never removes
anything from disk - the directory, and in later phases its tickets and
worktrees, stay where they are.
```

6. In "Runs", the `400` bullet's mention "It may name a model, a provider or a path" already
   covers a project directory; no change. In "Statuses a client has to tell apart", add a bullet
   after `409`:

```
- `409` on `DELETE /api/projects/{id}` - the project has an open run. The body
  says so; close the run and ask again.
```

- [ ] **Step 6: Format, lint, full tests, commit**

```bash
gofmt -l internal cmd && golangci-lint run && go test ./... -count=1
git add internal/web docs/web.md
git commit -m "feat(web): the projects seam, its routes, and a project on runs and skills"
```

`go test ./...` fails to compile `cmd/aigem` here because `webBackend` no longer satisfies the
skills and commands seams. That is expected between this task and the next: run
`go test ./internal/... -count=1` for this commit, leave `git push` out of this task, and push
both commits at the end of Task 6, when the tree builds again.

---

### Task 6: Wire the registry into the daemon

**Files:**
- Create: `cmd/aigem/webprojects.go`, `cmd/aigem/webprojects_test.go`
- Modify: `cmd/aigem/webbackend.go:26-60` (struct), `:72-84` (interface assertions), `:87-115`
  (`webBackendConfig`, `newWebBackend`), `:120-133` (`Unavailable`), `:168-183` (`OpenRun`),
  `:445-455` (`webRun`), `:456-490` (`webRunError`); `cmd/aigem/webphase1.go:391-560` (the
  skills and commands methods); `cmd/aigem/webruns.go:44-48` (`webRuntime`), `:70-90`
  (`openRun`'s environment), `:200-204` (the notifier); `cmd/aigem/webcmd.go:170-205`.
- Read only: `cmd/aigem/webbackend_test.go:49-90` (`testRuns`), `cmd/aigem/webphase1_test.go:647`
  (`writeProjectSkill`).

**Interfaces:**
- Consumes: `runner.NewProjects`, `(*runner.Projects).List/Add/Remove/Repositories/Env/Close`,
  `runner.ErrNoProject`, `runner.ErrBadProject`, `web.ProjectsBackend`, `web.Conflict`,
  `web.ErrNoProject`, `web.Refuse`.
- Produces:
  - `webBackendConfig.projects *runner.Projects`; `webBackend.projects`.
  - `func (b *webBackend) envFor(ctx, project string) (*runner.Env, error)`.
  - `webRuntime.projects *runner.Projects`.
  - `func (n *notifier) publishProject(v runner.ProjectView)`.
  - The `projects` feature is withdrawn when the registry is nil (no state directory).

- [ ] **Step 1: Write the failing tests**

Create `cmd/aigem/webprojects_test.go`:

```go
package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

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

func TestRemovingAProjectWithAnOpenRunIsAConflict(t *testing.T) {
	runs, _ := testRuns(t)
	b := newWebBackend(webBackendConfig{runs: runs, projects: testProjects(t, nil)})
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/aigem/ -run 'Project|SkillsAreRead|ResolvesTheProject' -count=1`
Expected: FAIL to compile (`unknown field projects`, methods missing).

- [ ] **Step 3: Write the adapter**

Create `cmd/aigem/webprojects.go`:

```go
package main

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/web"
)

// The project half of the backend: the registry's answers translated, and the
// one question every scoped route asks - which environment a project id means.

func (b *webBackend) Projects(context.Context) ([]web.Project, error) {
	if b.projects == nil {
		return nil, web.ErrUnavailable
	}
	out := []web.Project{}
	if b.env != nil {
		out = append(out, web.Project{Name: filepath.Base(b.env.Cwd), Dir: b.env.Cwd})
	}
	for _, v := range b.projects.List() {
		out = append(out, webProject(v))
	}
	return out, nil
}

func (b *webBackend) AddProject(_ context.Context, req web.NewProject) (web.Project, error) {
	if b.projects == nil {
		return web.Project{}, web.ErrUnavailable
	}
	if b.env != nil && filepath.Clean(req.Dir) == b.env.Cwd {
		return web.Project{}, web.Refuse(errors.New(
			"that is this daemon's own directory, which is already the first project"))
	}
	v, err := b.projects.Add(req.Dir, req.Name)
	if err != nil {
		return web.Project{}, webProjectError(err)
	}
	b.recordActivity(web.Activity{Kind: "project.added", Text: "Added project " + v.Name})
	return webProject(v), nil
}

func (b *webBackend) RemoveProject(_ context.Context, id string) error {
	if b.projects == nil {
		return web.ErrUnavailable
	}
	if b.runs != nil {
		for _, r := range b.runs.List() {
			if r.Live && r.ProjectID == id {
				return web.Conflict("this project has an open run; close it first")
			}
		}
	}
	if err := b.projects.Remove(id); err != nil {
		return webProjectError(err)
	}
	b.recordActivity(web.Activity{Kind: "project.removed", Text: "Removed project " + id})
	return nil
}

func (b *webBackend) ProjectRepos(_ context.Context, id string) ([]web.Repository, error) {
	if b.projects == nil {
		return nil, web.ErrUnavailable
	}
	repos, err := b.projects.Repositories(id)
	if err != nil {
		return nil, webProjectError(err)
	}
	out := make([]web.Repository, 0, len(repos))
	for _, r := range repos {
		out = append(out, web.Repository{Name: r.Name, Dir: r.Dir, Main: r.Main})
	}
	return out, nil
}

// envFor is the environment a request means: the daemon's own for an empty
// id, otherwise the project's, loaded on first use.
func (b *webBackend) envFor(ctx context.Context, project string) (*runner.Env, error) {
	if project == "" {
		if b.env == nil {
			return nil, web.ErrUnavailable
		}
		return b.env, nil
	}
	if b.projects == nil {
		return nil, web.Refuse(errors.New("this daemon serves no projects"))
	}
	env, err := b.projects.Env(ctx, project)
	if err != nil {
		return nil, webProjectError(err)
	}
	return env, nil
}

func webProject(v runner.ProjectView) web.Project {
	return web.Project{ID: v.ID, Name: v.Name, Dir: v.Dir, Created: v.Created, LoadError: v.LoadError}
}

// webProjectError classifies what the registry reports. A bad directory and a
// load that failed are both sentences for the person who typed the path.
func webProjectError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runner.ErrNoProject):
		return web.ErrNoProject
	case errors.Is(err, runner.ErrProjectsClosed):
		return err
	default:
		return web.Refuse(err)
	}
}
```

In `cmd/aigem/webbackend.go`:
- The struct gains `projects *runner.Projects` after `env *runner.Env`.
- Replace `pendingAt time.Time` and `pendingVal *skill.PendingSkills` with
  `pending map[string]pendingMemo` and add, after the struct:

```go
// pendingMemo is what a project's skills are waiting on, and when that was read.
type pendingMemo struct {
	at  time.Time
	val *skill.PendingSkills
}
```

- The assertions block gains `_ web.ProjectsBackend = (*webBackend)(nil)`.
- `webBackendConfig` gains `projects *runner.Projects` after `env`.
- `newWebBackend` passes `projects: cfg.projects` and `pending: map[string]pendingMemo{}`.
- `Unavailable` gains:

```go
	if b.projects == nil {
		out = append(out, "projects")
	}
```

- `OpenRun` passes `ProjectID: req.ProjectID` in the `runner.RunRequest`.
- `webRun` gains `ProjectID: v.ProjectID,` after `SessionID: v.SessionID,`.
- `webRunError` gains, before `case errors.Is(err, runner.ErrRunsClosed)`:

```go
	case errors.Is(err, runner.ErrNoProject):
		return web.ErrNoProject
```

In `cmd/aigem/webphase1.go`, the skills half becomes project-scoped:

```go
func (b *webBackend) Skills(ctx context.Context, project string) (web.Skills, error) {
	env, err := b.envFor(ctx, project)
	if errors.Is(err, web.ErrUnavailable) {
		return web.Skills{Items: []web.SkillSummary{}}, nil
	}
	if err != nil {
		return web.Skills{}, err
	}
	b.skillMu.Lock()
	defer b.skillMu.Unlock()
	out := web.Skills{Items: skillSummaries(env.Skills)}
	pending, err := b.pendingSkillsLocked(project, env)
	if err != nil {
		slog.Warn("the project's pending skills could not be read", "project", project, "err", err)
	}
	if pending != nil {
		out.Pending = &web.PendingSkills{
			Names: append([]string(nil), pending.Names...), Invalidated: pending.Invalidated,
		}
	}
	return out, nil
}

func (b *webBackend) Skill(ctx context.Context, project, name string) (web.Skill, error) {
	env, err := b.envFor(ctx, project)
	if errors.Is(err, web.ErrUnavailable) {
		return web.Skill{}, web.ErrNoSkill
	}
	if err != nil {
		return web.Skill{}, err
	}
	b.skillMu.Lock()
	defer b.skillMu.Unlock()
	sk, ok := env.Skills.Get(name)
	// ... the rest of the method body is unchanged from here ...
```

`TrustSkills(ctx, project)`: replace the `if b.env == nil` refusal with `env, err :=
b.envFor(ctx, project)`, returning `err` when it is not nil; then `skill.Pending(env.Cwd)`,
`b.forgetPendingLocked(project)`, `env.ApproveProjectSkills()`. Keep every comment and the
error mapping as they are.

`pendingSkillsLocked` and `forgetPendingLocked` become per project:

```go
func (b *webBackend) pendingSkillsLocked(project string, env *runner.Env) (*skill.PendingSkills, error) {
	if m, ok := b.pending[project]; ok && time.Since(m.at) < pendingSkillsTTL {
		return m.val, nil
	}
	got, err := skill.Pending(env.Cwd)
	if err != nil {
		return nil, err
	}
	b.pending[project] = pendingMemo{at: time.Now(), val: got}
	return got, nil
}

func (b *webBackend) forgetPendingLocked(project string) {
	delete(b.pending, project)
}
```

`Commands(ctx, project)`: `env, err := b.envFor(ctx, project)`; `ErrUnavailable` answers
`[]web.Command{}, nil` as today's `b.env == nil` branch does; any other error is returned;
then `uisession.Commands(env.Skills, env.MCP)` under `skillMu` as now.

`webphase1.go` does not import `internal/runner` today; add
`"github.com/gigovich/aigem/internal/runner"` for the `*runner.Env` parameter.

Every existing test in `cmd/aigem` that calls `b.Skills(ctx)`, `b.Skill(ctx, name)`,
`b.TrustSkills(ctx)` or `b.Commands(ctx)` gets the project argument `""` added.

In `cmd/aigem/webruns.go`:
- `webRuntime` gains `projects *runner.Projects`.
- `openRun` starts with the environment:

```go
	env := rt.env
	if req.ProjectID != "" {
		if rt.projects == nil {
			return nil, runner.Opened{}, errors.New("this daemon serves no projects")
		}
		var err error
		if env, err = rt.projects.Env(ctx, req.ProjectID); err != nil {
			return nil, runner.Opened{}, err
		}
	}
	if env == nil {
		return nil, runner.Opened{}, errors.New("no environment is loaded")
	}
	reg, err := env.NewTools()
```

  and every later `rt.env` in the function becomes `env` (`SystemPrompt`, `SessionTitle`,
  `Agents`, `Skills`, `Hooks`, `Project`, `Cwd`, `HandleCommands(env.Skills, env.MCP)`,
  `env.Attach`, `Root: env.Cwd`, `env.Detach`). The `_ context.Context` parameter becomes
  `ctx context.Context`.
- The notifier gains:

```go
func (n *notifier) publishProject(v runner.ProjectView) { n.publish("project.updated", webProject(v)) }
```

In `cmd/aigem/webcmd.go`, after `var announce notifier` and before `rt.newRuns`:

```go
	// The registry before the runs, so its deferred Close runs after theirs: a
	// session being saved needs the environment its SessionEnd hook runs in.
	// Without a state directory there is no registry, and the page is told so
	// through the feature map.
	var projects *runner.Projects
	if stateDir != "" {
		projects, err = runner.NewProjects(runner.ProjectsConfig{
			Store: store.New[[]runner.Project](filepath.Join(stateDir, "projects.json")),
			LoadEnv: func(ctx context.Context, dir string) (*runner.Env, error) {
				env, _, err := runner.Load(ctx, runner.Options{
					Cwd: dir, Version: versionString(), Search: searchCfg,
					Notify: func(n runner.Notice) { fmt.Fprintln(os.Stderr, "warning:", dir+":", n.Text) },
				})
				return env, err
			},
			Notify: announce.publishProject,
		})
		if err != nil {
			return err
		}
		defer projects.Close()
	}
	rt := &webRuntime{env: env, models: defaultModelRegistry(), projects: projects}
```

(remove the earlier `rt := &webRuntime{env: env, models: defaultModelRegistry()}` line) and
`newWebBackend(webBackendConfig{... env: env, projects: projects, ...})`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./cmd/aigem/ -count=1 && go test ./... -count=1`
Expected: PASS everywhere.

- [ ] **Step 5: Format, lint, deploy, check with curl**

```bash
gofmt -l internal cmd && golangci-lint run
make web && make install && systemctl --user restart aigem-web
sleep 2 && curl -s https://tba.tail74d52.ts.net/healthz
```

Then:

```bash
TOKEN=$(journalctl --user -u aigem-web --no-pager | grep -o 'token=[^ ]*' | tail -1 | cut -d= -f2)
H="Authorization: Bearer $TOKEN"; U=https://tba.tail74d52.ts.net
curl -s -H "$H" $U/api/meta | grep -o '"projects":true'
curl -s -H "$H" $U/api/projects
curl -s -H "$H" -X POST -d '{"dir":"/home/gigovich/work/devinlab"}' $U/api/projects
curl -s -H "$H" $U/api/projects/PRJ-1/repos
curl -s -H "$H" -X POST -d '{"dir":"nope"}' $U/api/projects -w ' %{http_code}\n'
curl -s -H "$H" -X DELETE $U/api/projects/PRJ-1 -w '%{http_code}\n'
```

Expected: `"projects":true`; the list starts with `{"id":"","name":"work",...}`; the add
answers a `PRJ-` record; repos lists `aigem` with `"main":"main"`; the bad path answers 400
with a sentence; the delete answers 204. (The id is `PRJ-1` only on a fresh registry; read it
from the add's answer.)

- [ ] **Step 6: Commit**

```bash
git add cmd/aigem
git commit -m "feat(aigem): serve projects - a registry, one environment each, scoped runs"
git push origin main
```

---

### Task 7: The client's wire, API client, store and test harness

**Files:**
- Modify: `internal/web/_ui/src/lib/wire.ts`, `src/lib/api.ts`, `src/state/app.ts`,
  `src/test/harness.tsx`
- Test: `internal/web/_ui/src/state/app.test.ts`

**Interfaces:**
- Produces:
  - `wire.ts`: `Feature` gains `'projects'`; `Project`, `NewProject`, `Repository` types;
    `Run.projectId?`, `NewRun.projectId?`; `ControlKind.ProjectUpdated = 'project.updated'`.
  - `api.ts`: `projects()`, `addProject(req)`, `removeProject(id)`, `projectRepos(id)`;
    `skills(project)`, `skill(name, project)`, `trustSkills(project)`, `commands(project)`.
  - `app.ts`: `AppState.projects: Project[]`, `AppState.project: string`;
    `selectProject(id)`, `currentProject(s)`, `inProject(runs, project)`;
    `refresh.projects`; `openSession()` opens in the current project.
  - `harness.tsx`: `Daemon.projects`, `DAEMON_PROJECT`, `META.features.projects`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/web/_ui/src/state/app.test.ts`:

```ts
function daemonWith(paths: string[], projects: unknown[] = []) {
  vi.stubGlobal(
    'fetch',
    vi.fn((path: string, init?: RequestInit) => {
      paths.push(`${init?.method ?? 'GET'} ${path}`)
      if (path === '/api/projects') return Promise.resolve(new Response(JSON.stringify(projects)))
      if (path === '/api/runs') return Promise.resolve(new Response(JSON.stringify([]), { status: 200 }))
      if (path.startsWith('/api/skills')) return Promise.resolve(new Response('{"items":[]}'))
      return Promise.resolve(new Response('[]', { status: 200 }))
    }),
  )
  store.set((s) => ({
    ...s,
    meta: {
      version: '',
      defaultModel: '',
      rev: 0,
      ui: true,
      features: { projects: true, skills: true, commands: true, runs: true },
    },
  }))
}

// The selection is what every list screen reads, so changing it has to bring
// the project's own catalogues with it - and survive a reload.
test('selecting a project saves it and reads its skills and commands', async () => {
  const paths: string[] = []
  daemonWith(paths)
  const { selectProject } = await import('./app')
  selectProject('PRJ-1')
  await vi.waitFor(() => {
    expect(paths).toContain('GET /api/skills?project=PRJ-1')
    expect(paths).toContain('GET /api/commands?project=PRJ-1')
  })
  expect(store.get().project).toBe('PRJ-1')
  expect(window.localStorage.getItem('aigem.project')).toBe('PRJ-1')
  window.localStorage.removeItem('aigem.project')
})

// A project forgotten in another tab, or on another day, must not leave this
// tab asking for catalogues of a project the daemon no longer lists.
test('a saved project the daemon no longer lists is forgotten', async () => {
  const paths: string[] = []
  daemonWith(paths, [{ id: '', name: 'work', dir: '/w' }])
  store.set((s) => ({ ...s, project: 'PRJ-7' }))
  const { refresh } = await import('./app')
  await refresh.projects()
  expect(store.get().project).toBe('')
})

test('a new session opens in the current project', async () => {
  const paths: string[] = []
  daemonWith(paths)
  vi.stubGlobal(
    'fetch',
    vi.fn((path: string, init?: RequestInit) => {
      paths.push(`${init?.method ?? 'GET'} ${path} ${typeof init?.body === 'string' ? init.body : ''}`)
      if (path === '/api/runs' && init?.method === 'POST') {
        return Promise.resolve(new Response('{"id":"RUN-1","status":"open","live":true}', { status: 201 }))
      }
      return Promise.resolve(new Response('[]', { status: 200 }))
    }),
  )
  store.set((s) => ({ ...s, project: 'PRJ-1' }))
  const { openSession } = await import('./app')
  await openSession()
  expect(paths).toContain('POST /api/runs {"projectId":"PRJ-1"}')
})

// Which conversation a tab means depends on the project it is looking at.
test('the fallback conversation is one in the current project', () => {
  const runs = [run({ id: 'a', projectId: 'PRJ-1' }), run({ id: 'b' })]
  const { inProject } = require('./app') as typeof import('./app')
  expect(inProject(runs, 'PRJ-1').map((r) => r.id)).toEqual(['a'])
  expect(inProject(runs, '').map((r) => r.id)).toEqual(['b'])
})
```

Replace the last test's `require` with a top-level import: add `inProject` to the existing
`import { compose, initialState, latestRunId, runCounts, store } from './app'` line and drop the
`require` line.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd internal/web/_ui && npx vitest run src/state/app.test.ts`
Expected: FAIL (`inProject` is not exported; `selectProject` is not exported).

- [ ] **Step 3: The wire**

In `src/lib/wire.ts`:
- `Feature` gains `| 'projects'`.
- `Run` gains `projectId?: string` after `sessionId?`; `NewRun` becomes
  `{ mode?: string; title?: string; model?: string; projectId?: string }`.
- After `NewRun` add:

```ts
/** A project: a directory on the daemon's machine. The daemon's own has an empty id. */
export type Project = { id: string; name: string; dir: string; created?: string; loadError?: string }

export type NewProject = { dir: string; name?: string }

/** A git checkout under a project. `name` is empty for the project directory itself. */
export type Repository = { name: string; dir: string; main?: string }
```

- `ControlKind` gains `ProjectUpdated: 'project.updated',`.

- [ ] **Step 4: The API client**

In `src/lib/api.ts`, import `NewProject`, `Project`, `Repository` from `./wire`, and:
- Replace the four skills/commands entries:

```ts
  skills: (project = '', signal?: AbortSignal) =>
    json<Skills>(`/api/skills${query({ project })}`, { signal }),
  skill: (name: string, project = '', signal?: AbortSignal) =>
    json<Skill>(`/api/skills/${encodeURIComponent(name)}${query({ project })}`, { signal }),
  trustSkills: (project = '', signal?: AbortSignal) =>
    json<SkillApproval>('/api/skills/trust', { ...body(project ? { project } : {}), signal }),

  commands: (project = '', signal?: AbortSignal) =>
    json<Command[]>(`/api/commands${query({ project })}`, { signal }),
```

- Add, before `models:`:

```ts
  projects: (signal?: AbortSignal) => json<Project[]>('/api/projects', { signal }),
  addProject: (req: NewProject, signal?: AbortSignal) =>
    json<Project>('/api/projects', { ...body(req), signal }),
  removeProject: async (id: string, signal?: AbortSignal) => {
    await send(`/api/projects/${encodeURIComponent(id)}`, { method: 'DELETE', signal })
  },
  projectRepos: (id: string, signal?: AbortSignal) =>
    json<Repository[]>(`/api/projects/${encodeURIComponent(id)}/repos`, { signal }),
```

`query` already drops an empty value, so `skills('')` still asks `/api/skills`.

- [ ] **Step 5: The store**

In `src/state/app.ts`:
- Import `Project` in the type import from `@/lib/wire`.
- `AppState` gains, after `activity: Activity[]`:

```ts
  projects: Project[]
  /** The project every list screen reads; empty is the daemon's own directory. Saved per browser. */
  project: string
```

- `initialState()` gains `projects: [],` and `project: read('aigem.project') ?? '',`.
- `refresh` gains `projects` and scopes skills and commands:

```ts
export const refresh = {
  runs: () => load('runs', 'runs', () => api.runs()),
  models: () => load('models', 'models', () => api.models()),
  skills: () => load('skills', 'skills', () => api.skills(store.get().project)),
  commands: () => load('commands', 'commands', () => api.commands(store.get().project)),
  usage: () => load('usage', 'usage', () => api.usage()),
  activity: () => load('activity', 'activity', readActivityTail),
  projects: async () => {
    await load('projects', 'projects', () => api.projects())
    // A saved selection the daemon no longer lists - forgotten elsewhere, or a
    // daemon without a registry - falls back to the daemon's own directory.
    const { projects, project } = store.get()
    if (project && !projects.some((p) => p.id === project)) selectProject('')
  },
}
```

- `refreshAll` reads projects first, because the other reads are scoped by the selection it
  may correct:

```ts
export async function refreshAll() {
  await refresh.projects()
  await Promise.all([
    refresh.runs(),
    refresh.models(),
    refresh.skills(),
    refresh.commands(),
    refresh.usage(),
    refresh.activity(),
  ])
}
```

- Add after `runCounts`:

```ts
/** The runs that belong to a project; an absent id on a record is the daemon's own directory. */
export function inProject(runs: Run[], project: string): Run[] {
  return runs.filter((r) => (r.projectId ?? '') === project)
}

export function currentProject(s: AppState): Project | undefined {
  return s.projects.find((p) => p.id === s.project)
}

/** Choose the project every list screen reads, and fetch what is scoped by it. */
export function selectProject(id: string) {
  if (store.get().project === id) return
  write('aigem.project', id)
  patch({ project: id })
  void refresh.skills()
  void refresh.commands()
}
```

- `conversationId` scopes the fallback:

```ts
export function conversationId(
  route: Route,
  runs: Run[],
  activeRun: string,
  project = '',
): string | undefined {
  if ((route.screen === 'run' || route.screen === 'chat') && route.id) return route.id
  return latestRunId(inProject(runs, project), activeRun)
}
```

  and `compose` passes `store.get().project` as the fourth argument.
- `openSession` opens in the current project: replace `await api.openRun({})` with

```ts
    const project = store.get().project
    const run = await api.openRun(project ? { projectId: project } : {})
```

- The control switch gains, before `case CLIENT_ERROR`:

```ts
        case ControlKind.ProjectUpdated:
          void refresh.projects()
          break
```

In `src/App.tsx`, `runId` passes the project:

```ts
  const runId = useMemo(
    () => conversationId(route, runs, activeRun, app.project) ?? '',
    [route, activeRun, runs, app.project],
  )
```

- [ ] **Step 6: The harness**

In `src/test/harness.tsx`:
- Import `Project` in the type import.
- `Daemon` gains `projects?: Project[]`.
- After `RUN`, add:

```ts
/** The daemon's own directory, as the registry lists it: first, with no id. */
export const DAEMON_PROJECT: Project = { id: '', name: 'aigem', dir: '/home/dev/aigem' }
```

- `META.features` gains `projects: true`.
- In `installDaemon`, before `store.set(initialState())`:

```ts
  window.localStorage.removeItem('aigem.project')
```

- In the fetch stub, replace the skills and commands lines and add projects:

```ts
      if (path === '/api/projects') return Promise.resolve(ok(daemon.projects ?? [DAEMON_PROJECT]))
      if (/^\/api\/projects\/[^/]+\/repos$/.test(path)) return Promise.resolve(ok([]))
      if (path === '/api/skills' || path.startsWith('/api/skills?')) {
        return Promise.resolve(ok(daemon.skills ?? { items: [] }))
      }
      if (path === '/api/commands' || path.startsWith('/api/commands?')) {
        return Promise.resolve(ok(daemon.commands ?? []))
      }
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `cd internal/web/_ui && npx vitest run src/state/app.test.ts && make -C ../../.. web-check`
Expected: the four new tests PASS; `web-check` is clean. If `screens.test.tsx`'s skill detail
test fails because its custom route is keyed `/api/skills/code-review` while the page now asks
`/api/skills/code-review` with no query (the daemon's directory is `''`), nothing changes; if it
asks with a query, the custom route key must include it - it must not, so investigate before
editing the test.

- [ ] **Step 8: Deploy, check, commit**

Deploy per Global Constraints. Nothing is visible yet; the check is that the page still comes
up: with playwright open `/chat`, wait for the list named `Sessions`, and assert no response
was 4xx or 5xx and no console error was logged.

```bash
git add internal/web/_ui/src
git commit -m "feat(web): the client knows projects - wire, api, a saved selection, scoped reads"
git push origin main
```

---

### Task 8: The sidebar's Projects block, the New project dialog, and `/projects/{id}`

**Files:**
- Create: `internal/web/_ui/src/screens/NewProjectDialog.tsx`,
  `internal/web/_ui/src/screens/projects.test.tsx`
- Modify: `internal/web/_ui/src/shell/Sidebar.tsx`, `src/App.tsx`, `src/lib/route.ts`
- Read only: `src/ui/Modal.tsx` (props: `title`, `onClose`, `confirm`, `width`),
  `src/shell/keymap.ts:76-82` (a modal's mod+Enter), `src/state/app.ts` (`selectProject`).

**Interfaces:**
- Consumes: `selectProject`, `refresh.projects`, `api.addProject`, `explain`, `flash`,
  `Modal`, `replace` from `@/lib/route`.
- Produces: `SCREENS` gains `'projects'`; `NewProjectDialog({ onClose })`; the sidebar's list
  `aria-label="Projects"` with one button per project, `aria-current="true"` on the chosen one;
  the "+" button (`aria-label="New project"`) opens the dialog titled "New project".

- [ ] **Step 1: Write the failing tests**

Create `internal/web/_ui/src/screens/projects.test.tsx`:

```tsx
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'
import type { Project } from '@/lib/wire'
import { DAEMON_PROJECT, mountApp } from '@/test/harness'

afterEach(() => {
  vi.unstubAllGlobals()
  window.localStorage.removeItem('aigem.project')
})

const WORK: Project = { id: 'PRJ-1', name: 'work', dir: '/home/dev/work' }
const PROJECTS = [DAEMON_PROJECT, WORK]

test('the sidebar lists the projects and remembers the chosen one', async () => {
  const user = userEvent.setup()
  const h = await mountApp({ projects: PROJECTS })
  const list = await screen.findByRole('list', { name: 'Projects' })
  expect(within(list).getByRole('button', { name: /aigem/ })).toHaveAttribute('aria-current', 'true')

  await user.click(within(list).getByRole('button', { name: /work/ }))
  expect(within(list).getByRole('button', { name: /work/ })).toHaveAttribute('aria-current', 'true')
  expect(window.localStorage.getItem('aigem.project')).toBe('PRJ-1')
  await waitFor(() => expect(h.paths).toContain('/api/skills?project=PRJ-1'))
})

test('a project whose environment failed to load says so', async () => {
  await mountApp({ projects: [DAEMON_PROJECT, { ...WORK, loadError: 'hook exited 1' }] })
  const list = await screen.findByRole('list', { name: 'Projects' })
  expect(within(list).getByRole('button', { name: /work/ })).toHaveTextContent('failed to load')
})

test('a new project is added from the dialog, and a refusal is shown as text', async () => {
  const user = userEvent.setup()
  let attempts = 0
  const h = await mountApp({
    projects: PROJECTS,
    routes: {
      'POST /api/projects': () => {
        attempts++
        return attempts === 1
          ? new Response('cannot add that project: /nope is not a directory', { status: 400 })
          : new Response(JSON.stringify({ id: 'PRJ-2', name: 'thing', dir: '/home/dev/thing' }), {
              status: 201,
            })
      },
    },
  })
  await user.click(await screen.findByRole('button', { name: 'New project' }))
  const dialog = await screen.findByRole('dialog', { name: 'New project' })
  await user.type(within(dialog).getByRole('textbox', { name: /Directory/ }), '/nope')
  await user.click(within(dialog).getByRole('button', { name: 'Add project' }))
  expect(await within(dialog).findByRole('alert')).toHaveTextContent('/nope is not a directory')

  await user.clear(within(dialog).getByRole('textbox', { name: /Directory/ }))
  await user.type(within(dialog).getByRole('textbox', { name: /Directory/ }), '/home/dev/thing')
  await user.click(within(dialog).getByRole('button', { name: 'Add project' }))
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'New project' })).not.toBeInTheDocument())
  expect(h.sent.filter((r) => r.path === '/api/projects').pop()?.body).toBe('{"dir":"/home/dev/thing"}')
  expect(window.localStorage.getItem('aigem.project')).toBe('PRJ-2')
})

test('/projects/{id} selects the project and lands on the sessions', async () => {
  await mountApp({ projects: PROJECTS, path: '/projects/PRJ-1' })
  await waitFor(() => expect(window.location.pathname).toBe('/chat'))
  const list = await screen.findByRole('list', { name: 'Projects' })
  expect(within(list).getByRole('button', { name: /work/ })).toHaveAttribute('aria-current', 'true')
})

test('without the projects feature the sidebar offers nothing to add', async () => {
  await mountApp({ meta: { features: { controlSocket: true, runs: true } } })
  await screen.findByRole('navigation', { name: 'Navigation' })
  expect(screen.queryByRole('button', { name: 'New project' })).not.toBeInTheDocument()
  expect(screen.getByText("This daemon's directory.")).toBeInTheDocument()
})
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd internal/web/_ui && npx vitest run src/screens/projects.test.tsx`
Expected: FAIL - no list named "Projects"; the dialog is the "later phase" one.

- [ ] **Step 3: The route**

In `src/lib/route.ts`, `SCREENS` gains `'projects'` after `'tickets'`, and the doc comment's
"eight screens" becomes "nine screens" in both places it appears.

- [ ] **Step 4: The dialog**

Create `src/screens/NewProjectDialog.tsx`:

```tsx
import { useState } from 'react'
import { api } from '@/lib/api'
import { explain, flash, refresh, selectProject } from '@/state/app'
import { Modal } from '@/ui/Modal'

const INPUT =
  'h-[28px] rounded-md border border-line bg-bg px-2 font-mono text-[12px] text-fg outline-none focus:border-primary'

/**
 * Add a project: a directory on the daemon's machine, and optionally a name.
 *
 * The daemon's refusal is shown inside the dialog as text - it names the path
 * that was typed and why it would not do - rather than as a banner behind it.
 */
export function NewProjectDialog({ onClose }: { onClose: () => void }) {
  const [dir, setDir] = useState('')
  const [name, setName] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const ready = dir.trim() !== '' && !busy

  const add = async () => {
    if (!ready) return
    setBusy(true)
    setError('')
    try {
      const trimmed = name.trim()
      const project = await api.addProject(trimmed ? { dir: dir.trim(), name: trimmed } : { dir: dir.trim() })
      await refresh.projects()
      selectProject(project.id)
      flash(`Added ${project.name}`)
      onClose()
    } catch (err) {
      setError(explain(err))
      setBusy(false)
    }
  }

  return (
    <Modal
      title="New project"
      onClose={onClose}
      width={480}
      confirm={{ label: busy ? 'Adding…' : 'Add project', onClick: () => void add(), disabled: !ready }}
    >
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          void add()
        }}
      >
        <label className="flex flex-col gap-1 text-[11.5px] text-fg-subtle">
          Directory on the daemon's machine
          <input
            value={dir}
            onChange={(e) => setDir(e.target.value)}
            placeholder="/home/you/work/thing"
            spellCheck={false}
            className={INPUT}
          />
        </label>
        <label className="flex flex-col gap-1 text-[11.5px] text-fg-subtle">
          Name, if not the directory's
          <input value={name} onChange={(e) => setName(e.target.value)} className={INPUT} />
        </label>
        {error && (
          <p role="alert" className="m-0 text-[11.5px] text-attention">
            {error}
          </p>
        )}
      </form>
    </Modal>
  )
}
```

- [ ] **Step 5: The sidebar**

In `src/shell/Sidebar.tsx`:
- Import `selectProject` from `@/state/app` (add to the existing import).
- The `useApp` selector gains `projects: s.projects, project: s.project`.
- Replace the Projects block (`<div className="px-[6px]">` ... `</div>` containing the `<h2>`
  and the "This daemon's directory." paragraph) with:

```tsx
      <div className="px-[6px]">
        <div className="flex items-center pt-0 pr-1 pb-[5px] pl-2">
          <h2 className="m-0 text-[10px] font-semibold tracking-[.07em] text-fg-subtle uppercase">
            Projects
          </h2>
          {features.projects === true && (
            <button
              type="button"
              onClick={onNewProject}
              aria-label="New project"
              title="New project"
              className="ml-auto grid size-[18px] cursor-pointer place-items-center rounded-[4px] text-[13px] text-fg-subtle hover:bg-s0 hover:text-fg"
            >
              <span aria-hidden="true">+</span>
            </button>
          )}
        </div>
        {features.projects === true ? (
          <ul aria-label="Projects" className="m-0 list-none p-0">
            {projects.map((p) => {
              const active = p.id === project
              return (
                <li key={p.id}>
                  <button
                    type="button"
                    aria-current={active ? 'true' : undefined}
                    title={p.loadError ?? p.dir}
                    onClick={() => {
                      selectProject(p.id)
                      setNav(false)
                    }}
                    className={`flex h-[26px] w-full cursor-pointer items-center gap-2 rounded-[5px] px-2 text-left hover:bg-s0 ${
                      active ? 'bg-s0 font-medium text-fg' : 'text-fg-muted'
                    }`}
                  >
                    <span aria-hidden="true" className="w-[13px] text-center text-[11px] text-fg-subtle">
                      {p.id ? '▪' : '⌂'}
                    </span>
                    <span className="min-w-0 flex-1 overflow-hidden text-ellipsis whitespace-nowrap">
                      {p.name}
                    </span>
                    {p.loadError && (
                      <>
                        <span aria-hidden="true" className="text-[10px] text-attention">
                          !
                        </span>
                        <span className="sr-only">failed to load</span>
                      </>
                    )}
                  </button>
                </li>
              )
            })}
          </ul>
        ) : (
          <p className="m-0 px-2 pb-1 text-[11px] text-fg-subtle">This daemon's directory.</p>
        )}
      </div>
```

The `feature` typing: `features` is `Partial<Record<Feature, boolean>>`, so
`features.projects` type-checks once Task 7 added the key.

- [ ] **Step 6: The shell**

In `src/App.tsx`:
- Import `NewProjectDialog` from `@/screens/NewProjectDialog` and `replace` from
  `@/lib/route` (add to the existing import), and `selectProject` from `@/state/app`.
- Rename the state `explainProjects`/`setExplainProjects` to `newProject`/`setNewProject`
  everywhere in the file (the `layerOpen` line, the keymap's `modalOpen`, `closeLayers`,
  `submitModal`, the sidebar's `onNewProject`).
- Replace the `{explainProjects && (<Modal title="Projects arrive in a later phase" ...>)}` block
  with `{newProject && <NewProjectDialog onClose={() => setNewProject(false)} />}`.
- `submitModal` becomes `() => undefined` (the dialog's own form handles Enter; mod+Enter does
  nothing).
- `Screen` gains a case before `default`:

```tsx
    case 'projects':
      return <ProjectSelect id={route.id} />
```

  and after `Screen` add:

```tsx
/** `/projects/{id}` is a selection, not a screen: choose the project and go to its sessions. */
function ProjectSelect({ id }: { id?: string }) {
  useEffect(() => {
    selectProject(id ?? '')
    replace({ screen: 'chat' })
  }, [id])
  return null
}
```

- `TITLES` gains `projects: 'Projects',`.

- [ ] **Step 7: Run the tests to verify they pass**

Run:

```bash
cd internal/web/_ui && npx vitest run src/screens/projects.test.tsx src/a11y.test.tsx src/shell
make -C ../../.. web-check
```

Expected: PASS; `web-check` clean. The a11y test "every control on the shell has a name"
passes because each project row has text.

- [ ] **Step 8: Deploy and check headlessly**

Deploy per Global Constraints. Then in the playwright scratch directory write `projects.mjs`:

```js
import { chromium } from 'playwright'
const base = 'https://tba.tail74d52.ts.net'
const browser = await chromium.launch({ channel: 'chrome' })
const ctx = await browser.newContext({ storageState: 'state.json' })
const page = await ctx.newPage()
const bad = []
page.on('response', (r) => { if (r.status() >= 400) bad.push(`${r.status()} ${r.url()}`) })
page.on('console', (m) => { if (m.type() === 'error') bad.push('console: ' + m.text()) })
await page.goto(base + '/chat')
await page.getByRole('button', { name: 'New project' }).click()
await page.getByRole('textbox', { name: /Directory/ }).fill('/home/gigovich/work/devinlab')
await page.getByRole('button', { name: 'Add project' }).click()
await page.getByRole('list', { name: 'Projects' }).getByRole('button', { name: /devinlab/ }).waitFor()
console.log('sidebar:', await page.getByRole('list', { name: 'Projects' }).innerText())
await page.getByRole('button', { name: 'New project' }).click()
await page.getByRole('textbox', { name: /Directory/ }).fill('/nope')
await page.getByRole('button', { name: 'Add project' }).click()
console.log('refusal:', await page.getByRole('alert').innerText())
await page.keyboard.press('Escape')
await page.setViewportSize({ width: 400, height: 800 })
await page.reload()
console.log('phone overflow:', await page.evaluate(() => document.documentElement.scrollWidth > innerWidth))
console.log('bad:', bad)
await browser.close()
```

Run `node projects.mjs`. Expected: the sidebar text contains `devinlab` and `work`; the
refusal names `/nope`; phone overflow `false`; `bad` holds only the one expected 400 from
`/api/projects`. Then remove the project it added:
`curl -s -H "Authorization: Bearer $TOKEN" -X DELETE $U/api/projects/<id>` where `<id>` comes
from `GET /api/projects`.

- [ ] **Step 9: Commit**

```bash
git add internal/web/_ui/src
git commit -m "feat(web): projects in the sidebar, a dialog to add one, and /projects/{id}"
git push origin main
```

---

### Task 9: Sessions and skills follow the selected project

**Files:**
- Modify: `internal/web/_ui/src/screens/Chat.tsx:44-60` (the selector and `shown`),
  `:170-200` (the list header), `src/screens/Skills.tsx:64-75` (`api.skill`), `:78-95`
  (`trust`).
- Test: `internal/web/_ui/src/screens/projects.test.tsx`

**Interfaces:**
- Consumes: `inProject`, `AppState.project`, `api.skill(name, project)`,
  `api.trustSkills(project)`.
- Produces: the session list's "All projects" toggle (`aria-pressed`), the header count of
  the scoped list.

- [ ] **Step 1: Write the failing tests**

Append to `src/screens/projects.test.tsx` (add `RUN` to the harness import and `act` plus
`navigate` imports: `import { act } from '@testing-library/react'`, `import { navigate } from
'@/lib/route'`):

```tsx
test('the session list shows the current project, and "All projects" widens it', async () => {
  const user = userEvent.setup()
  const theirs = { ...RUN, id: 'r-2', title: 'Theirs', projectId: 'PRJ-1' }
  await mountApp({ projects: PROJECTS, runs: [RUN, theirs] })
  const list = await screen.findByRole('list', { name: 'Sessions' })
  expect(within(list).getByText('Rotate the signing keys')).toBeInTheDocument()
  expect(within(list).queryByText('Theirs')).not.toBeInTheDocument()

  await user.click(screen.getByRole('button', { name: 'All projects' }))
  expect(within(list).getByText('Theirs')).toBeInTheDocument()

  await user.click(screen.getByRole('button', { name: 'All projects' }))
  const projects = screen.getByRole('list', { name: 'Projects' })
  await user.click(within(projects).getByRole('button', { name: /work/ }))
  expect(within(list).getByText('Theirs')).toBeInTheDocument()
  expect(within(list).queryByText('Rotate the signing keys')).not.toBeInTheDocument()
})

test('a new session opens in the chosen project', async () => {
  const user = userEvent.setup()
  const h = await mountApp({
    projects: PROJECTS,
    routes: {
      'POST /api/runs': () =>
        new Response(JSON.stringify({ ...RUN, id: 'r-9', projectId: 'PRJ-1' }), { status: 201 }),
    },
  })
  const projects = await screen.findByRole('list', { name: 'Projects' })
  await user.click(within(projects).getByRole('button', { name: /work/ }))
  await user.click(screen.getByRole('button', { name: 'New session' }))
  await waitFor(() =>
    expect(h.sent.find((r) => r.path === '/api/runs')?.body).toBe('{"projectId":"PRJ-1"}'),
  )
})

test('the skills screen reads and trusts the chosen project', async () => {
  const user = userEvent.setup()
  const detail = {
    name: 'review',
    description: 'd',
    userInvocable: true,
    modelInvocable: true,
    allowedTools: [],
    disallowedTools: [],
    paths: [],
    body: 'x',
  }
  const h = await mountApp({
    projects: PROJECTS,
    skills: {
      items: [{ name: 'review', description: 'd', userInvocable: true, modelInvocable: true }],
      pending: { names: ['theirs'] },
    },
    routes: {
      '/api/skills/review?project=PRJ-1': () => new Response(JSON.stringify(detail), { status: 200 }),
      '/api/skills/review': () => new Response(JSON.stringify(detail), { status: 200 }),
      'POST /api/skills/trust': () => new Response('{"loaded":["theirs"],"notices":[]}', { status: 200 }),
    },
  })
  const projects = await screen.findByRole('list', { name: 'Projects' })
  await user.click(within(projects).getByRole('button', { name: /work/ }))
  act(() => navigate({ screen: 'skills' }))
  await waitFor(() => expect(h.paths).toContain('/api/skills/review?project=PRJ-1'))

  await user.click(await screen.findByRole('button', { name: 'Review and load' }))
  await user.click(await screen.findByRole('button', { name: 'Load them' }))
  await waitFor(() =>
    expect(h.sent.find((r) => r.path === '/api/skills/trust')?.body).toBe('{"project":"PRJ-1"}'),
  )
})
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd internal/web/_ui && npx vitest run src/screens/projects.test.tsx`
Expected: the three new tests FAIL (no "All projects" button; the list shows both runs; the
skill detail is asked without a project).

- [ ] **Step 3: Scope the session list**

In `src/screens/Chat.tsx`:
- Import `inProject` from `@/state/app` (add to the existing import) and `useState` is already
  imported.
- The `useApp` selector gains `project: s.project, projects: s.meta?.features.projects === true`.
  Name the second `hasProjects` in the destructuring: `const { runs, models, pendingCommand,
  opening, phone, project, hasProjects } = useApp((s) => ({ ..., project: s.project, hasProjects:
  s.meta?.features.projects === true }))`.
- After `const [filter, setFilter] = useState('')` add:

```ts
  const [all, setAll] = useState(false)
  const scoped = all || !hasProjects ? runs : inProject(runs, project)
```

  and `shown` filters `scoped` instead of `runs`.
- In the list header, the count becomes `{scoped.length}` and, after it and before the `New`
  button, add:

```tsx
            {hasProjects && (
              <button
                type="button"
                aria-pressed={all}
                aria-label="All projects"
                title="Show sessions from every project"
                onClick={() => setAll(!all)}
                className={`h-5 rounded-[5px] border px-[7px] text-[10.5px] ${
                  all ? 'border-primary text-fg' : 'border-line text-fg-muted'
                } cursor-pointer hover:border-line-strong hover:text-fg`}
              >
                All
              </button>
            )}
```

- The empty states read `scoped.length === 0` and `scoped.length > 0 && shown.length === 0`.

- [ ] **Step 4: Scope the skills screen**

In `src/screens/Skills.tsx`:
- The `useApp` selector gains `project: s.project`.
- `api.skill(name, abort.signal)` becomes `api.skill(name, project, abort.signal)` and the
  effect's dependency list becomes `[name, project]`.
- `api.trustSkills()` becomes `api.trustSkills(project)`; the `useCallback` dependency list
  becomes `[project]`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd internal/web/_ui && npx vitest run src/screens && make -C ../../.. web-check`
Expected: PASS; `web-check` clean.

- [ ] **Step 6: Deploy, check headlessly, commit**

Deploy per Global Constraints. Extend `projects.mjs` (or write `sessions.mjs`) to: add
`/home/gigovich/work/devinlab`, click it in the sidebar, click `New` in the sessions list,
wait for the address bar to hold `/chat/RUN-`, read `GET /api/runs/<id>` through
`page.request.get` and assert `projectId` is the project's id, then click `All projects` and
assert the list grew. Close the run and remove the project afterwards.

```bash
git add internal/web/_ui/src
git commit -m "feat(web): sessions and skills follow the chosen project"
git push origin main
```

---

### Task 10: The Worktrees screen lists a project's repositories

**Files:**
- Create: `internal/web/_ui/src/screens/Worktrees.tsx`
- Modify: `internal/web/_ui/src/screens/Placeholders.tsx` (remove `Repos`; reword the two
  that stay), `src/App.tsx` (import), `src/shell/Sidebar.tsx` (the `repos` row gains
  `feature: 'projects'`)
- Test: `internal/web/_ui/src/screens/projects.test.tsx`

**Interfaces:**
- Consumes: `api.projectRepos`, `currentProject`, `DataGrid`, `EmptyState`.
- Produces: `Worktrees()`, a grid `aria-label="Repositories"`.

- [ ] **Step 1: Write the failing tests**

Append to `src/screens/projects.test.tsx`:

```tsx
test('the worktrees screen lists the repositories of the chosen project', async () => {
  const user = userEvent.setup()
  await mountApp({
    projects: PROJECTS,
    routes: {
      '/api/projects/PRJ-1/repos': () =>
        new Response(
          JSON.stringify([
            { name: '', dir: '/home/dev/work', main: 'main' },
            { name: 'api', dir: '/home/dev/work/api' },
          ]),
          { status: 200 },
        ),
    },
  })
  act(() => navigate({ screen: 'repos' }))
  expect(await screen.findByText(/no project record/)).toBeInTheDocument()

  const projects = screen.getByRole('list', { name: 'Projects' })
  await user.click(within(projects).getByRole('button', { name: /work/ }))
  const grid = await screen.findByRole('grid', { name: 'Repositories' })
  expect(within(grid).getByText('work (the project itself)')).toBeInTheDocument()
  expect(within(grid).getByText('api')).toBeInTheDocument()
  expect(within(grid).getByText('main')).toBeInTheDocument()
  expect(within(grid).getByText('neither main nor master')).toBeInTheDocument()
  expect(within(grid).getAllByText('no worktrees yet')).toHaveLength(2)
})

test('the worktrees row is offered only with projects', async () => {
  await mountApp({ meta: { features: { controlSocket: true, runs: true } } })
  const nav = await screen.findByRole('navigation', { name: 'Navigation' })
  expect(within(nav).queryByRole('button', { name: /Worktrees/ })).not.toBeInTheDocument()
})
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd internal/web/_ui && npx vitest run src/screens/projects.test.tsx`
Expected: the two new tests FAIL.

- [ ] **Step 3: The screen**

Create `src/screens/Worktrees.tsx`:

```tsx
import { useEffect, useState } from 'react'
import { api } from '@/lib/api'
import type { Repository } from '@/lib/wire'
import { currentProject, explain, setBanner, useApp } from '@/state/app'
import { DataGrid } from '@/ui/DataGrid'
import type { Column } from '@/ui/DataGrid'
import { EmptyState } from '@/ui/EmptyState'

/**
 * A project's repositories, with the branch a run will merge into. The
 * worktrees themselves arrive with runs on tickets; until then every row says
 * so.
 */
export function Worktrees() {
  const { project, name } = useApp((s) => ({ project: s.project, name: currentProject(s)?.name ?? '' }))
  const [repos, setRepos] = useState<{ project: string; items: Repository[] } | null>(null)

  useEffect(() => {
    if (!project) return
    const abort = new AbortController()
    void api
      .projectRepos(project, abort.signal)
      .then((items) => setRepos({ project, items }))
      .catch((err: unknown) => {
        if (!abort.signal.aborted) setBanner(explain(err))
      })
    return () => abort.abort()
  }, [project])

  const columns: Column<Repository>[] = [
    { key: 'name', header: 'Repository', width: '200px', cell: (r) => r.name || `${name} (the project itself)` },
    { key: 'main', header: 'Main branch', width: '140px', cell: (r) => r.main ?? 'neither main nor master' },
    { key: 'dir', header: 'Directory', width: 'minmax(200px, 1fr)', cell: (r) => r.dir },
    { key: 'worktrees', header: 'Worktrees', width: '140px', cell: () => 'no worktrees yet' },
  ]

  const loaded = repos?.project === project ? repos.items : null
  return (
    <>
      <div className="flex-none border-b border-line px-[18px] pt-[14px] pb-3">
        <h1 className="m-0 text-[16px] font-semibold tracking-[-0.015em]">Repositories & worktrees</h1>
      </div>
      {!project ? (
        <EmptyState
          title="This daemon's directory has no project record."
          detail="Choose or add a project in the sidebar to list its repositories. Worktrees arrive with runs on tickets."
        />
      ) : loaded === null ? (
        <p className="px-[18px] py-4 text-[12px] text-fg-subtle">Reading the repositories…</p>
      ) : (
        <DataGrid
          label="Repositories"
          columns={columns}
          rows={loaded}
          rowKey={(r) => r.dir}
          minWidth={680}
          empty={
            <EmptyState
              title="No repositories."
              detail={`${name} holds no git checkout, at its root or one level down.`}
            />
          }
        />
      )}
    </>
  )
}
```

`DataGrid` draws `empty` when `rows` is empty (`src/ui/DataGrid.tsx:121`), so the grid is
rendered whether or not the project has a checkout.

- [ ] **Step 4: Retire the placeholder and gate the row**

In `src/screens/Placeholders.tsx`: delete `Repos`; change `Tickets`' detail to "A ticket belongs
to a repository inside a project. Tickets arrive in the next phase." and `Task`'s to "Overview,
discussion, changes and runs are drawn against a ticket, which arrives in the next phase.";
reword the file comment's "the concept it is about arrives with projects, which is phase two" to
"the concept it is about arrives with tickets, which is the next phase".

In `src/App.tsx`: `import { Task, Tickets } from '@/screens/Placeholders'` and
`import { Worktrees } from '@/screens/Worktrees'`; the `'repos'` case renders `<Worktrees />`.

In `src/shell/Sidebar.tsx`, the `BOTTOM` row for `repos` gains `feature: 'projects'`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd internal/web/_ui && npx vitest run && make -C ../../.. web-check`
Expected: PASS; `web-check` clean. If the a11y "whole shell can be reached from the keyboard"
test or a layout test counted the Worktrees row, they mount with `META` (which has
`projects: true`), so the row is still there.

- [ ] **Step 6: Deploy, check headlessly, commit**

Deploy per Global Constraints. Headless: add `/home/gigovich/work/devinlab`, select it, go to
`/repos`, assert the grid names `aigem` with `main`; select the daemon's directory and assert
the empty state; then remove the project.

```bash
git add internal/web/_ui/src
git commit -m "feat(web): the worktrees screen lists a project's repositories"
git push origin main
```

---

### Task 11: Review cycles, changelog, final deploy

**Files:**
- Modify: `CHANGELOG.md`
- Read only: everything since the commit before Task 1 (`git log --oneline` names it as the
  parent of "feat(runner): a registry of projects").

- [ ] **Step 1: First review cycle**

Dispatch review subagents in worktree isolation over `git diff <commit before Task 1>..HEAD`,
each with one brief:
- code quality and simplicity - the top criterion: any clear simplification within the
  existing architecture is the highest-priority finding;
- functionality and logic - trace: a second `Env()` call while the first load is in flight;
  `Remove` during a load; `Close` during a load; `refreshAll` correcting a stale selection
  before the scoped reads; a `project.updated` frame for a project the tab has selected whose
  environment just failed;
- security and performance - the project path is a person's own typed path, the sandbox roots
  at it, `DELETE` removes no files, `git show-ref` runs with `-C dir` and never with a
  user-shaped argument list beyond the two literal branch names;
- testing and documentation - every route in `docs/web.md` has a test, the feature-map test
  lists every key, the harness fake answers every path the page asks for;
- architecture and design - `internal/web` still imports nothing from `internal/runner`; the
  registry does not know runs; the adapter, not the router, maps errors;
- contract changes - the `SkillsBackend` and `CommandsBackend` signatures, `NewRun.projectId`,
  the feature key and the control kind, against `docs/web.md` and the client's `wire.ts`.

- [ ] **Step 2: Apply what they find**

Rerun `go test ./... -count=1`, `golangci-lint run` and `make web-check` after the fixes.

- [ ] **Step 3: Second and third review cycles**

Repeat Step 1 and Step 2 twice more, each over the diff that includes the fixes. Stop early
only when a cycle reports nothing.

- [ ] **Step 4: Changelog**

In `CHANGELOG.md` under `## [Unreleased]` / `### Added`, after the bullet that begins
"- The browser UI runs the slash commands:", add (wrapped at 80, hyphens only):

```
- Projects. The browser adds a project - a directory on the daemon's machine -
  and the daemon keeps a registry of them in `$XDG_STATE_HOME/aigem/
  projects.json`. Each project gets one environment, loaded the first time
  something needs it; a run opens in its project's environment and is rooted
  at the project directory; the skills and command catalogues are read per
  project, and a project's skills are approved per project. The sidebar
  chooses the project every list reads, the session list follows it with an
  "All projects" toggle, and the Worktrees screen lists a project's
  repositories with the branch a run will merge into. The API grew
  `/api/projects`, a `projects` feature key and a `project.updated` frame.
```

- [ ] **Step 5: Final deploy, headless pass, commit, push**

```bash
make web && make install && systemctl --user restart aigem-web
```

Run the full headless script from Task 8 Step 8 once more, then remove what it added.

```bash
git add -A
git commit -m "fix(web,runner): close what the review cycles found; note projects in the changelog"
git push origin main
```

---

## Out of scope, on purpose

- Tickets, the Task screen, worktrees themselves, autonomous runs, Stop run: sub-projects B and
  C, each with its own plan.
- Palette entries for switching projects, project names in the breadcrumb, and closing a
  project's environment when its last run closes: not in the spec; add when asked.
- Re-discovering repositories on a timer, or watching the project directory.
