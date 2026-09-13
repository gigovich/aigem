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
