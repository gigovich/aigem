package pathutil

import (
	"path/filepath"
	"testing"
)

func TestWithin(t *testing.T) {
	root := filepath.FromSlash("/r/root")
	for p, want := range map[string]bool{
		"/r/root":          true,
		"/r/root/a/b":      true,
		"/r/root/..foo":    true,
		"/r":               false,
		"/r/rootless":      false,
		"/r/root/../other": false,
		"/elsewhere":       false,
	} {
		if got := Within(root, filepath.Clean(filepath.FromSlash(p))); got != want {
			t.Errorf("Within(%s, %s) = %v, want %v", root, p, got, want)
		}
	}
}
