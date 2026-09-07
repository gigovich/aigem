package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"

	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/store"
	"github.com/gigovich/aigem/internal/web"
)

func TestWebModelsExposeOnlyTransportMetadataAndPersistCanonicalDefault(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := newWebBackend("test", nil, nil)
	models, err := b.Models(context.Background())
	if err != nil || len(models) == 0 {
		t.Fatalf("Models = %+v, %v", models, err)
	}
	m := models[0]
	if strings.Contains(m.Name, "http://") || strings.Contains(m.Name, "Authorization") {
		t.Fatalf("model metadata exposes provider configuration: %+v", m)
	}
	got, err := b.SetDefaultModel(context.Background(), m.Ref)
	if err != nil {
		t.Fatalf("SetDefaultModel: %v", err)
	}
	if got.Ref != m.Ref || !got.Default || config.LoadPrefs().Model != m.Ref {
		t.Fatalf("default = %+v, saved %q, want %q", got, config.LoadPrefs().Model, m.Ref)
	}
	if _, err := b.SetDefaultModel(context.Background(), "missing/model"); err == nil {
		t.Fatal("an unknown model became the default")
	}
}

func TestWebModelsRejectOAuthModelsThatCannotOpen(t *testing.T) {
	state, cfg := t.TempDir(), t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("OPENAI_API_KEY", "")
	if err := os.MkdirAll(filepath.Join(cfg, "aigem"), 0o700); err != nil {
		t.Fatal(err)
	}
	modelsJSON := `{"providers":[{"id":"openai","models":[{"id":"not-codex","name":"Not Codex"}]}]}`
	if err := os.WriteFile(filepath.Join(cfg, "aigem", "models.json"), []byte(modelsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := auth.Put("openai", auth.Record{Kind: auth.KindOAuth, Token: &oauth2.Token{AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	b := newWebBackend("test", nil, nil)
	models, err := b.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if m.Ref == "openai/not-codex" && m.Authenticated {
			t.Fatalf("unsupported OAuth model reported authenticated: %+v", m)
		}
	}
	if _, err := b.SetDefaultModel(context.Background(), "openai/not-codex"); err == nil {
		t.Fatal("unsupported OAuth model became the default")
	}
}

func TestSkillDetailIsStaticAndContainsNoAbsoluteSourcePaths(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "skills", "inspect")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "executed")
	body := "---\nname: inspect\ndescription: inspect safely\npaths:\n  - '*.go'\n  - " + filepath.ToSlash(root) + "/secret\n---\n\n!`touch " + marker + "`\n" + strings.Repeat("x", maxSkillPreview)
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, errs := skill.DiscoverDir(filepath.Join(root, "skills"))
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	b := newWebBackend("test", nil, nil, webBackendOptions{env: &runner.Env{Skills: reg}})
	got, err := b.Skill(context.Background(), "inspect")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Body, "touch ") {
		t.Fatalf("detail did not return the static markdown body: %q", got.Body)
	}
	if !got.BodyTruncated || len(got.Body) != maxSkillPreview {
		t.Fatalf("large detail was not explicitly capped: bytes=%d truncated=%v", len(got.Body), got.BodyTruncated)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("serving a detail executed its dynamic injection: %v", err)
	}
	if len(got.Paths) != 1 || got.Paths[0] != "*.go" {
		t.Fatalf("paths = %v, want relative metadata only", got.Paths)
	}
}

type limitsBackend struct {
	llm.Backend
	onLimits func(llm.Limits)
}

func (*limitsBackend) UsageReport() llm.UsageReport                                { return llm.UsageReport{} }
func (*limitsBackend) OnCall(func(llm.Usage, llm.UsageReport))                     {}
func (*limitsBackend) OnCallCtx(func(context.Context, llm.Usage, llm.UsageReport)) {}
func (b *limitsBackend) OnLimits(f func(llm.Limits))                               { b.onLimits = f }

func TestWebModelLimitPersistenceSurvivesSwitch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	first := &limitsBackend{Backend: llm.New("http://127.0.0.1:9", "one")}
	ref := webModelRef(first)
	first.onLimits(llm.Limits{Provider: "first", Windows: []llm.LimitWindow{{Name: "requests", Remaining: "1"}}})
	second := &limitsBackend{Backend: llm.New("http://127.0.0.1:9", "two")}
	ref.Set(second)
	second.onLimits(llm.Limits{Provider: "second", Windows: []llm.LimitWindow{{Name: "tokens", Remaining: "2"}}})
	stored := llm.LoadLimits()
	if stored["first"].Windows[0].Remaining != "1" || stored["second"].Windows[0].Remaining != "2" {
		t.Fatalf("saved limits after switch = %+v", stored)
	}
}

