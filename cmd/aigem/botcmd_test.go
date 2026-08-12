package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/bot"
)

// isolatedBots points the config and state dirs at temp dirs, registers an
// auth-free and a logged-out provider in the user models.json, and saves the
// named bots. Each bot carries cron and turn-budget settings so a model switch,
// which rewrites the whole file, is checked for clobbering them.
func isolatedBots(t *testing.T, names ...string) {
	t.Helper()
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("XAI_API_KEY", "")
	// "keyed" is authenticated but cannot be opened with an API key on the
	// Responses API: it separates "a credential exists" from "this model opens".
	models := `{"providers":[{"id":"acme","base_url":"https://acme.test","api":"openai-completions",
"auth":"none","models":[{"id":"fast","name":"Acme Fast","context_window":100000}]},
{"id":"locked","base_url":"https://locked.test","api":"openai-completions","auth":"apikey",
"models":[{"id":"big","name":"Locked Big","context_window":100000}]},
{"id":"keyed","base_url":"https://keyed.test","api":"openai-responses","auth":"apikey",
"models":[{"id":"resp","name":"Keyed Responses","context_window":100000}]}]}`
	dir := filepath.Join(cfgHome, "aigem")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(models), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := auth.Put("keyed", auth.Record{Kind: auth.KindAPIKey, Key: "k"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		c := bot.Config{
			Name: name, Role: "developer", Workdir: t.TempDir(),
			TurnBudget: bot.TurnBudgetConf{MaxDuration: "45m"},
			Cron:       []bot.CronJob{{ID: "job1", Expr: "5 */2 * * *", Prompt: "check the board"}},
		}
		if err := bot.Save(c); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBotModelSetsNormalizesAndClears(t *testing.T) {
	isolatedBots(t, "kate")

	if err := botModel([]string{"kate", "acme/fast"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	c, err := bot.Load("kate")
	if err != nil || c.Model != "acme/fast" {
		t.Fatalf("model = %q, %v", c.Model, err)
	}
	// The whole file is rewritten on a switch; everything else must survive.
	if len(c.Cron) != 1 || c.Cron[0].ID != "job1" || c.TurnBudget.MaxDuration != "45m" ||
		c.Persona != "" {
		t.Fatalf("switch clobbered other config: %+v", c)
	}

	// A bare id is stored normalized as provider/id.
	if err := botModel([]string{"kate", "fast"}); err != nil {
		t.Fatalf("set bare id: %v", err)
	}
	if c, _ := bot.Load("kate"); c.Model != "acme/fast" {
		t.Fatalf("bare id stored as %q, want acme/fast", c.Model)
	}

	if err := botModel([]string{"kate", "--clear"}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if c, _ := bot.Load("kate"); c.Model != "" {
		t.Fatalf("model after clear = %q, want empty", c.Model)
	}

	dir, _ := bot.Dir("kate")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("left a temporary file behind: %s", e.Name())
		}
	}
}

func TestBotModelAllSwitchesEveryBotAndClears(t *testing.T) {
	isolatedBots(t, "kate", "lisa")

	if err := botModel([]string{"--all", "acme/fast"}); err != nil {
		t.Fatalf("--all: %v", err)
	}
	for _, n := range []string{"kate", "lisa"} {
		c, _ := bot.Load(n)
		if c.Model != "acme/fast" {
			t.Fatalf("%s model = %q, want acme/fast", n, c.Model)
		}
		if len(c.Cron) != 1 || c.TurnBudget.MaxDuration != "45m" {
			t.Fatalf("--all clobbered %s's other config: %+v", n, c)
		}
	}

	if err := botModel([]string{"--all", "--clear"}); err != nil {
		t.Fatalf("--all --clear: %v", err)
	}
	for _, n := range []string{"kate", "lisa"} {
		if c, _ := bot.Load(n); c.Model != "" {
			t.Fatalf("%s model after clear = %q, want empty", n, c.Model)
		}
	}
}

func TestBotModelRejectsBadRefsWithoutWriting(t *testing.T) {
	isolatedBots(t, "kate", "lisa")
	if err := botModel([]string{"--all", "acme/fast"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	// "locked/big" needs an API key that is not stored; "keyed/resp" has one but
	// cannot be opened with it; " " would otherwise reach Resolve, which reads a
	// blank ref as "the default model".
	for _, ref := range []string{"acme/nope", "ghost/fast", "locked/big", "keyed/resp", "  "} {
		if err := botModel([]string{"kate", ref}); err == nil {
			t.Fatalf("botModel(kate, %q) succeeded, want an error", ref)
		}
		if err := botModel([]string{"--all", ref}); err == nil {
			t.Fatalf("botModel(--all, %q) succeeded, want an error", ref)
		}
		for _, n := range []string{"kate", "lisa"} {
			if c, _ := bot.Load(n); c.Model != "acme/fast" {
				t.Fatalf("rejected ref %q changed %s to %q", ref, n, c.Model)
			}
		}
	}
}

// A bot named after a bare model id is what makes "--all <name>" ambiguous: it
// would resolve, and every bot would be switched instead of the one named.
func TestBotModelAllRejectsABotNameThatResolvesAsAModel(t *testing.T) {
	isolatedBots(t, "fast", "lisa")
	if err := botModel([]string{"--all", "fast"}); err == nil {
		t.Fatal("--all with a bot name succeeded, want an error")
	}
	for _, n := range []string{"fast", "lisa"} {
		if c, _ := bot.Load(n); c.Model != "" {
			t.Fatalf("%s was switched to %q", n, c.Model)
		}
	}
}

func TestBotModelReportsPinnedUnusableAndAuto(t *testing.T) {
	isolatedBots(t, "kate", "lisa", "zoe")
	if err := botModel([]string{"kate", "acme/fast"}); err != nil {
		t.Fatal(err)
	}
	// "locked" needs an API key that was never stored, so lisa is pinned to a model
	// the CLI would refuse to set - the state a logout leaves behind. zoe stays
	// unpinned, giving the report one row of each kind.
	c, _ := bot.Load("lisa")
	c.Model = "locked/big"
	if err := bot.Save(c); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := botModel(nil); err != nil {
			t.Fatal(err)
		}
	})
	rows := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			rows[f[0]] = f[1:]
		}
	}
	for name, want := range map[string][]string{
		"kate": {"acme/fast", "configured"},
		"lisa": {"locked/big", "UNUSABLE"},
		"zoe":  {"auto"},
	} {
		got := strings.Join(rows[name], " ")
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Fatalf("row %q = %q, want it to contain %q\nfull report:\n%s", name, got, w, out)
			}
		}
	}
	// The unpinned row still names the model that would be opened today.
	if len(rows["zoe"]) < 2 {
		t.Fatalf("unpinned row has no model column: %q\n%s", rows["zoe"], out)
	}
}

