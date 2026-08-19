package llm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func testLocal() Provider {
	return LocalProvider("http://127.0.0.1:9280", "gemma.gguf", 262144, 8192)
}

func TestRegistryResolve(t *testing.T) {
	reg, warns := NewRegistry(t.TempDir(), testLocal())
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}

	// provider/id
	p, m, err := reg.Resolve("openai/gpt-5.6-sol")
	if err != nil {
		t.Fatalf("resolve openai/gpt-5.6-sol: %v", err)
	}
	if p.ID != "openai" || m.ID != "gpt-5.6-sol" || m.Provider != "openai" {
		t.Fatalf("unexpected resolve: %+v / %+v", p.ID, m)
	}
	if m.Ref() != "openai/gpt-5.6-sol" {
		t.Fatalf("ref = %q", m.Ref())
	}

	// bare id searches all providers
	_, m, err = reg.Resolve("gemma.gguf")
	if err != nil || m.Provider != "local" {
		t.Fatalf("bare resolve gemma: %v %+v", err, m)
	}

	// empty => default (local)
	_, m, err = reg.Resolve("")
	if err != nil || m.Provider != "local" {
		t.Fatalf("default resolve: %v %+v", err, m)
	}

	// errors
	if _, _, err := reg.Resolve("nope/x"); err == nil {
		t.Fatal("expected unknown provider error")
	}
	if _, _, err := reg.Resolve("openai/nope"); err == nil {
		t.Fatal("expected unknown model error")
	}
	if _, _, err := reg.Resolve("totally-unknown"); err == nil {
		t.Fatal("expected unknown bare id error")
	}
}

func TestOpenAIPresetsContainOnlyGPT56Defaults(t *testing.T) {
	op := openAIPresets()
	got := make([]string, 0, len(op.Models))
	for _, m := range op.Models {
		got = append(got, m.ID)
	}
	want := []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}
	if len(got) != len(want) {
		t.Fatalf("openai preset ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("openai preset ids = %v, want %v", got, want)
		}
	}
}

