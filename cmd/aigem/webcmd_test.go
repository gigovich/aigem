package main

import (
	"testing"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/tools"
	"github.com/gigovich/aigem/internal/web"
)

// Each conversation needs its own sandbox. Sharing one is not a resource
// question: the delegation and skill tools are registered into a registry bound
// to the confirmation function of whichever session registered them last, so a
// tool call in one conversation would ask another's clients for approval. The
// daemon's own tests cannot catch this - they inject their own factory - so the
// guard belongs against the one the command actually builds.
func TestWebFactoryBuildsARegistryPerSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	built := 0
	seen := map[*tools.Registry]bool{}
	o := webRun{
		client: llm.NewRef(llm.New("http://127.0.0.1:9", "t")),
		newRegistry: func() (*tools.Registry, error) {
			built++
			r, err := tools.NewRegistry(t.TempDir())
			if err != nil {
				return nil, err
			}
			seen[r] = true
			return r, nil
		},
		agents: agent.DefaultSubagents(),
		cwd:    ".",
	}
	f := o.factory()
	for range 2 {
		s, err := f(web.Spec{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
	}
	if built != 2 || len(seen) != 2 {
		t.Fatalf("built %d registries, %d distinct; two sessions need two", built, len(seen))
	}
}

// The daemon serves the directory it was started in. A session asked for
// somewhere else is refused rather than silently sandboxed to the wrong root.
func TestWebFactoryRefusesAnotherDirectory(t *testing.T) {
	o := webRun{
		newRegistry: func() (*tools.Registry, error) { return tools.NewRegistry(t.TempDir()) },
		cwd:         "/one",
	}
	if _, err := o.factory()(web.Spec{Cwd: "/two"}); err == nil {
		t.Fatal("a session in another directory was accepted")
	}
}
