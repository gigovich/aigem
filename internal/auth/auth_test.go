package auth

import (
	"context"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/gigovich/aigem/internal/llm"
)

// A peer process may rotate the shared one-time-use refresh token and write a
// fresh access token to auth.json. persistSource must reload that token instead
// of refreshing with its stale in-memory snapshot (which OpenAI would reject as
// reused). Here the disk token is still valid, so the reload needs no network.
func TestPersistSourceReloadsPeerToken(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	fresh := &oauth2.Token{
		AccessToken:  "peer-fresh",
		RefreshToken: "rt-new",
		Expiry:       time.Now().Add(time.Hour),
	}
	if err := Put("openai", Record{Kind: KindOAuth, Token: fresh}); err != nil {
		t.Fatal(err)
	}

	stale := Record{Kind: KindOAuth, Token: &oauth2.Token{
		AccessToken:  "stale",
		RefreshToken: "rt-old",
		Expiry:       time.Now().Add(-time.Hour),
	}}
	s := &persistSource{ctx: context.Background(), provider: "openai", rec: stale}

	tok, err := s.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "peer-fresh" {
		t.Fatalf("access token = %q, want peer-fresh (reloaded from disk)", tok.AccessToken)
	}
}

func TestXAICredentialKinds(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	t.Setenv("XAI_API_KEY", "xk-env")
	cred, err := Credential(context.Background(), llm.XAIProviderID)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Kind != llm.AuthAPIKey {
		t.Fatalf("env kind = %q", cred.Kind)
	}
	if !IsAuthenticated(llm.XAIProviderID) {
		t.Fatal("env key should authenticate xai")
	}

	// The env key stays the winner even with a stored OAuth record: it is the
	// 403 tier-gating escape hatch and must work without a logout.
	if err := Put(llm.XAIProviderID, Record{Kind: KindOAuth, Token: &oauth2.Token{
		AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour),
	}, TokenURL: "https://auth.x.ai/oauth2/token"}); err != nil {
		t.Fatal(err)
	}
	cred, err = Credential(context.Background(), llm.XAIProviderID)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Kind != llm.AuthAPIKey {
		t.Fatalf("env-over-oauth kind = %q", cred.Kind)
	}
	if got := Describe(llm.XAIProviderID); got != "api key (env)" {
		t.Fatalf("describe with env = %q", got)
	}

	// Without the env key the stored record maps to the xai OAuth kind.
	t.Setenv("XAI_API_KEY", "")
	cred, err = Credential(context.Background(), llm.XAIProviderID)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Kind != llm.AuthOAuthXAI {
		t.Fatalf("stored kind = %q", cred.Kind)
	}
	tok, err := cred.Token(context.Background())
	if err != nil || tok != "at" {
		t.Fatalf("token = %q, err = %v", tok, err)
	}
	if got := Describe(llm.XAIProviderID); got != "grok subscription" {
		t.Fatalf("describe = %q", got)
	}
}

func TestOAuthConfigForSelectsProvider(t *testing.T) {
	cfg := oauthConfigFor(llm.XAIProviderID, Record{TokenURL: "https://auth.x.ai/custom/token"})
	if cfg.ClientID != xaiClientID || cfg.Endpoint.TokenURL != "https://auth.x.ai/custom/token" {
		t.Fatalf("xai config = %+v", cfg)
	}
	cfg = oauthConfigFor(llm.XAIProviderID, Record{})
	if cfg.Endpoint.TokenURL != xaiTokenURLFallback {
		t.Fatalf("xai fallback token url = %q", cfg.Endpoint.TokenURL)
	}
	cfg = oauthConfigFor(llm.OpenAIProviderID, Record{})
	if cfg.ClientID != chatGPTClientID {
		t.Fatalf("openai config = %+v", cfg)
	}
}

func TestXAIEndpointOK(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://auth.x.ai/oauth2/token":  true,
		"http://auth.x.ai/oauth2/token":   false,
		"https://evil.example/token":      false,
		"https://auth.x.ai.evil.example/": false,
		"":                                false,
	} {
		if got := xaiEndpointOK(raw); got != want {
			t.Errorf("xaiEndpointOK(%q) = %v, want %v", raw, got, want)
		}
	}
}
