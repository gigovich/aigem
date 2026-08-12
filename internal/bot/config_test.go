package bot

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gigovich/aigem/internal/agent"
)

func intPtr(n int) *int { return &n }

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	in := Config{
		Name:    "amiran",
		Role:    "developer",
		Workdir: "/tmp/repo",
		Persona: "female; use feminine forms in Russian",
	}
	if err := Save(in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load("amiran")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Role != "developer" || got.Workdir != "/tmp/repo" ||
		got.Persona != in.Persona {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestSaveKeepsFileModeAndLeavesNoTemp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	c := Config{Name: "amiran", Role: "developer"}
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	dir, _ := Dir("amiran")
	path := filepath.Join(dir, "bot.yaml")
	// Not 0600: that is what CreateTemp produces anyway, so it would pass even if
	// the mode were never carried over.
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}

	c.Model = "openai/gpt-5.6-sol"
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("Save widened a hardened bot.yaml to %v", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "bot.yaml" {
			t.Fatalf("Save left %s behind", e.Name())
		}
	}
	if got, err := Load("amiran"); err != nil || got.Model != "openai/gpt-5.6-sol" {
		t.Fatalf("model = %q, %v", got.Model, err)
	}
}

func TestListAndRemove(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, n := range []string{"amiran", "bob"} {
		if err := Save(Config{Name: n, Role: "developer"}); err != nil {
			t.Fatal(err)
		}
	}
	names, err := List()
	if err != nil || len(names) != 2 {
		t.Fatalf("List = %v, %v", names, err)
	}
	if err := Remove("bob"); err != nil {
		t.Fatal(err)
	}
	names, err = List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "amiran" {
		t.Fatalf("after Remove, List = %v", names)
	}
}

func TestTurnBudgetConfDefaults(t *testing.T) {
	got, err := (TurnBudgetConf{}).ResolveTurnBudget()
	if err != nil {
		t.Fatal(err)
	}
	want := agent.DefaultTurnBudget()
	if got != want {
		t.Fatalf("default turn budget = %+v, want %+v", got, want)
	}
}

func TestTurnBudgetConfOverrides(t *testing.T) {
	got, err := (TurnBudgetConf{
		MaxModelRounds:       intPtr(7),
		MaxToolCalls:         intPtr(11),
		MaxRepeatedToolCalls: intPtr(3),
		MaxDuration:          "45m",
	}).ResolveTurnBudget()
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxModelRounds != 7 || got.MaxToolCalls != 11 || got.MaxRepeatedToolCalls != 3 || got.MaxDuration != 45*time.Minute {
		t.Fatalf("unexpected overridden turn budget: %+v", got)
	}
}

func TestTurnBudgetForRole(t *testing.T) {
	dev := TurnBudgetForRole("developer")
	if dev.MaxModelRounds != 120 || dev.MaxToolCalls != 300 || dev.MaxDuration != 45*time.Minute {
		t.Fatalf("developer budget = %+v, want the larger allowance", dev)
	}
	for _, r := range []string{"manager", "researcher", "architect", "tester", "unknown"} {
		if got := TurnBudgetForRole(r); got != agent.DefaultTurnBudget() {
			t.Fatalf("role %q budget = %+v, want default", r, got)
		}
	}
}

func TestResolveTurnBudgetForRoleBaseAndOverrides(t *testing.T) {
	base := TurnBudgetForRole("developer")
	// No overrides: the developer base is used verbatim.
	got, err := (TurnBudgetConf{}).ResolveTurnBudgetFor(base)
	if err != nil {
		t.Fatal(err)
	}
	if got != base {
		t.Fatalf("no-override resolve = %+v, want developer base %+v", got, base)
	}
	// A bot override still wins over the role base.
	got, err = (TurnBudgetConf{MaxModelRounds: intPtr(200)}).ResolveTurnBudgetFor(base)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxModelRounds != 200 || got.MaxToolCalls != base.MaxToolCalls {
		t.Fatalf("override on role base = %+v, want rounds=200 and base tool calls", got)
	}
}

func TestTurnBudgetConfExplicitZeroDisables(t *testing.T) {
	got, err := (TurnBudgetConf{
		MaxModelRounds:       intPtr(0),
		MaxToolCalls:         intPtr(0),
		MaxRepeatedToolCalls: intPtr(0),
		MaxDuration:          "0",
	}).ResolveTurnBudget()
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxModelRounds != 0 || got.MaxToolCalls != 0 || got.MaxRepeatedToolCalls != 0 || got.MaxDuration != 0 {
		t.Fatalf("explicit zero values should disable every budget, got %+v", got)
	}
}

func TestTurnBudgetConfYAMLExplicitZero(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte(`
name: jane
turnBudget:
  maxModelRounds: 0
  maxToolCalls: 0
  maxRepeatedToolCalls: 0
  maxDuration: "0"
`), &c); err != nil {
		t.Fatal(err)
	}
	got, err := c.TurnBudget.ResolveTurnBudget()
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxModelRounds != 0 || got.MaxToolCalls != 0 || got.MaxRepeatedToolCalls != 0 || got.MaxDuration != 0 {
		t.Fatalf("yaml explicit zero values should disable every budget, got %+v", got)
	}
}

func TestTurnBudgetConfRejectsBadDuration(t *testing.T) {
	if _, err := (TurnBudgetConf{MaxDuration: "soon"}).ResolveTurnBudget(); err == nil {
		t.Fatal("expected bad duration error")
	}
}

func TestResolveLLMPaceFactor(t *testing.T) {
	if got := (Config{}).ResolveLLMPaceFactor(); got != DefaultLLMPaceFactor {
		t.Fatalf("unset = %v, want default %v", got, DefaultLLMPaceFactor)
	}
	f := 2.5
	if got := (Config{LLMPaceFactor: &f}).ResolveLLMPaceFactor(); got != 2.5 {
		t.Fatalf("set = %v, want 2.5", got)
	}
	zero := 0.0
	if got := (Config{LLMPaceFactor: &zero}).ResolveLLMPaceFactor(); got != 0 {
		t.Fatalf("zero = %v, want 0 (disabled)", got)
	}
}
