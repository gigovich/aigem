package tui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestActiveAtToken(t *testing.T) {
	cases := []struct {
		val   string
		start int
		query string
		ok    bool
	}{
		{"", 0, "", false},
		{"hello", 0, "", false},
		{"@", 0, "", true},
		{"@foo", 0, "foo", true},
		{"see @foo/bar", 4, "foo/bar", true},
		{"see @foo bar", 0, "", false},  // token closed by trailing space
		{"email a@b.com", 0, "", false}, // '@' not at a word start
		{"a\n@deep", 2, "deep", true},   // '@' after a newline starts a token
		{"@a @b", 3, "b", true},         // last open token wins
	}
	for _, c := range cases {
		start, query, ok := activeAtToken(c.val)
		if ok != c.ok || (ok && (start != c.start || query != c.query)) {
			t.Errorf("activeAtToken(%q) = (%d,%q,%v), want (%d,%q,%v)",
				c.val, start, query, ok, c.start, c.query, c.ok)
		}
	}
}

func TestListProjectFiles(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "main.go"), "")
	mustWrite(t, filepath.Join(root, "internal", "tui", "tui.go"), "")
	mustWrite(t, filepath.Join(root, ".git", "config"), "")
	mustWrite(t, filepath.Join(root, "node_modules", "dep", "index.js"), "")

	got := listProjectFiles(root)
	have := map[string]bool{}
	for _, f := range got {
		have[f.path] = true
	}
	if !have["main.go"] || !have["internal/tui/tui.go"] || !have["internal"] {
		t.Errorf("expected project files and dirs in index, got %v", have)
	}
	for p := range have {
		if p == ".git" || p == "node_modules" ||
			filepath.Dir(p) == ".git" || filepath.Dir(p) == "node_modules" {
			t.Errorf("skipped dir leaked into index: %q", p)
		}
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