func TestBotModelNoChangeAndNoBots(t *testing.T) {
	isolatedBots(t, "kate")
	if err := botModel([]string{"kate", "acme/fast"}); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := botModel([]string{"kate", "acme/fast"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "no change") {
		t.Fatalf("re-setting the same ref should report no change, got:\n%s", out)
	}

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	out = captureStdout(t, func() {
		if err := botModel(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "no bots configured") {
		t.Fatalf("empty bots dir should say so, got:\n%s", out)
	}
}

// captureStdout runs f with os.Stdout redirected to a pipe and returns what it wrote.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		io.Copy(&b, r)
		done <- b.String()
	}()
	// Restore in a defer: f calls t.Fatal on failure, which unwinds past the rest
	// of this function and would otherwise leave every later test writing into the
	// pipe.
	defer func() {
		w.Close()
		os.Stdout = orig
	}()
	f()
	w.Close()
	os.Stdout = orig
	return <-done
}

// The scheduler rewrites the whole bot.yaml whenever the bot adds, removes, or
// expires a job. It must not carry a stale model back to disk, or a switch made
// while the bot is running is lost before the restart that would apply it.
func TestSaveCronJobsKeepsAModelSetWhileRunning(t *testing.T) {
	isolatedBots(t, "kate")
	if err := botModel([]string{"kate", "acme/fast"}); err != nil {
		t.Fatal(err)
	}
	if err := saveCronJobs("kate")([]bot.CronJob{{ID: "job2", Expr: "0 * * * *", Prompt: "poll"}}); err != nil {
		t.Fatal(err)
	}
	c, err := bot.Load("kate")
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "acme/fast" {
		t.Fatalf("cron persist reverted the model to %q", c.Model)
	}
	if len(c.Cron) != 1 || c.Cron[0].ID != "job2" {
		t.Fatalf("cron jobs not persisted: %+v", c.Cron)
	}
}

func TestBotModelExplainsAProjectOnlyRef(t *testing.T) {
	isolatedBots(t, "kate")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".aigem"), 0o755); err != nil {
		t.Fatal(err)
	}
	projJSON := `{"providers":[{"id":"repo","base_url":"https://repo.test","api":"openai-completions",
"auth":"none","models":[{"id":"only-here","context_window":100}]}]}`
	if err := os.WriteFile(filepath.Join(dir, ".aigem", "models.json"), []byte(projJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	err := botModel([]string{"kate", "repo/only-here"})
	if err == nil {
		t.Fatal("pinning a project-only ref succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "project-local") {
		t.Fatalf("error does not explain why: %v", err)
	}
	// A credential failure must not get the same explanation.
	err = botModel([]string{"kate", "locked/big"})
	if err == nil || strings.Contains(err.Error(), "project-local") {
		t.Fatalf("credential failure got the project-file hint: %v", err)
	}
}

func TestBotModelRejectsMalformedArgs(t *testing.T) {
	isolatedBots(t, "kate")
	for _, args := range [][]string{
		{"--all"},                    // no ref
		{"--all", "a", "b"},          // two refs
		{"--all", "kate"},            // a bot name is not a ref
		{"--all", "--clear", "kate"}, // clear-all takes no names
		{"--clear"},                  // no bot name
		{"a", "b", "c"},              // too many positionals
		{"--bogus"},                  // unknown flag
		{"ghost"},                    // unknown bot, report mode
		{"ghost", "--clear"},         // unknown bot, clear
	} {
		if err := botModel(args); err == nil {
			t.Fatalf("botModel(%v) succeeded, want an error", args)
		}
	}
}
