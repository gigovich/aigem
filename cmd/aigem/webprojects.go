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
