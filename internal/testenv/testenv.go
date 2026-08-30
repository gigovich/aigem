// Package testenv provides hermetic process-level fixtures for Go test packages.
package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var tempVars = []string{"TMPDIR", "TMP", "TEMP"}

// Run executes a test package inside a private environment and returns its exit code.
func Run(m *testing.M) int {
	sandbox, cleanup, err := setup()
	if err != nil {
		fmt.Fprintln(os.Stderr, "test environment setup failed")
		return 1
	}
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintln(os.Stderr, "test environment cleanup failed")
		code = 1
	}
	_ = sandbox
	return code
}

func setup() (string, func() error, error) {
	base, err := defaultTempDir()
	if err != nil {
		return "", nil, err
	}
	if root := outermostWorktree(base); root != "" {
		base = filepath.Dir(root)
		if outermostWorktree(base) != "" {
			return "", nil, fmt.Errorf("temporary directory is inside a worktree")
		}
	}
	sandbox, err := os.MkdirTemp(base, "aigem-testenv-")
	if err != nil {
		return "", nil, err
	}
	oldWD, err := os.Getwd()
	if err != nil {
		_ = os.RemoveAll(sandbox)
		return "", nil, err
	}
	cleanup := func() error {
		if err := os.Chdir(oldWD); err != nil {
			return err
		}
		return os.RemoveAll(sandbox)
	}
	if err := os.Chdir(sandbox); err != nil {
		_ = os.RemoveAll(sandbox)
		return "", nil, err
	}
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "TMPDIR", "TMP", "TEMP"} {
		if err := os.Mkdir(filepath.Join(sandbox, name), 0o700); err != nil {
			_ = cleanup()
			return "", nil, err
		}
		if err := os.Setenv(name, filepath.Join(sandbox, name)); err != nil {
			_ = cleanup()
			return "", nil, err
		}
	}
	return sandbox, cleanup, nil
}

func defaultTempDir() (string, error) {
	old := make(map[string]string, len(tempVars))
	set := make(map[string]bool, len(tempVars))
	for _, name := range tempVars {
		if value, ok := os.LookupEnv(name); ok {
			old[name] = value
			set[name] = true
			if err := os.Unsetenv(name); err != nil {
				return "", err
			}
		}
	}
	base := os.TempDir()
	for _, name := range tempVars {
		if set[name] {
			if err := os.Setenv(name, old[name]); err != nil {
				return "", err
			}
		}
	}
	return base, nil
}

func outermostWorktree(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	var roots []string
	for dir := abs; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			roots = append(roots, dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	if len(roots) == 0 {
		return ""
	}
	return roots[len(roots)-1]
}

// IsWithinWorktree is used by tests to assert that a fixture cannot resolve into
// a checkout through an ancestor walk.
func IsWithinWorktree(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
