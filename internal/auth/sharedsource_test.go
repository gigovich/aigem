package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// TestRotatedTokenSurvivesAFailedWrite: the provider has already spent the old refresh token by
// the time the store write is attempted, so a failed write must not also cost us the replacement.
func TestRotatedTokenSurvivesAFailedWrite(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(blocker, "state")) // every write under it fails

	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{
		Transport: rotatingTokenTransport{},
	})
	src := &persistSource{ctx: ctx, provider: "openai", rec: Record{
		Kind:  KindOAuth,
		Token: &oauth2.Token{AccessToken: "old", RefreshToken: "r1", Expiry: time.Now().Add(-time.Minute)},
	}}

	tok, err := src.Token()
	if err != nil {
		t.Fatalf("a failed store write threw the rotated token away: %v", err)
	}
	if tok.AccessToken != "new" {
		t.Fatalf("access token = %q, want the rotated one", tok.AccessToken)
	}
	// The burned r1 must not stay in memory either, or the next refresh presents it and the
	// provider rejects it.
	if src.rec.Token.RefreshToken != "r2" {
		t.Fatalf("in-memory refresh token = %q, want the rotated one", src.rec.Token.RefreshToken)
	}
}

// TestAFailedWriteDoesNotResurrectTheBurnedToken: the reload-from-disk that lets peers share one
// auth.json must not run while the disk copy is the credential this source already spent.
func TestAFailedWriteDoesNotResurrectTheBurnedToken(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only directory this test needs")
	}
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	stored := oauthRecord("r1")
	stored.Token.Expiry = time.Now().Add(-time.Minute)
	if err := Put("openai", stored); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(state, "aigem"), 0o500); err != nil { // readable, unwritable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(state, "aigem"), 0o700) })

	tr := &oneShotRefreshTransport{}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: tr})
	src := &persistSource{ctx: ctx, provider: "openai", rec: Record{
		Kind:  KindOAuth,
		Token: &oauth2.Token{AccessToken: "at", RefreshToken: "r1", Expiry: time.Now().Add(-time.Minute)},
	}}

	if _, err := src.Token(); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	src.rec.Token.Expiry = time.Now().Add(-time.Minute) // the next call must refresh again
	if _, err := src.Token(); err != nil {
		t.Fatalf("second refresh reused a spent refresh token: %v", err)
	}
	if !tr.rotations() {
		t.Fatal("the transport never saw a refresh")
	}
}

// oneShotRefreshTransport answers refreshes with a token whose refresh half rotates every time,
// and rejects any refresh token it has already answered - the one-time-use rule that makes a
// stale copy of auth.json dangerous.
type oneShotRefreshTransport struct {
	mu    sync.Mutex
	spent map[string]bool
	n     int
}

func (t *oneShotRefreshTransport) rotations() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.n > 0
}

func (t *oneShotRefreshTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	sent := form.Get("refresh_token")
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.spent == nil {
		t.spent = map[string]bool{}
	}
	if t.spent[sent] {
		return jsonResponse(400, `{"error":"invalid_grant"}`), nil
	}
	t.spent[sent] = true
	t.n++
	next := fmt.Sprintf(`{"access_token":"at%d","refresh_token":"r%d","token_type":"Bearer","expires_in":3600}`,
		t.n+1, t.n+1)
	return jsonResponse(200, next), nil
}

type rotatingTokenTransport struct{}

func (rotatingTokenTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return jsonResponse(200,
		`{"access_token":"new","refresh_token":"r2","token_type":"Bearer","expires_in":3600}`), nil
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestSourceKeysSeparateProviders(t *testing.T) {
	if sourceKey("openai", oauthRecord("r1")) == sourceKey("xai", oauthRecord("r1")) {
		t.Fatal("two providers sharing a refresh token would share one source")
	}
}
