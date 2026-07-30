package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// "Always (this folder)" must grant exactly the folder the confirmation box
// named. When the approved path is itself a directory, taking its parent would
// silently widen the grant to every sibling - approving .../data would hand over
// all of .../secrets too.
func TestAllowDirOnDirectoryDoesNotGrantParent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	base := t.TempDir()
	root := filepath.Join(base, "workdir")
	parent := filepath.Join(base, "srv")
	approved := filepath.Join(parent, "data")
	sibling := filepath.Join(parent, "secrets")
	for _, d := range []string{root, approved, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(sibling, "token.txt")
	if err := os.WriteFile(secret, []byte("sk-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	r.SetPathGrants(true)

	asked := 0
	r.SetPathApprover(func(string, PathIntent) PathDecision {
		asked++
		return PathAllowDir
	})

	// Approve the directory itself, the way `list_dir <dir>` would.
	if _, err := run(t, r, "list_dir", map[string]any{"path": approved}); err != nil {
		t.Fatalf("list_dir on the approved directory failed: %v", err)
	}
	if asked != 1 {
		t.Fatalf("expected exactly one approval prompt, got %d", asked)
	}

	// The grant must cover the approved directory without asking again...
	if _, err := run(t, r, "list_dir", map[string]any{"path": approved}); err != nil {
		t.Fatalf("approved directory was not remembered: %v", err)
	}
	if asked != 1 {
		t.Errorf("approved directory asked again (%d prompts); the grant did not stick", asked)
	}

	// ...but must NOT silently cover a sibling under the parent.
	asked = 0
	r.SetPathApprover(func(string, PathIntent) PathDecision {
		asked++
		return PathDeny
	})
	if out, err := run(t, r, "read_file", map[string]any{"path": secret}); err == nil {
		t.Errorf("a sibling of the approved directory was readable without approval: %q", out)
	}
	if asked == 0 {
		t.Error("reading a sibling did not prompt; the grant leaked to the parent directory")
	}
}
