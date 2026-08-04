package auth

import (
	"context"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func oauthRecord(refresh string) Record {
	return Record{
		Kind:  KindOAuth,
		Token: &oauth2.Token{AccessToken: "at", RefreshToken: refresh, Expiry: time.Now().Add(time.Hour)},
	}
}

func TestSharedSourceIsReusedForTheSameCredential(t *testing.T) {
	t.Cleanup(ResetSources)
	ResetSources()
	rec := oauthRecord("r1")
	first := sharedSource(context.Background(), "openai", rec)
	second := sharedSource(context.Background(), "openai", rec)
	// One source per provider is the whole point: several bots refreshing the same single-use
	// refresh token at once is what got their tokens rejected.
	if first != second {
		t.Fatal("a second caller for the same credential built its own token source")
	}
}

func TestSharedSourceIsReplacedWhenTheCredentialChanges(t *testing.T) {
	t.Cleanup(ResetSources)
	ResetSources()
	old := sharedSource(context.Background(), "openai", oauthRecord("r1"))
	fresh := sharedSource(context.Background(), "openai", oauthRecord("r2"))
	if old == fresh {
		t.Fatal("a new login reused the source built from the credential it replaced")
	}
	// The superseded entry must not linger, or the map grows by one per token rotation.
	sourceMu.Lock()
	n := len(sourceCache)
	sourceMu.Unlock()
	if n != 1 {
		t.Fatalf("cache holds %d sources for one provider, want 1", n)
	}
}

func TestResetSourcesDropsEverything(t *testing.T) {
	t.Cleanup(ResetSources)
	ResetSources()
	before := sharedSource(context.Background(), "openai", oauthRecord("r1"))
	ResetSources()
	after := sharedSource(context.Background(), "openai", oauthRecord("r1"))
	// A logout must not leave a source that keeps presenting the credential just removed.
	if before == after {
		t.Fatal("ResetSources kept a source built before the credential was cleared")
	}
}

func TestSourceKeysSeparateProviders(t *testing.T) {
	if sourceKey("openai", oauthRecord("r1")) == sourceKey("xai", oauthRecord("r1")) {
		t.Fatal("two providers sharing a refresh token would share one source")
	}
}