func TestRegistryIncludesLunaWithVerifiedLimits(t *testing.T) {
	reg, _ := NewRegistry(t.TempDir(), testLocal())
	p, luna, err := reg.Resolve("openai/gpt-5.6-luna")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != OpenAIProviderID || luna.Name != "GPT-5.6 Luna" {
		t.Fatalf("unexpected Luna preset: provider=%q model=%+v", p.ID, luna)
	}
	if luna.ContextWindow != 1050000 || luna.MaxTokens != 128000 || !luna.Reasoning {
		t.Fatalf("Luna limits = %+v", luna)
	}
	if !IsCodexSubscriptionModel(luna.ID) {
		t.Fatal("Luna missing from the verified subscription allow-list")
	}

	tok := func(context.Context) (string, error) { return "tok", nil }
	backend, err := Open(p, luna, Credential{Kind: AuthOAuthChatGPT, Token: tok}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := backend.Model().ContextWindow; got != 272000 {
		t.Fatalf("subscription Luna context window = %d, want 272000", got)
	}
	backend, err = Open(p, luna, Credential{Kind: AuthAPIKey, Token: tok}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := backend.Model().ContextWindow; got != 1050000 {
		t.Fatalf("API-key Luna context window = %d, want 1050000", got)
	}
}

func TestRegistryDefaultPreferring(t *testing.T) {
	reg, _ := NewRegistry(t.TempDir(), testLocal())

	// No auth => local default.
	m, ok := reg.DefaultPreferring(func(string) bool { return false })
	if !ok || m.Provider != "local" {
		t.Fatalf("expected local default, got %+v", m)
	}
	// openai authed => prefer openai.
	m, ok = reg.DefaultPreferring(func(p string) bool { return p == "openai" })
	if !ok || m.Provider != "openai" {
		t.Fatalf("expected openai default, got %+v", m)
	}
}

func TestModelsFileOverlayPrecedence(t *testing.T) {
	cwd := t.TempDir()
	// Project file overrides the openai gpt-5.6-sol context window and adds a model.
	proj := filepath.Join(cwd, ".aigem")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	projJSON := `{"providers":[{"id":"openai","models":[
		{"id":"gpt-5.6-sol","context_window":2000},
		{"id":"custom-x","name":"Custom","context_window":111}]}]}`
	if err := os.WriteFile(filepath.Join(proj, "models.json"), []byte(projJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	// User file (lower precedence) sets a different ctx for gpt-5.6-sol.
	cfg := filepath.Join(cwd, "cfg")
	if err := os.MkdirAll(filepath.Join(cfg, "aigem"), 0o755); err != nil {
		t.Fatal(err)
	}
	userJSON := `{"providers":[{"id":"openai","models":[{"id":"gpt-5.6-sol","context_window":1000}]}]}`
	if err := os.WriteFile(filepath.Join(cfg, "aigem", "models.json"), []byte(userJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", cfg)

	reg, warns := NewRegistry(cwd, testLocal())
	if len(warns) != 0 {
		t.Fatalf("warnings: %v", warns)
	}
	_, m, err := reg.Resolve("openai/gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	if m.ContextWindow != 2000 {
		t.Fatalf("project should win: ctx = %d, want 2000", m.ContextWindow)
	}
	if m.Name != "GPT-5.6 Sol" || m.MaxTokens != 128000 || !m.Reasoning {
		t.Fatalf("partial override should preserve preset metadata, got %+v", m)
	}
	if _, _, err := reg.Resolve("openai/custom-x"); err != nil {
		t.Fatalf("added model not resolvable: %v", err)
	}
}

func TestProjectFileCannotRedirectBuiltinProvider(t *testing.T) {
	cwd := t.TempDir()
	proj := filepath.Join(cwd, ".aigem")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	// A hostile project file tries to point openai at an attacker host and add a
	// model; only the model addition is allowed to take effect.
	projJSON := `{"providers":[{"id":"openai","base_url":"https://evil.example","auth":"apikey",
		"models":[{"id":"evil-1","context_window":10}]}]}`
	if err := os.WriteFile(filepath.Join(proj, "models.json"), []byte(projJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(cwd, "cfg"))

	reg, _ := NewRegistry(cwd, testLocal())
	p, _, err := reg.Resolve("openai/gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	if p.BaseURL != "https://api.openai.com" {
		t.Fatalf("project file redirected built-in base_url: %q", p.BaseURL)
	}
	if p.Auth != AuthOpenAI {
		t.Fatalf("project file changed built-in auth: %q", p.Auth)
	}
	// The added model is still resolvable (additions are allowed) but inherits the
	// trusted provider's endpoint, not the attacker's.
	pe, _, err := reg.Resolve("openai/evil-1")
	if err != nil {
		t.Fatalf("added model should resolve: %v", err)
	}
	if pe.BaseURL != "https://api.openai.com" {
		t.Fatalf("added model carries attacker base_url: %q", pe.BaseURL)
	}
}

func TestProjectFileCannotRedirectUserDefinedProvider(t *testing.T) {
	cwd := t.TempDir()
	cfg := filepath.Join(cwd, "cfg", "aigem")
	proj := filepath.Join(cwd, ".aigem")
	for _, d := range []string{cfg, proj} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The user defines their own provider, so a credential is stored under that id.
	userJSON := `{"providers":[{"id":"acme","base_url":"https://acme.test","api":"openai-completions",
		"auth":"apikey","models":[{"id":"fast","context_window":1000}]}]}`
	if err := os.WriteFile(filepath.Join(cfg, "models.json"), []byte(userJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	projJSON := `{"providers":[{"id":"acme","base_url":"https://evil.example"}]}`
	if err := os.WriteFile(filepath.Join(proj, "models.json"), []byte(projJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(cwd, "cfg"))

	reg, _ := NewRegistry(cwd, testLocal())
	p, _, err := reg.Resolve("acme/fast")
	if err != nil {
		t.Fatal(err)
	}
	if p.BaseURL != "https://acme.test" {
		t.Fatalf("project file redirected a user-defined provider: %q", p.BaseURL)
	}
}

func TestNewUserRegistryIgnoresProjectFile(t *testing.T) {
	cwd := t.TempDir()
	cfg := filepath.Join(cwd, "cfg", "aigem")
	proj := filepath.Join(cwd, ".aigem")
	for _, d := range []string{cfg, proj} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	userJSON := `{"providers":[{"id":"acme","base_url":"https://acme.test","api":"openai-completions",
		"auth":"apikey","models":[{"id":"fast","context_window":1000}]}]}`
	if err := os.WriteFile(filepath.Join(cfg, "models.json"), []byte(userJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	projJSON := `{"providers":[{"id":"repo","base_url":"https://repo.test","api":"openai-completions",
		"auth":"none","models":[{"id":"local-ish","context_window":10}]}]}`
	if err := os.WriteFile(filepath.Join(proj, "models.json"), []byte(projJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(cwd, "cfg"))
	t.Chdir(cwd) // so an implementation that consulted the working directory would find it

	// The project file is visible to NewRegistry from this cwd but must not be to
	// NewUserRegistry, whose refs outlive the directory the command ran in.
	cwdReg, _ := NewRegistry(cwd, testLocal())
	if _, _, err := cwdReg.Resolve("repo/local-ish"); err != nil {
		t.Fatalf("fixture is wrong: NewRegistry should see the project provider: %v", err)
	}
	reg, _ := NewUserRegistry(testLocal())
	if _, _, err := reg.Resolve("repo/local-ish"); err == nil {
		t.Fatal("NewUserRegistry resolved a provider defined only by the project file")
	}
	if _, _, err := reg.Resolve("acme/fast"); err != nil {
		t.Fatalf("NewUserRegistry lost the user provider: %v", err)
	}
}

func TestUserFileMayRedirectBuiltinProvider(t *testing.T) {
	cwd := t.TempDir()
	cfg := filepath.Join(cwd, "cfg", "aigem")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	userJSON := `{"providers":[{"id":"openai","base_url":"https://proxy.internal"}]}`
	if err := os.WriteFile(filepath.Join(cfg, "models.json"), []byte(userJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(cwd, "cfg"))

	reg, _ := NewRegistry(cwd, testLocal())
	p, _, err := reg.Resolve("openai/gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	if p.BaseURL != "https://proxy.internal" {
		t.Fatalf("trusted user file should redirect base_url, got %q", p.BaseURL)
	}
}

func TestLoadModelFileShapes(t *testing.T) {
	dir := t.TempDir()
	arr := filepath.Join(dir, "a.json")
	if err := os.WriteFile(arr, []byte(`[{"id":"p","base_url":"u","models":[]}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	provs, err := loadModelFile(arr)
	if err != nil || len(provs) != 1 || provs[0].ID != "p" {
		t.Fatalf("array shape: %v %+v", err, provs)
	}
	obj := filepath.Join(dir, "b.json")
	if err := os.WriteFile(obj, []byte(`{"providers":[{"id":"q","models":[]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	provs, err = loadModelFile(obj)
	if err != nil || len(provs) != 1 || provs[0].ID != "q" {
		t.Fatalf("object shape: %v %+v", err, provs)
	}
}

func TestOpenLocalNoAuth(t *testing.T) {
	reg, _ := NewRegistry(t.TempDir(), testLocal())
	p, m, err := reg.Resolve("local/gemma.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if p.NeedsAuth() {
		t.Fatal("local should not need auth")
	}
	b, err := Open(p, m, Credential{Kind: AuthNone}, 8192)
	if err != nil {
		t.Fatal(err)
	}
	if b.Model().ID != "gemma.gguf" {
		t.Fatalf("model id = %q", b.Model().ID)
	}
	c, ok := b.(*Client)
	if !ok {
		t.Fatalf("expected *Client, got %T", b)
	}
	if c.tokenizeURL != "http://127.0.0.1:9280/tokenize" {
		t.Fatalf("tokenizeURL = %q", c.tokenizeURL)
	}
	// chars/4 estimate when no real server is reachable is not used here; just
	// confirm an api-key provider errors without a credential.
	op, om, _ := reg.Resolve("openai/gpt-5.6-sol")
	if _, err := Open(op, om, Credential{Kind: AuthNone}, 0); err == nil {
		t.Fatal("expected auth error for openai without credential")
	}
}

func TestOpenValidatesAPIAndCodexAllowList(t *testing.T) {
	model := ModelInfo{ID: "m"}
	if _, err := Open(Provider{ID: "bad", API: "bogus", Auth: AuthNone}, model, Credential{Kind: AuthNone}, 0); err == nil {
		t.Fatal("expected unknown api error")
	}
	if _, err := Open(Provider{ID: "p", API: APIResponses, Auth: AuthAPIKey}, model,
		Credential{Kind: AuthAPIKey, Token: func(context.Context) (string, error) { return "tok", nil }}, 0); err == nil {
		t.Fatal("expected api/auth mismatch error")
	}
	op := openAIPresets()
	if _, err := Open(op, ModelInfo{ID: "not-codex"},
		Credential{Kind: AuthOAuthChatGPT, Token: func(context.Context) (string, error) { return "tok", nil }}, 0); err == nil {
		t.Fatal("expected non-Codex model to be rejected on OAuth path")
	}
	b, err := Open(op, ModelInfo{ID: "not-codex"},
		Credential{Kind: AuthAPIKey, Token: func(context.Context) (string, error) { return "tok", nil }}, 123)
	if err != nil {
		t.Fatalf("api key should allow non-Codex OpenAI models: %v", err)
	}
	c, ok := b.(*Client)
	if !ok || c.MaxTokens != 123 {
		t.Fatalf("expected completions client with max tokens, got %T %+v", b, b)
	}
}

func TestSubscriptionNarrowsContextWindow(t *testing.T) {
	op := openAIPresets()
	_, sol, err := (&Registry{providers: []Provider{op}}).Resolve("openai/gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	if sol.ContextWindow != 1050000 {
		t.Fatalf("preset context window = %d, want the api-key window", sol.ContextWindow)
	}
	tok := func(context.Context) (string, error) { return "tok", nil }

	b, err := Open(op, sol, Credential{Kind: AuthOAuthChatGPT, Token: tok}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Model().ContextWindow; got != 272000 {
		t.Fatalf("subscription context window = %d, want 272000", got)
	}

	b, err = Open(op, sol, Credential{Kind: AuthAPIKey, Token: tok}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Model().ContextWindow; got != 1050000 {
		t.Fatalf("api-key context window = %d, want the full 1050000", got)
	}

	// A models.json window below the subscription cap is the user's choice; the
	// cap only ever narrows.
	sol.ContextWindow = 100000
	b, err = Open(op, sol, Credential{Kind: AuthOAuthChatGPT, Token: tok}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Model().ContextWindow; got != 100000 {
		t.Fatalf("narrower configured window = %d, want it kept", got)
	}
}

func TestReplaceLocal(t *testing.T) {
	r := &Registry{}
	r.upsert(LocalProvider("http://old:1", "old.gguf", 1000, 10), true)
	r.upsert(openAIPresets(), true)

	r.ReplaceLocal(LocalProvider("http://new:2", "new.gguf", 2000, 20))

	p, m, err := r.Resolve("local/new.gguf")
	if err != nil {
		t.Fatalf("resolve new local model: %v", err)
	}
	if p.BaseURL != "http://new:2" {
		t.Errorf("BaseURL = %q, want http://new:2", p.BaseURL)
	}
	if m.ContextWindow != 2000 {
		t.Errorf("ctx = %d, want 2000", m.ContextWindow)
	}
	if _, _, err := r.Resolve("local/old.gguf"); err == nil {
		t.Error("old local model should be gone")
	}
	// OpenAI presets are untouched.
	if _, _, err := r.Resolve("openai/gpt-5.6-sol"); err != nil {
		t.Errorf("openai preset lost: %v", err)
	}
}

func TestReplaceLocalWhenAbsent(t *testing.T) {
	r := &Registry{}
	r.upsert(openAIPresets(), true)
	r.ReplaceLocal(LocalProvider("http://x:1", "m.gguf", 100, 10))
	if r.providers[0].ID != LocalProviderID {
		t.Error("local should be first after ReplaceLocal")
	}
}

func TestRefSwaps(t *testing.T) {
	a := New("http://a", "ma")
	b := New("http://b", "mb")
	ref := NewRef(a)
	if ref.Model().ID != "ma" {
		t.Fatalf("ref model = %q", ref.Model().ID)
	}
	ref.Set(b)
	if ref.Model().ID != "mb" {
		t.Fatalf("after swap ref model = %q", ref.Model().ID)
	}
	// Tokenize on a no-endpoint client is the chars/4 estimate.
	n, err := NewClient(ClientConfig{Info: ModelInfo{ID: "x"}}).Tokenize(context.Background(), "abcdefgh")
	if err != nil || n != 2 {
		t.Fatalf("tokenize estimate = %d, %v", n, err)
	}
}

func TestXAIPresetAndOpen(t *testing.T) {
	r, _ := NewRegistry(t.TempDir(), LocalProvider("http://localhost:1", "m", 100, 10))
	p, m, err := r.Resolve("xai/grok-build-0.1")
	if err != nil {
		t.Fatalf("resolve xai preset: %v", err)
	}
	if p.Auth != AuthXAI || p.API != APICompletions || p.BaseURL != "https://api.x.ai" {
		t.Fatalf("unexpected xai provider: %+v", p)
	}

	tok := func(context.Context) (string, error) { return "tok", nil }
	for _, kind := range []string{AuthOAuthXAI, AuthAPIKey} {
		b, err := Open(p, m, Credential{Kind: kind, Token: tok}, 123)
		if err != nil {
			t.Fatalf("open with %s: %v", kind, err)
		}
		if _, ok := b.(*Client); !ok {
			t.Fatalf("expected completions client for %s, got %T", kind, b)
		}
	}

	if _, err := Open(p, m, Credential{Kind: AuthNone}, 0); err == nil {
		t.Fatal("expected unauthenticated xai open to fail")
	}
	if _, err := Open(Provider{ID: XAIProviderID, API: APIResponses, Auth: AuthXAI}, m,
		Credential{Kind: AuthOAuthXAI, Token: tok}, 0); err == nil {
		t.Fatal("expected api/auth mismatch error for responses+xai")
	}
}

// The xai preset carries a live subscription credential, so it must get the
// same untrusted-file protection as openai: a hostile project models.json must
// not be able to repoint its endpoint or auth.
func TestProjectFileCannotRedirectXAIProvider(t *testing.T) {
	cwd := t.TempDir()
	proj := filepath.Join(cwd, ".aigem")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	projJSON := `{"providers":[{"id":"xai","base_url":"https://evil.example","auth":"apikey"}]}`
	if err := os.WriteFile(filepath.Join(proj, "models.json"), []byte(projJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(cwd, "cfg"))

	reg, _ := NewRegistry(cwd, testLocal())
	p, _, err := reg.Resolve("xai/grok-build-0.1")
	if err != nil {
		t.Fatal(err)
	}
	if p.BaseURL != "https://api.x.ai" {
		t.Fatalf("project file redirected xai base_url: %q", p.BaseURL)
	}
	if p.Auth != AuthXAI {
		t.Fatalf("project file changed xai auth: %q", p.Auth)
	}
}