func TestWebUsageIncludesAuthenticatedProviderWithoutSnapshot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XAI_API_KEY", "key")
	b := newWebBackend("test", nil, nil)
	usage, err := b.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range usage {
		if got.Provider == "xai" {
			if got.Windows == nil {
				t.Fatal("provider without a snapshot had null windows")
			}
			return
		}
	}
	t.Fatalf("authenticated xai missing from usage: %+v", usage)
}

func TestActivityPersistsAndRunMutationsAppendExactlyOnce(t *testing.T) {
	log := store.NewLog[web.Activity](filepath.Join(t.TempDir(), "activity.jsonl"))
	runs, _ := testRuns(t)
	var published []string
	b := newWebBackend("test", nil, runs, webBackendOptions{activity: log, notify: func(kind string, _ any) {
		published = append(published, kind)
	}})
	run := openTestRun(t, b)
	if err := b.CloseRun(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.CloseRun(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := b.Activity(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != "run.created" || got[1].Kind != "run.closed" {
		t.Fatalf("activity = %+v, want one creation and one closure", got)
	}
	if got[0].Seq != 1 || got[1].Seq != 2 || got[0].At.IsZero() {
		t.Fatalf("activity cursors/times were not restored from the log: %+v", got)
	}
	if len(published) != 2 || published[0] != "activity.updated" || published[1] != "activity.updated" {
		t.Fatalf("activity publications = %v, want exactly the two successful appends", published)
	}
	other := newWebBackend("test", nil, nil, webBackendOptions{activity: store.NewLog[web.Activity](log.Path())})
	resumed, err := other.Activity(context.Background(), 1, 100)
	if err != nil || len(resumed) != 1 || resumed[0].Seq != 2 {
		t.Fatalf("persisted activity after 1 = %+v, %v", resumed, err)
	}
}

func TestSkillReadsAndApprovalAreSynchronized(t *testing.T) {
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".skills", "project-one")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: project-one\ndescription: project skill\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(cwd, ".skills", "broken")
	if err := os.MkdirAll(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "SKILL.md"), []byte("not valid frontmatter"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, _, err := runner.Load(context.Background(), runner.Options{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(env.Close)
	b := newWebBackend("test", nil, nil, webBackendOptions{env: env})

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_, _ = b.Skills(context.Background())
				_, _ = b.Commands(context.Background())
			}
		}()
	}
	approved, err := b.TrustSkills(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if len(approved.Loaded) != 1 || approved.Loaded[0] != "project-one" {
		t.Fatalf("approval = %+v", approved)
	}
	if len(approved.Notices) != 1 || strings.Contains(strings.Join(approved.Notices, " "), cwd) {
		t.Fatalf("approval notices exposed a source path: %+v", approved.Notices)
	}
	if _, err := b.Skill(context.Background(), "project-one"); err != nil {
		t.Fatalf("approved skill is not readable: %v", err)
	}
}

func TestBackendShutdownCancelsProviderLogins(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := newWebBackend("test", nil, nil)
	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	login, err := b.BeginLogin(requestCtx, web.LoginRequest{Provider: "openai"})
	if err != nil {
		t.Skipf("the fixed OAuth callback port is unavailable: %v", err)
	}
	b.CloseBackend()
	b.CloseBackend()
	got, err := b.Login(context.Background(), login.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(auth.FlowFailed) || got.Error == "" {
		t.Fatalf("login after shutdown = %+v, want failed without a credential", got)
	}
}
