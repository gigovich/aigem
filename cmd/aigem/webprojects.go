package main

import (
	"context"
	"errors"
	"path/filepath"
	"time"

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
	if err := b.projects.Remove(id); err != nil {
		return webProjectError(err)
	}
	b.skillMu.Lock()
	b.forgetPendingLocked(id)
	b.skillMu.Unlock()
	b.recordActivity(web.Activity{Kind: "project.removed", Text: "Removed project " + id})
	return nil
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

// projectEnv is the one rule for what a project id means, shared by the read
// routes and by a run being opened: the daemon's own environment for an empty
// id, otherwise the project's, loaded on first use. A retained environment
// holds the project open - Remove refuses it until the returned func is
// called - and the returned func is safe to call on any path.
func projectEnv(ctx context.Context, own *runner.Env, projects *runner.Projects, id string, retain bool) (
	*runner.Env, func(), error,
) {
	nothing := func() {}
	switch {
	case id == "":
		if own == nil {
			return nil, nothing, web.ErrUnavailable
		}
		return own, nothing, nil
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
	env, _, err := projectEnv(ctx, b.env, b.projects, project, false)
	if err != nil && project != "" {
		return nil, webProjectError(err)
	}
	return env, err
}

func webProject(v runner.ProjectView) web.Project {
	return web.Project{ID: v.ID, Name: v.Name, Dir: v.Dir, Created: v.Created, LoadError: v.LoadError}
}

// webProjectError classifies what the registry reports. A bad directory and a
// load that failed are both sentences for the person who typed the path.
func webProjectError(err error) error {
	switch {
	case errors.Is(err, runner.ErrNoProject):
		return web.ErrNoProject
	case errors.Is(err, runner.ErrProjectInUse):
		return web.Conflict("this project has an open run; close it first")
	case errors.Is(err, runner.ErrProjectsClosed):
		return err
	default:
		return web.Refuse(err)
	}
}
