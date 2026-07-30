package pathgrant

import (
	"path/filepath"
	"testing"
)

// tempDir is t.TempDir() with symlinks resolved. Grants are stored canonically,
// so a test that compares a stored Dir against a raw t.TempDir() path fails
// wherever the temp root is reached through a link - notably macOS, where
// /var/folders/... resolves to /private/var/folders/....
func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return real
}
