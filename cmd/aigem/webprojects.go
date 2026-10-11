package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gigovich/aigem/internal/pathutil"
	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/web"
)

// The project half of the backend: the registry's answers translated, and the
// one question every scoped route asks - which environment a project id means.

// projectReposTimeout bounds one discovery: it stats the project directory and
// shells out to git once per checkout it finds.
const projectReposTimeout = 5 * time.Second

func (b *webBackend) Projects(context.Context) ([]web.Project, error) {
	if b.projects == nil {
		return nil, web.ErrUnavailable
	}
	out := []web.Project{}
	for _, v := range b.projects.List() {
		out = append(out, webProject(v))
	}
	return out, nil
}

func (b *webBackend) AddProject(_ context.Context, req web.NewProject) (web.Project, error) {
	if b.projects == nil {
		return web.Project{}, web.ErrUnavailable
	}
	dir, err := b.underRoot(req.Dir)
	if err != nil {
		return web.Project{}, err
	}
	v, err := b.projects.Add(dir, req.Name)
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
	if err := b.projects.Remove(id); err != nil {
		return webProjectError(err)
	}
	b.skillMu.Lock()
	b.forgetPendingLocked(id)
	b.skillMu.Unlock()
	b.recordActivity(web.Activity{Kind: "project.removed", Text: "Removed project " + id})
	return nil
}

func (b *webBackend) UpdateProject(_ context.Context, id string, req web.ProjectPatch) (
	web.Project, error,
) {
	if b.projects == nil {
		return web.Project{}, web.ErrUnavailable
	}
	v, err := b.projects.SetDispatch(id, req.Slots, req.Paused)
	if err != nil {
		return web.Project{}, webProjectError(err)
	}
	return webProject(v), nil
}

func (b *webBackend) ProjectRepos(ctx context.Context, id string) ([]web.Repository, error) {
	if b.projects == nil {
		return nil, web.ErrUnavailable
	}
	// Discovery stats a directory tree and shells out to git per checkout, and
	// the project directory is whatever the person named.
	ctx, cancel := context.WithTimeout(ctx, projectReposTimeout)
	defer cancel()
	repos, err := b.projects.Repositories(ctx, id)
	if err != nil {
		return nil, webProjectError(err)
	}
	out := make([]web.Repository, 0, len(repos))
	for _, r := range repos {
		out = append(out, web.Repository{Name: r.Name, Dir: r.Dir, Main: r.Main})
	}
	return out, nil
}

// maxDirs bounds one listing: the form shows a tree a person reads, and a
// directory holding more than this is not one anybody picks a project from.
// maxScanned bounds the entries read to find them, files included.
const (
	maxDirs    = 1000
	maxScanned = 10000
)

func (b *webBackend) Dirs(ctx context.Context, path string) (web.DirListing, error) {
	if b.projects == nil {
		return web.DirListing{}, web.ErrUnavailable
	}
	dir, err := b.underRoot(path)
	if err != nil {
		return web.DirListing{}, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return web.DirListing{}, web.Refuse(err)
	}
	defer func() { _ = f.Close() }()
	rel, _ := filepath.Rel(b.root, dir)
	if rel == "." {
		rel = ""
	}
	out := web.DirListing{Root: b.root, Path: filepath.ToSlash(rel)}
	for scanned := 0; ; {
		if scanned >= maxScanned || len(out.Dirs) > maxDirs {
			out.Truncated = true
			break
		}
		if err := ctx.Err(); err != nil {
			return web.DirListing{}, err
		}
		batch, err := f.ReadDir(256)
		scanned += len(batch)
		for _, e := range batch {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			full := filepath.Join(dir, e.Name())
			// A link is listed only when it leads to a directory the root allows.
			if e.Type()&os.ModeSymlink != 0 {
				target, err := b.underRoot(full)
				if err != nil {
					continue
				}
				if info, err := os.Stat(target); err != nil || !info.IsDir() {
					continue
				}
			} else if !e.IsDir() {
				continue
			}
			_, gitErr := os.Lstat(filepath.Join(full, ".git"))
			out.Dirs = append(out.Dirs, web.Dir{Name: e.Name(), Git: gitErr == nil})
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return web.DirListing{}, web.Refuse(err)
		}
	}
	slices.SortFunc(out.Dirs, func(a, b web.Dir) int { return strings.Compare(a.Name, b.Name) })
	if len(out.Dirs) > maxDirs {
		out.Dirs = out.Dirs[:maxDirs]
	}
	return out, nil
}

// underRoot resolves a path the page sent - relative to the root, or absolute
// - and refuses one that is not the root or beneath it. A symlink repointed
// after the check is not guarded against: the person picking is the one who
// could repoint it.
func (b *webBackend) underRoot(p string) (string, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(b.root, filepath.FromSlash(p))
	}
	dir, err := pathutil.Canonical(p)
	if err != nil {
		return "", web.Refuse(err)
	}
	if !pathutil.Within(b.root, dir) {
		return "", web.Refuse(fmt.Errorf("only directories under %s can be used", b.root))
	}
	return dir, nil
}

// projectEnv is the one rule for what a project id means, shared by the read
// routes and by a run being opened: the project's environment, loaded on first
// use. A retained environment holds the project open - Remove refuses it until
// the returned func is called - and the returned func is safe to call on any
// path.
func projectEnv(ctx context.Context, projects *runner.Projects, id string, retain bool) (
	*runner.Env, func(), error,
) {
	nothing := func() {}
	switch {
	case id == "":
		return nil, nothing, web.Refuse(errors.New("choose a project first"))
	case projects == nil:
		return nil, nothing, web.Refuse(errors.New("this daemon serves no projects"))
	case retain:
		env, release, err := projects.Retain(ctx, id)
		if err != nil {
			return nil, nothing, err
		}
		return env, release, nil
	default:
		env, err := projects.Env(ctx, id)
		return env, nothing, err
	}
}

// envFor is projectEnv for a read route, with the registry's answer translated
// into the statuses internal/web knows.
func (b *webBackend) envFor(ctx context.Context, project string) (*runner.Env, error) {
	env, _, err := projectEnv(ctx, b.projects, project, false)
	if err != nil {
		return nil, webProjectError(err)
	}
	return env, nil
}

func webProject(v runner.ProjectView) web.Project {
	return web.Project{
		ID: v.ID, Name: v.Name, Dir: v.Dir, Created: v.Created, LoadError: v.LoadError,
		Slots: v.Slots, Paused: v.Paused,
	}
}

// webProjectError classifies what the registry reports. A bad directory and a
// load that failed are both sentences for the person who typed the path.
func webProjectError(err error) error {
	switch {
	case errors.Is(err, runner.ErrNoProject):
		return web.ErrNoProject
	case errors.Is(err, runner.ErrProjectInUse):
		return web.Conflict("this project has an open run; delete it first")
	case errors.Is(err, runner.ErrProjectsClosed):
		return err
	default:
		return web.Refuse(err)
	}
}
