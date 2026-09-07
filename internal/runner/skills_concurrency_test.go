package runner

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
)

func TestAttachCannotMissConcurrentSkillApproval(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "skills", "greet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: greet\ndescription: greet\n---\nhello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	approved, errs := skill.DiscoverDir(filepath.Join(root, "skills"))
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	reg, err := tools.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	sess := NewSession(Spec{
		Tools: reg, Backend: llm.NewRef(llm.New("http://127.0.0.1:9", "test")),
	})
	t.Cleanup(sess.Local.Close)
	env := &Env{Cwd: root}

	started := make(chan struct{})
	release := make(chan struct{})
	oldApprove := approveEnvironmentSkills
	approveEnvironmentSkills = func(_ string, _ *skill.Registry, _ ...*Session) (SkillApproval, error) {
		close(started)
		<-release
		return SkillApproval{Catalog: approved, Loaded: []string{"greet"}}, nil
	}
	t.Cleanup(func() { approveEnvironmentSkills = oldApprove })

	approvedDone := make(chan error, 1)
	go func() {
		_, err := env.ApproveProjectSkills()
		approvedDone <- err
	}()
	await(t, started, "the approval to take the environment's lock")
	attached := make(chan error, 1)
	go func() { attached <- env.Attach(sess) }()
	// Long enough that Attach has certainly been scheduled and has certainly
	// reached the lock. Reading the channel immediately would pass whether or
	// not Attach blocks, because the goroutine has not run yet either way.
	select {
	case err := <-attached:
		t.Fatalf("Attach returned while the approval held the lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := awaitErr(t, approvedDone, "the approval to finish"); err != nil {
		t.Fatal(err)
	}
	if err := awaitErr(t, attached, "Attach to finish"); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get(agent.SkillToolName); !ok {
		t.Fatal("the concurrently attached session missed the approved catalog")
	}
}

// await and awaitErr fail rather than hang: a guard added above the stub below
// would otherwise turn this test into a package-timeout with no explanation.
func await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func awaitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}
