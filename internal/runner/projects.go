package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/store"
)

var (
	ErrNoProject      = errors.New("runner: no such project")
	ErrBadProject     = errors.New("runner: cannot add that project")
	ErrProjectsClosed = errors.New("runner: the project registry is closed")
	ErrProjectInUse   = errors.New("runner: the project has an open run")
)

const projectIDPrefix = "PRJ-"

type Project struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Dir     string    `json:"dir"`
	Created time.Time `json:"created"`
}

// ProjectTable is the saved registry. The counter is saved with the rows
// because it is not derivable from them: the highest id can be the one just
// removed, and an id that came back would name a project the pages and the run
// table still remember.
type ProjectTable struct {
	Next     int       `json:"next"`
	Projects []Project `json:"projects"`
}

type ProjectView struct {
	Project
	LoadError string
}

type ProjectsConfig struct {
	Store   *store.File[ProjectTable]
	LoadEnv func(ctx context.Context, dir string) (*Env, error)
	Notify  func(ProjectView)
	Now     func() time.Time
}

type Projects struct {
	loadEnv func(context.Context, string) (*Env, error)
	notify  func(ProjectView)
	now     func() time.Time

	mu     sync.Mutex
	file   *store.File[ProjectTable]
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
	refs    int
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
	p.next = saved.Next
	for _, rec := range saved.Projects {
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
		return ProjectView{}, fmt.Errorf("%w: %w", ErrBadProject, err)
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
	if pr.refs > 0 {
		p.mu.Unlock()
		return ErrProjectInUse
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
	rows := make([]Project, 0, len(p.order))
	for _, id := range p.order {
		rows = append(rows, p.byID[id].rec)
	}
	if err := p.file.Save(ProjectTable{Next: p.next, Projects: rows}); err != nil {
		return fmt.Errorf("runner: could not write the project table: %w", err)
	}
	return nil
}

// Env is the project's environment, loaded on first use and kept for the
// daemon's life.
func (p *Projects) Env(ctx context.Context, id string) (*Env, error) {
	env, _, err := p.open(ctx, id, false)
	return env, err
}

// Retain is Env for a caller that will hold the environment - an open run.
// Remove refuses the project until the returned func is called, which is safe
// to call more than once.
func (p *Projects) Retain(ctx context.Context, id string) (*Env, func(), error) {
	return p.open(ctx, id, true)
}

// open loads the environment if it is not loaded yet.
//
// The load runs without the lock, as OpenRun does: it dials MCP servers and
// runs the SessionStart hook. Callers that arrive together share one load, and
// take its answer rather than trying again. A load that failed is recorded on
// the project, announced, and tried again by the next caller to arrive.
func (p *Projects) open(ctx context.Context, id string, retain bool) (*Env, func(), error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, nil, ErrProjectsClosed
		}
		pr := p.byID[id]
		if pr == nil {
			p.mu.Unlock()
			return nil, nil, ErrNoProject
		}
		if pr.env != nil {
			env, release := pr.env, p.retainLocked(pr, retain)
			p.mu.Unlock()
			return env, release, nil
		}
		if pr.loading != nil {
			wait := pr.loading
			p.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			p.mu.Lock()
			switch {
			case pr.env != nil:
				env, release := pr.env, p.retainLocked(pr, retain)
				p.mu.Unlock()
				return env, release, nil
			case pr.loadErr != "":
				err := fmt.Errorf("could not load project %s: %s", pr.rec.Name, pr.loadErr)
				p.mu.Unlock()
				return nil, nil, err
			}
			p.mu.Unlock()
			continue
		}
		done := make(chan struct{})
		pr.loading = done
		dir, name := pr.rec.Dir, pr.rec.Name
		p.mu.Unlock()

		// The load outlives the request that started it: a second caller is
		// already waiting on it, and the environment it builds is the
		// project's, not this request's.
		env, err := func() (*Env, error) {
			// A panic in the load would otherwise leave loading set and done
			// never closed, wedging the project for the daemon's life. The
			// normal path does the same handoff below, in the one critical
			// section that also decides the abandoned answer, so that a waiter
			// never sees loading == nil and env == nil at once.
			defer func() {
				if r := recover(); r != nil {
					p.mu.Lock()
					pr.loading = nil
					close(done)
					p.mu.Unlock()
					panic(r)
				}
			}()
			return p.loadEnv(context.WithoutCancel(ctx), dir)
		}()

		p.mu.Lock()
		pr.loading = nil
		close(done)
		abandoned := p.byID[id] != pr
		closed := p.closed
		if abandoned || closed {
			p.mu.Unlock()
			if env != nil {
				env.Close()
			}
			if closed {
				return nil, nil, ErrProjectsClosed
			}
			return nil, nil, ErrNoProject
		}
		if err != nil {
			pr.loadErr = err.Error()
			v := p.viewLocked(pr)
			p.mu.Unlock()
			p.notify(v)
			return nil, nil, fmt.Errorf("could not load project %s: %w", name, err)
		}
		pr.env = env
		announce := pr.loadErr != ""
		pr.loadErr = ""
		v := p.viewLocked(pr)
		release := p.retainLocked(pr, retain)
		p.mu.Unlock()
		if announce {
			p.notify(v)
		}
		return env, release, nil
	}
}

func (p *Projects) retainLocked(pr *project, retain bool) func() {
	if !retain {
		return func() {}
	}
	pr.refs++
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			pr.refs--
			p.mu.Unlock()
		})
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

type Repository struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
	Main string `json:"main,omitempty"`
}

// Repositories discovers the project's git checkouts on demand: the project
// directory itself when it is one, then each direct child that is, by name.
func (p *Projects) Repositories(ctx context.Context, id string) ([]Repository, error) {
	v, err := p.Get(id)
	if err != nil {
		return nil, err
	}
	var out []Repository
	if isCheckout(v.Dir) {
		out = append(out, Repository{Dir: v.Dir, Main: mainBranch(ctx, v.Dir)})
	}
	entries, err := os.ReadDir(v.Dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		dir := filepath.Join(v.Dir, e.Name())
		if isCheckout(dir) {
			out = append(out, Repository{Name: e.Name(), Dir: dir, Main: mainBranch(ctx, dir)})
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
func mainBranch(ctx context.Context, dir string) string {
	for _, b := range []string{"main", "master"} {
		cmd := exec.CommandContext(ctx, "git", "-C", dir, "show-ref", "--verify", "--quiet", "refs/heads/"+b)
		if cmd.Run() == nil {
			return b
		}
	}
	return ""
}
