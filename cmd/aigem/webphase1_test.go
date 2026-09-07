package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	b := newWebBackend(webBackendConfig{version: "test"})
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
	b := newWebBackend(webBackendConfig{version: "test"})
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
	b := newWebBackend(webBackendConfig{version: "test", env: &runner.Env{Skills: reg}})
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
	b := newWebBackend(webBackendConfig{version: "test"})
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
	b := newWebBackend(webBackendConfig{version: "test", runs: runs, activity: log,
		notify: func(kind string, _ any) { published = append(published, kind) }})
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
	other := newWebBackend(webBackendConfig{version: "test", activity: store.NewLog[web.Activity](log.Path())})
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
	b := newWebBackend(webBackendConfig{version: "test", env: env})

	// The readers have to be running *while* the approval replaces the catalog,
	// or this test proves nothing: eight goroutines that finish a hundred cheap
	// reads in microseconds are long gone by the time discovery has walked the
	// project. They are started, waited for, and then kept reading until the
	// approval has returned.
	var wg, ready sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		wg.Add(1)
		ready.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = b.Skills(context.Background())
				_, _ = b.Commands(context.Background())
			}
		}()
	}
	ready.Wait()
	approved, err := b.TrustSkills(context.Background())
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
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

// A project that defines no skills is a person asking for something that does
// not apply, not a daemon fault. Answering it as a fault puts a 500 and a log
// line in front of a browser for the ordinary case of an empty project.
func TestApprovingAProjectWithNoSkillsIsRefusedNotFailed(t *testing.T) {
	cwd := t.TempDir()
	env, _, err := runner.Load(context.Background(), runner.Options{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(env.Close)
	b := newWebBackend(webBackendConfig{version: "test", env: env})

	_, err = b.TrustSkills(context.Background())
	var refusal *web.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("TrustSkills on a project with no skills = %v, want a refusal", err)
	}
	if strings.Contains(refusal.Reason, cwd) {
		t.Fatalf("the refusal names a filesystem path: %q", refusal.Reason)
	}
}

func TestBackendShutdownCancelsProviderLogins(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := newWebBackend(webBackendConfig{version: "test"})
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
	if got.State != string(auth.FlowCancelled) || got.Error == "" {
		t.Fatalf("login after shutdown = %+v, want cancelled without a credential", got)
	}
}

// stubFlows hands out logins with no provider behind them, so the bookkeeping
// around one can be driven without binding the callback port.
func stubFlows(t *testing.T, started *int32) func(context.Context, string) (*auth.Flow, error) {
	t.Helper()
	return func(ctx context.Context, provider string) (*auth.Flow, error) {
		if started != nil {
			atomic.AddInt32(started, 1)
		}
		return auth.NewPendingFlow(ctx, provider), nil
	}
}

// The caps are what stands between a signed-in page and an unbounded number of
// outbound authorization requests. A cap that counted the wrong thing would let
// a loop through and be invisible until it was.
func TestPendingLoginsAreBoundedGloballyAndPerProvider(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := newWebBackend(webBackendConfig{version: "test", beginFlow: stubFlows(t, nil)})
	t.Cleanup(b.CloseBackend)

	var ids []string
	for range maxPendingLoginsPerProvider {
		v, err := b.BeginLogin(context.Background(), web.LoginRequest{Provider: "openai"})
		if err != nil {
			t.Fatalf("BeginLogin: %v", err)
		}
		ids = append(ids, v.ID)
	}
	_, err := b.BeginLogin(context.Background(), web.LoginRequest{Provider: "openai"})
	if !errors.Is(err, web.ErrBusy) {
		t.Fatalf("the %dth openai login = %v, want ErrBusy", maxPendingLoginsPerProvider+1, err)
	}
	// The per-provider cap is per provider: another one still has room.
	for range maxPendingLogins - maxPendingLoginsPerProvider {
		if _, err := b.BeginLogin(context.Background(), web.LoginRequest{Provider: "xai"}); err != nil {
			t.Fatalf("a second provider was refused by the first one's cap: %v", err)
		}
	}
	// The global cap cannot be reached separately today: two providers take
	// browser logins, and 2 x 4 is exactly 8, so the per-provider cap always
	// fires first. It is the bound that starts doing work when a third provider
	// gains one, and this pins that relationship rather than pretending to
	// exercise a path that does not exist.
	if maxPendingLogins > 2*maxPendingLoginsPerProvider {
		t.Fatalf("maxPendingLogins (%d) is above what the two providers with browser "+
			"logins can reach (%d); nothing bounds the total any more",
			maxPendingLogins, 2*maxPendingLoginsPerProvider)
	}
	// Finishing one gives its slot back, which is what makes the cap a bound on
	// what is in flight rather than on what has ever been asked for.
	if err := b.CancelLogin(context.Background(), ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := b.BeginLogin(context.Background(), web.LoginRequest{Provider: "openai"}); err != nil {
		t.Fatalf("a cancelled login did not give its slot back: %v", err)
	}
}

// A cancelled login is finished, not forgotten: the page that started it is
// still polling, and an id that vanished would answer 404 where the truth is
// that the person cancelled it.
func TestACancelledLoginKeepsItsRecordAndStopsItsWork(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := newWebBackend(webBackendConfig{version: "test", beginFlow: stubFlows(t, nil)})
	t.Cleanup(b.CloseBackend)

	v, err := b.BeginLogin(context.Background(), web.LoginRequest{Provider: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if v.State != string(auth.FlowPending) {
		t.Fatalf("a new login is %q, want pending", v.State)
	}
	if err := b.CancelLogin(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	got, err := b.Login(context.Background(), v.ID)
	if err != nil {
		t.Fatalf("a cancelled login is unreadable: %v", err)
	}
	if got.State != string(auth.FlowCancelled) || got.Error != "cancelled" {
		t.Fatalf("a cancelled login reads as %q/%q, want cancelled - a page must not be "+
			"shown a provider failure for something the person did", got.State, got.Error)
	}
	if got.URL != "" || got.Code != "" {
		t.Fatalf("a finished login kept its authorization URL or code: %+v", got)
	}
	if err := b.CancelLogin(context.Background(), "LOGIN-nope"); !errors.Is(err, web.ErrNoLogin) {
		t.Fatalf("cancelling an unknown login = %v, want ErrNoLogin", err)
	}
}

// Finished logins are kept so a page can read what happened, and evicted so a
// long-lived daemon does not hold every one it ever started.
func TestFinishedLoginsAreEvictedOldestFirst(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := newWebBackend(webBackendConfig{version: "test", beginFlow: stubFlows(t, nil)})
	t.Cleanup(b.CloseBackend)

	var ids []string
	for range maxRetainedTerminalLoginFlows + 4 {
		v, err := b.BeginLogin(context.Background(), web.LoginRequest{Provider: "openai"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, v.ID)
		// Cancelled and waited out one at a time, so the cap is never reached and
		// the order they finish in is the order they were started in.
		if err := b.CancelLogin(context.Background(), v.ID); err != nil {
			t.Fatal(err)
		}
	}
	// The eviction happens on the watcher goroutine, so wait for it rather than
	// racing it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		b.flowMu.Lock()
		n := len(b.flowOrder)
		b.flowMu.Unlock()
		if n <= maxRetainedTerminalLoginFlows {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d login records retained, want at most %d", n, maxRetainedTerminalLoginFlows)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The oldest went and the newest stayed.
	if _, err := b.Login(context.Background(), ids[0]); !errors.Is(err, web.ErrNoLogin) {
		t.Errorf("the oldest finished login was retained: %v", err)
	}
	if _, err := b.Login(context.Background(), ids[len(ids)-1]); err != nil {
		t.Errorf("the newest finished login was evicted: %v", err)
	}
}

// Shutdown owns the logins the daemon started: a flow left running would hold
// a listener and a goroutine past the process's own teardown.
func TestClosingTheBackendEndsEveryLoginItStarted(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := newWebBackend(webBackendConfig{version: "test", beginFlow: stubFlows(t, nil)})
	v, err := b.BeginLogin(context.Background(), web.LoginRequest{Provider: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	b.CloseBackend()
	b.CloseBackend()
	got, err := b.Login(context.Background(), v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(auth.FlowCancelled) {
		t.Fatalf("a login survived shutdown as %q", got.State)
	}
	if _, err := b.BeginLogin(context.Background(), web.LoginRequest{Provider: "openai"}); err == nil {
		t.Fatal("a closed backend started another login")
	}
}

// The feed and the announcement are one thing: a page told the collection
// changed refetches it, and finding nothing new there is worse than never
// having been told. So the durable append has to succeed first.
func TestAnActivityThatCouldNotBeRecordedIsNotAnnounced(t *testing.T) {
	// A log whose directory cannot be created, so every append fails.
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	log := store.NewLog[web.Activity](filepath.Join(blocked, "activity.jsonl"))
	var published []string
	b := newWebBackend(webBackendConfig{version: "test", activity: log,
		notify: func(kind string, _ any) { published = append(published, kind) }})

	if b.recordActivity(web.Activity{Kind: "run.created", Text: "Run created"}) {
		t.Fatal("recordActivity reported success against a log it cannot write")
	}
	if len(published) != 0 {
		t.Fatalf("published %v for an append that failed", published)
	}
}

// Two tabs pressing Close at the same moment. The run ends once, so the feed
// says so once: a second line would have the person reading that they closed
// the same conversation twice.
func TestTwoConcurrentClosesRecordOneClosure(t *testing.T) {
	log := store.NewLog[web.Activity](filepath.Join(t.TempDir(), "activity.jsonl"))
	runs, _ := testRuns(t)
	b := newWebBackend(webBackendConfig{version: "test", runs: runs, activity: log})
	run := openTestRun(t, b)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = b.CloseRun(context.Background(), run.ID)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
	got, err := b.Activity(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var closed int
	for _, a := range got {
		if a.Kind == "run.closed" {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("two concurrent closes recorded %d closures, want one: %+v", closed, got)
	}
}

// Setting the default to what is already saved is not a change. Treating it as
// one makes a durable write, a feed entry and a broadcast out of a button a
// person can hold down, and the state directory grows for as long as they do.
func TestSettingTheDefaultModelToWhatIsSavedChangesNothing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	log := store.NewLog[web.Activity](filepath.Join(t.TempDir(), "activity.jsonl"))
	var published int
	b := newWebBackend(webBackendConfig{version: "test", activity: log,
		notify: func(kind string, _ any) {
			if kind == "model.default" {
				published++
			}
		}})

	models, err := b.Models(context.Background())
	if err != nil || len(models) == 0 {
		t.Fatalf("Models = %+v, %v", models, err)
	}
	ref := models[0].Ref
	// The first call saves, even when that model was already what the daemon
	// would have chosen: nothing was written down before.
	if _, err := b.SetDefaultModel(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if config.LoadPrefs().Model != ref {
		t.Fatalf("the first call saved %q, want %q", config.LoadPrefs().Model, ref)
	}
	after, err := b.Activity(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := b.SetDefaultModel(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
	}
	got, err := b.Activity(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(after) {
		t.Fatalf("repeating the same default appended %d entries", len(got)-len(after))
	}
	if published != 1 {
		t.Fatalf("the daemon announced %d changes, want the one that happened", published)
	}
}

// Env.Pending is the snapshot Load took, and the first approval clears it. A
// skill added or edited after that is genuinely pending, so a gate that trusts
// the snapshot refuses the only route that can pick it up - for the life of the
// daemon, on a catalog that is quietly out of date.
func TestASkillAddedAfterAnApprovalCanStillBeApproved(t *testing.T) {
	cwd := t.TempDir()
	writeProjectSkill(t, cwd, "one")
	env, _, err := runner.Load(context.Background(), runner.Options{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(env.Close)
	b := newWebBackend(webBackendConfig{version: "test", env: env})

	first, err := b.TrustSkills(context.Background())
	if err != nil {
		t.Fatalf("the first approval: %v", err)
	}
	if len(first.Loaded) != 1 {
		t.Fatalf("the first approval loaded %v, want the one skill", first.Loaded)
	}
	// Nothing pending now, so a second press is refused - that is the bound.
	if _, err := b.TrustSkills(context.Background()); err == nil {
		t.Fatal("approving with nothing pending succeeded")
	}

	writeProjectSkill(t, cwd, "two")
	// The page has to be able to see it before anyone can press the button.
	listed, err := b.Skills(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if listed.Pending == nil || len(listed.Pending.Names) == 0 {
		t.Fatalf("a skill added after the approval is not reported as pending: %+v", listed.Pending)
	}
	second, err := b.TrustSkills(context.Background())
	if err != nil {
		t.Fatalf("approving a skill added after the first approval: %v", err)
	}
	if len(second.Loaded) != 2 {
		t.Fatalf("the second approval loaded %v, want both skills", second.Loaded)
	}
	if _, err := b.Skill(context.Background(), "two"); err != nil {
		t.Fatalf("the newly approved skill is not readable: %v", err)
	}
}

func writeProjectSkill(t *testing.T, cwd, name string) {
	t.Helper()
	dir := filepath.Join(cwd, ".skills", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: the " + name + " skill\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A daemon built without a run registry must answer, not panic: the feature map
// withdraws the screen, but a client that ignores it still reaches the route,
// and a nil dereference there takes down the handler goroutine.
func TestRunRoutesWithoutARegistryAreUnavailableRatherThanFatal(t *testing.T) {
	b := newWebBackend(webBackendConfig{version: "test"})
	if _, err := b.Runs(context.Background()); !errors.Is(err, web.ErrUnavailable) {
		t.Fatalf("Runs without a registry = %v, want ErrUnavailable", err)
	}
	if _, err := b.Run(context.Background(), "RUN-1"); !errors.Is(err, web.ErrUnavailable) {
		t.Fatalf("Run without a registry = %v, want ErrUnavailable", err)
	}
	if err := b.CloseRun(context.Background(), "RUN-1"); !errors.Is(err, web.ErrUnavailable) {
		t.Fatalf("CloseRun without a registry = %v, want ErrUnavailable", err)
	}
	if _, err := b.RunEvents(context.Background(), "RUN-1", 0, 0); !errors.Is(err, web.ErrUnavailable) {
		t.Fatalf("RunEvents without a registry = %v, want ErrUnavailable", err)
	}
	if _, err := b.RunBlob(context.Background(), "RUN-1", 1); !errors.Is(err, web.ErrUnavailable) {
		t.Fatalf("RunBlob without a registry = %v, want ErrUnavailable", err)
	}
	if _, err := b.WatchRun(context.Background(), "RUN-1", web.RunClient{}, 0); !errors.Is(
		err, web.ErrUnavailable) {
		t.Fatalf("WatchRun without a registry = %v, want ErrUnavailable", err)
	}
	if _, err := b.RunArtifacts(context.Background(), "RUN-1"); !errors.Is(err, web.ErrUnavailable) {
		t.Fatalf("RunArtifacts without a registry = %v, want ErrUnavailable", err)
	}
	if err := b.ApplyRunOp(context.Background(), "RUN-1", web.RunOp{Op: "submit"}); !errors.Is(
		err, web.ErrUnavailable) {
		t.Fatalf("ApplyRunOp without a registry = %v, want ErrUnavailable", err)
	}
	if _, err := b.OpenRun(context.Background(), web.NewRun{}); !errors.Is(err, web.ErrUnavailable) {
		t.Fatalf("OpenRun without a registry = %v, want ErrUnavailable", err)
	}
	// And the feature map says so, so a page never offers the screen.
	var withdrawn bool
	for _, name := range b.Unavailable() {
		if name == "runs" {
			withdrawn = true
		}
	}
	if !withdrawn {
		t.Fatalf("Unavailable() = %v, want it to withdraw runs", b.Unavailable())
	}
}

// A provider that never answers must not hold the request, and must not let a
// page abandon requests to start an unbounded number of outbound calls: the
// slot is held until the call it reserved actually returns.
func TestALoginStartThatHangsIsBoundedAndKeepsItsSlot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	release := make(chan struct{})
	var started int32
	b := newWebBackend(webBackendConfig{version: "test",
		beginFlow: func(ctx context.Context, provider string) (*auth.Flow, error) {
			atomic.AddInt32(&started, 1)
			<-release
			return auth.NewPendingFlow(ctx, provider), nil
		}})
	t.Cleanup(func() { close(release); b.CloseBackend() })

	// The request's own context is what a browser closing a tab cancels.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := b.BeginLogin(ctx, web.LoginRequest{Provider: "openai"})
		done <- err
	}()
	waitFor(t, func() bool { return atomic.LoadInt32(&started) == 1 })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("an abandoned login start = %v, want the request's own cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("BeginLogin ignored its request context and waited for the provider")
	}
	// The reservation is still held, so the abandoned call cannot be repeated
	// without limit while the provider is still not answering.
	b.flowMu.Lock()
	held := b.flowStarting["openai"]
	b.flowMu.Unlock()
	if held != 1 {
		t.Fatalf("the abandoned start holds %d openai slots, want 1", held)
	}
}

// waitFor polls until cond holds, failing rather than hanging.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the condition")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
