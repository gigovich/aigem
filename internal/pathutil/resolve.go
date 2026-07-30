// Package pathutil holds the path canonicalization shared by the sandbox and
// the persisted path grants. Both decide containment by comparing strings, so
// both must spell the same directory the same way - including for paths that do
// not exist yet.
package pathutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// maxSymlinkHops bounds manual link following, mirroring the kernel's ELOOP
// limit so a cycle of dangling links cannot spin.
const maxSymlinkHops = 40

// ResolveDeepest resolves symlinks in the deepest *existing* ancestor of p and
// rejoins the components that do not exist yet.
//
// filepath.EvalSymlinks alone is not enough, in two ways:
//
//   - It fails outright when any component is missing, so callers that fall back
//     to the raw path end up storing two different spellings of one directory
//     depending on whether it existed at the time. On macOS, where /var is a
//     symlink to /private/var, that makes string containment checks disagree.
//   - It reports ENOENT both for a component that is absent and for one that IS
//     a symlink whose target does not exist. Treating the latter as absent lets
//     a path resolve to something lexically inside a root while the kernel
//     follows the link outside it on create.
func ResolveDeepest(p string) (string, error) { return resolve(p, 0) }

func resolve(p string, hops int) (string, error) {
	if hops > maxSymlinkHops {
		return "", fmt.Errorf("too many levels of symbolic links resolving %q", p)
	}
	var missing []string
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return rejoin(real, missing), nil
		}
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(cur)
			if err != nil {
				return "", err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(cur), target)
			}
			resolved, err := resolve(filepath.Clean(target), hops+1)
			if err != nil {
				return "", err
			}
			return rejoin(resolved, missing), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil // walked to the filesystem root without finding anything
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}

// rejoin appends the not-yet-existing components, outermost first, back onto a
// resolved base.
func rejoin(base string, missing []string) string {
	for i := len(missing) - 1; i >= 0; i-- {
		base = filepath.Join(base, missing[i])
	}
	return base
}

// Canonical returns p as an absolute, symlink-resolved, cleaned path. It is
// ResolveDeepest plus the absolute-path step, for callers that store or compare
// the result.
func Canonical(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	resolved, err := ResolveDeepest(filepath.Clean(abs))
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}
