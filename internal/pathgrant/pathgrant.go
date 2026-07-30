// Package pathgrant persists the directories a user has allowed an agent to
// read outside its working directory.
//
// A grant is scoped to the project it was made from: approving ~/work/shared
// while working in ~/work/api does not open it for a session started in
// ~/work/web. Grants are read access only - a write outside the working
// directory is asked about every time and never remembered, because the second
// question costs less than an overwritten file in someone else's repository.
package pathgrant

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/pathutil"
)

// Grant is one approved directory, and everything beneath it.
type Grant struct {
	Project   string    `json:"project"`
	Dir       string    `json:"dir"`
	GrantedAt time.Time `json:"grantedAt"`
}

type file struct {
	Version int     `json:"version"`
	Grants  []Grant `json:"grants"`
}

var mu sync.Mutex

// Allowed reports whether path is inside a directory granted for project.
func Allowed(project, path string) (bool, error) {
	project, err := canonical(project)
	if err != nil {
		return false, err
	}
	path, err = canonical(path)
	if err != nil {
		return false, err
	}
	mu.Lock()
	defer mu.Unlock()
	f, err := load()
	if err != nil {
		return false, err
	}
	for _, g := range f.Grants {
		if g.Project == project && within(g.Dir, path) {
			return true, nil
		}
	}
	return false, nil
}

// Add records dir (and everything under it) as readable from project. Adding a
// directory that already covers, or is covered by, an existing grant collapses
// them so the file cannot grow a redundant entry per file read.
func Add(project, dir string) error {
	project, err := canonical(project)
	if err != nil {
		return err
	}
	dir, err = canonical(dir)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	f, err := load()
	if err != nil {
		return err
	}
	kept := f.Grants[:0]
	for _, g := range f.Grants {
		if g.Project == project {
			if within(g.Dir, dir) {
				return nil // already covered by a broader grant
			}
			if within(dir, g.Dir) {
				continue // the new grant subsumes this one
			}
		}
		kept = append(kept, g)
	}
	f.Grants = append(kept, Grant{Project: project, Dir: dir, GrantedAt: time.Now().UTC()})
	return save(f)
}

// List returns the directories granted for project, in path order.
func List(project string) ([]Grant, error) {
	project, err := canonical(project)
	if err != nil {
		return nil, err
	}
	mu.Lock()
	defer mu.Unlock()
	f, err := load()
	if err != nil {
		return nil, err
	}
	var out []Grant
	for _, g := range f.Grants {
		if g.Project == project {
			out = append(out, g)
		}
	}
	return out, nil
}

// ListAll returns every grant, in project then path order.
func ListAll() ([]Grant, error) {
	mu.Lock()
	defer mu.Unlock()
	f, err := load()
	if err != nil {
		return nil, err
	}
	return f.Grants, nil
}

// Forget removes one granted directory from project, reporting whether it was
// there. dir must match a recorded grant exactly.
func Forget(project, dir string) (bool, error) {
	project, err := canonical(project)
	if err != nil {
		return false, err
	}
	dir, err = canonical(dir)
	if err != nil {
		return false, err
	}
	mu.Lock()
	defer mu.Unlock()
	f, err := load()
	if err != nil {
		return false, err
	}
	kept, found := f.Grants[:0], false
	for _, g := range f.Grants {
		if g.Project == project && g.Dir == dir {
			found = true
			continue
		}
		kept = append(kept, g)
	}
	if !found {
		return false, nil
	}
	f.Grants = kept
	return true, save(f)
}

// ForgetProject removes every grant made from project, reporting how many went.
func ForgetProject(project string) (int, error) {
	project, err := canonical(project)
	if err != nil {
		return 0, err
	}
	mu.Lock()
	defer mu.Unlock()
	f, err := load()
	if err != nil {
		return 0, err
	}
	kept, n := f.Grants[:0], 0
	for _, g := range f.Grants {
		if g.Project == project {
			n++
			continue
		}
		kept = append(kept, g)
	}
	if n == 0 {
		return 0, nil
	}
	f.Grants = kept
	return n, save(f)
}

// within reports whether path is dir itself or sits beneath it. Both must
// already be canonical, so this is a pure prefix test on path elements - a
// string prefix would let /srv/data-old match a grant on /srv/data.
func within(dir, path string) bool {
	if dir == path {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// canonical resolves p to an absolute, symlink-free path so a grant cannot be
// side-stepped by reaching the same directory through a link.
func canonical(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	// Resolving the deepest existing ancestor, rather than requiring the whole
	// path to exist, is what keeps one directory from being stored under two
	// spellings - which on macOS (/var -> /private/var) made grants silently
	// stop matching once a path was recorded before it existed.
	return pathutil.Canonical(p)
}

func grantFile() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "path-grants.json"), nil
}

func load() (file, error) {
	path, err := grantFile()
	if err != nil {
		return file{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return file{Version: 1}, nil
	}
	if err != nil {
		return file{}, fmt.Errorf("read path grants: %w", err)
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return file{}, fmt.Errorf("parse path grants: %w", err)
	}
	if f.Version != 1 {
		return file{}, fmt.Errorf("unsupported path grant version %d", f.Version)
	}
	return f, nil
}

func save(f file) error {
	path, err := grantFile()
	if err != nil {
		return err
	}
	f.Version = 1
	sort.Slice(f.Grants, func(i, j int) bool {
		if f.Grants[i].Project != f.Grants[j].Project {
			return f.Grants[i].Project < f.Grants[j].Project
		}
		return f.Grants[i].Dir < f.Grants[j].Dir
	})
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// A uniquely named temp file: several aigem processes share this directory,
	// and a fixed ".tmp" would let two concurrent writers interleave.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".path-grants-*.json")
	if err != nil {
		return fmt.Errorf("write path grants: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write path grants: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
