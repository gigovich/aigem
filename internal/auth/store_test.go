package auth

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/gigovich/aigem/internal/llm"
)

func TestStoreRoundTripAndPerms(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)

	if _, ok, err := Get("openai"); err != nil || ok {
		t.Fatalf("expected empty store: ok=%v err=%v", ok, err)
	}
	if err := Put("openai", Record{Kind: KindAPIKey, Key: "sk-secret"}); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := Get("openai")
	if err != nil || !ok || rec.Key != "sk-secret" {
		t.Fatalf("round-trip failed: %+v ok=%v err=%v", rec, ok, err)
	}

	info, err := os.Stat(filepath.Join(state, "aigem", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("auth.json perms = %o, want 600", perm)
	}

	if err := Delete("openai"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := Get("openai"); ok {
		t.Fatal("record should be gone after delete")
	}
}

func TestCredentialFromEnvAndStore(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// Env key resolves even with no stored record.
	t.Setenv("OPENAI_API_KEY", "sk-env")
	cred, err := Credential(context.Background(), llm.OpenAIProviderID)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Kind != llm.AuthAPIKey {
		t.Fatalf("kind = %q", cred.Kind)
	}
	tok, _ := cred.Token(context.Background())
	if tok != "sk-env" {
		t.Fatalf("token = %q", tok)
	}
	if !IsAuthenticated(llm.OpenAIProviderID) {
		t.Fatal("env key should authenticate openai")
	}

	// Stored API key used when env is absent.
	t.Setenv("OPENAI_API_KEY", "")
	if err := Put(llm.OpenAIProviderID, Record{Kind: KindAPIKey, Key: "sk-stored"}); err != nil {
		t.Fatal(err)
	}
	cred, _ = Credential(context.Background(), llm.OpenAIProviderID)
	tok, _ = cred.Token(context.Background())
	if tok != "sk-stored" {
		t.Fatalf("stored token = %q", tok)
	}

	// Unauthenticated provider yields Kind=none.
	if err := Delete(llm.OpenAIProviderID); err != nil {
		t.Fatal(err)
	}
	cred, _ = Credential(context.Background(), llm.OpenAIProviderID)
	if cred.Kind != llm.AuthNone {
		t.Fatalf("expected none, got %q", cred.Kind)
	}
}

func TestCredentialForModelPrefersEnvAPIKeyForNonCodexWhenOAuthStored(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "sk-env")
	if err := Put(llm.OpenAIProviderID, Record{
		Kind:  KindOAuth,
		Token: &oauth2.Token{AccessToken: "oauth-token", Expiry: time.Now().Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	cred, err := CredentialForModel(context.Background(), llm.OpenAIProviderID, "custom-api-only")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Kind != llm.AuthAPIKey {
		t.Fatalf("non-Codex model should use env API key, got %q", cred.Kind)
	}
	cred, err = CredentialForModel(context.Background(), llm.OpenAIProviderID, "gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Kind != llm.AuthOAuthChatGPT {
		t.Fatalf("Codex model should keep OAuth credential, got %q", cred.Kind)
	}
}

func TestAccountIDFromIDToken(t *testing.T) {
	// header.payload.signature with payload claiming chatgpt_account_id.
	payload := `{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-123"}}`
	idTok := "x." + b64url(payload) + ".y"
	if got := accountIDFromIDToken(idTok); got != "acct-123" {
		t.Fatalf("account id = %q", got)
	}
	if accountIDFromIDToken("bad") != "" {
		t.Fatal("malformed token should yield empty account id")
	}
}

func b64url(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
