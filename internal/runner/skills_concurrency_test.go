package runner

import (
	"os"
	"path/filepath"
	"testing"

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
	<-started // approval now owns env.sessMu
	attached := make(chan error, 1)
	go func() { attached <- env.Attach(sess) }()
	select {
	case err := <-attached:
		t.Fatalf("Attach returned during approval: %v", err)
	default:
	}
	close(release)
	if err := <-approvedDone; err != nil {
		t.Fatal(err)
	}
	if err := <-attached; err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get(agent.SkillToolName); !ok {
		t.Fatal("the concurrently attached session missed the approved catalog")
	}
}
