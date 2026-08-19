package bot

import (
	"os"
	"os/exec"
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

func TestModelSelection(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want ModelSelection
	}{
		{
			name: "architect role default",
			cfg:  Config{Role: "architect"},
			want: ModelSelection{Effective: DefaultArchitectModel, Source: ModelSourceRoleDefault},
		},
		{
			name: "manager role default",
			cfg:  Config{Role: "manager"},
			want: ModelSelection{Effective: DefaultBotModel, Source: ModelSourceRoleDefault},
		},
		{
			name: "researcher role default",
			cfg:  Config{Role: "researcher"},
			want: ModelSelection{Effective: DefaultBotModel, Source: ModelSourceRoleDefault},
		},
		{
			name: "developer role default",
			cfg:  Config{Role: "developer"},
			want: ModelSelection{Effective: DefaultBotModel, Source: ModelSourceRoleDefault},
		},
		{
			name: "tester role default",
			cfg:  Config{Role: "tester"},
			want: ModelSelection{Effective: DefaultBotModel, Source: ModelSourceRoleDefault},
		},
		{
			name: "trimmed configured override",
			cfg:  Config{Role: "architect", Model: "  openai/gpt-5.4  "},
			want: ModelSelection{Configured: "openai/gpt-5.4", Effective: "openai/gpt-5.4", Source: ModelSourceConfigured},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cfg.ModelSelection()
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("selection = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestModelSelectionRejectsUnknownRoleWithoutOverride(t *testing.T) {
	if _, err := (Config{Role: "developre"}).ModelSelection(); err == nil {
		t.Fatal("expected unknown role error")
	}
	got, err := (Config{Role: "developre", Model: "openai/gpt-5.4"}).ModelSelection()
	if err != nil {
		t.Fatalf("configured override should remain readable: %v", err)
	}
	if got.Effective != "openai/gpt-5.4" || got.Source != ModelSourceConfigured {
		t.Fatalf("configured selection = %+v", got)
	}
}

func TestUpdateSerializesDisjointChangesAcrossProcesses(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := Save(Config{
		Name: "amiran", Role: "developer", Model: "openai/gpt-5.4",
		Cron: []CronJob{{ID: "old", Expr: "0 * * * *", Prompt: "old"}},
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configHome, "aigem", "bots", "amiran", "bot.yaml")
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	parentDone := make(chan error, 1)
	go func() {
		_, err := Update("amiran", func(c *Config) error {
			close(entered)
			<-release
			c.Model = DefaultBotModel
			return nil
		})
		parentDone <- err
	}()
	<-entered

	ready := filepath.Join(configHome, "child-ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestUpdateProcessHelper$")
	cmd.Env = append(os.Environ(), "AIGEM_UPDATE_HELPER=cron", "AIGEM_UPDATE_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready)
	childDone := make(chan error, 1)
	go func() { childDone <- cmd.Wait() }()
	select {
	case err := <-childDone:
		t.Fatalf("child update completed while parent held the lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if err := <-parentDone; err != nil {
		t.Fatal(err)
	}
	if err := <-childDone; err != nil {
		t.Fatal(err)
	}
	got, err := Load("amiran")
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != DefaultBotModel {
		t.Fatalf("model = %q, want %q", got.Model, DefaultBotModel)
	}
	if len(got.Cron) != 1 || got.Cron[0].ID != "new" {
		t.Fatalf("cron = %+v, want child update", got.Cron)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("Update changed bot.yaml mode to %v", info.Mode().Perm())
	}
}

func TestUpdateLockIsReleasedWhenProcessExits(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := Save(Config{Name: "amiran", Role: "developer"}); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(configHome, "child-ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestUpdateProcessHelper$")
	cmd.Env = append(os.Environ(), "AIGEM_UPDATE_HELPER=hold", "AIGEM_UPDATE_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed helper unexpectedly succeeded")
	}
	if _, err := Update("amiran", func(c *Config) error {
		c.Model = DefaultBotModel
		return nil
	}); err != nil {
		t.Fatalf("update after lock holder exited: %v", err)
	}
}

func TestUpdateProcessHelper(t *testing.T) {
	mode := os.Getenv("AIGEM_UPDATE_HELPER")
	if mode == "" {
		return
	}
	ready := os.Getenv("AIGEM_UPDATE_READY")
	switch mode {
	case "cron":
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Update("amiran", func(c *Config) error {
			c.Cron = []CronJob{{ID: "new", Expr: "5 * * * *", Prompt: "new"}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	case "hold":
		if _, err := Update("amiran", func(*Config) error {
			if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
				return err
			}
			select {}
		}); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
