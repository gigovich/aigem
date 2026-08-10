package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"golang.org/x/oauth2"

	"github.com/gigovich/aigem/internal/llm"
)

// Verified ChatGPT (Codex public client) OAuth constants.
const (
	chatGPTClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	chatGPTAuthURL  = "https://auth.openai.com/oauth/authorize"
	chatGPTTokenURL = "https://auth.openai.com/oauth/token"
	chatGPTRedirect = "http://localhost:1455/auth/callback"
)

var chatGPTScopes = []string{"openid", "profile", "email", "offline_access"}

// xAI Grok subscription (Grok CLI public client) OAuth constants. The flow is
// device-code: no redirect listener, the user approves in any browser. The
// token endpoint is discovered via OIDC at login and persisted in the Record;
// xaiTokenURLFallback covers records that predate a successful discovery.
const (
	xaiClientID         = "b1a00492-073a-47ea-816f-4c329264a828"
	xaiIssuer           = "https://auth.x.ai"
	xaiDiscoveryURL     = xaiIssuer + "/.well-known/openid-configuration"
	xaiDeviceAuthURL    = xaiIssuer + "/oauth2/device/code"
	xaiTokenURLFallback = xaiIssuer + "/oauth2/token"
)

var xaiScopes = []string{"openid", "profile", "email", "offline_access", "grok-cli:access", "api:access"}

// Record kinds.
const (
	KindAPIKey = "apikey"
	KindOAuth  = "oauth"
)

// oauthConfig is the oauth2 config for the ChatGPT subscription flow.
func oauthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID:    chatGPTClientID,
		Endpoint:    oauth2.Endpoint{AuthURL: chatGPTAuthURL, TokenURL: chatGPTTokenURL},
		RedirectURL: chatGPTRedirect,
		Scopes:      chatGPTScopes,
	}
}

// xaiOAuthConfig is the oauth2 config for the Grok subscription flow. tokenURL
// comes from the record's discovered endpoint when present; it is re-validated
// here so a tampered auth.json cannot redirect refresh traffic (and the
// refresh token it carries) off the xAI issuer host.
func xaiOAuthConfig(tokenURL string) *oauth2.Config {
	if !xaiEndpointOK(tokenURL) {
		tokenURL = xaiTokenURLFallback
	}
	return &oauth2.Config{
		ClientID: xaiClientID,
		Endpoint: oauth2.Endpoint{DeviceAuthURL: xaiDeviceAuthURL, TokenURL: tokenURL},
		Scopes:   xaiScopes,
	}
}

// oauthConfigFor selects the refresh config for a stored OAuth record.
func oauthConfigFor(provider string, rec Record) *oauth2.Config {
	if provider == llm.XAIProviderID {
		return xaiOAuthConfig(rec.TokenURL)
	}
	return oauthConfig()
}

// IsAuthenticated reports whether a provider can be used without an interactive
// login. The local provider is always usable; OpenAI is usable with a stored
// credential or $OPENAI_API_KEY.
func IsAuthenticated(provider string) bool {
	if provider == llm.LocalProviderID || provider == "" {
		return true
	}
	if provider == llm.OpenAIProviderID && os.Getenv("OPENAI_API_KEY") != "" {
		return true
	}
	if provider == llm.XAIProviderID && os.Getenv("XAI_API_KEY") != "" {
		return true
	}
	_, ok, _ := Get(provider)
	return ok
}

// Describe returns a short human label for a provider's stored credential, or ""
// if none. Secrets are never included.
func Describe(provider string) string {
	if provider == llm.OpenAIProviderID && os.Getenv("OPENAI_API_KEY") != "" {
		if _, ok, _ := Get(provider); !ok {
			return "api key (env)"
		}
	}
	if provider == llm.XAIProviderID && os.Getenv("XAI_API_KEY") != "" {
		// The env key also overrides a stored record for xai (403 escape
		// hatch), so surface it unconditionally.
		return "api key (env)"
	}
	rec, ok, _ := Get(provider)
	if !ok {
		return ""
	}
	switch rec.Kind {
	case KindAPIKey:
		return "api key"
	case KindOAuth:
		if provider == llm.XAIProviderID {
			return "grok subscription"
		}
		if rec.AccountID != "" {
			return "chatgpt (" + rec.AccountID + ")"
		}
		return "chatgpt"
	}
	return rec.Kind
}

// Credential resolves a provider's stored credential into an llm.Credential.
// $OPENAI_API_KEY is honored for the openai provider when no OAuth record is
// stored. An unauthenticated provider yields Kind=none.
func Credential(ctx context.Context, provider string) (llm.Credential, error) {
	return credential(ctx, provider, "")
}

// CredentialForModel resolves credentials with model-aware OpenAI behavior: a
// stored ChatGPT OAuth token is used only for Codex subscription models. If an
// env API key is available and the model is not subscription-supported, route to
// the supported OpenAI API path instead of sending the model to Codex.
func CredentialForModel(ctx context.Context, provider string, modelID string) (llm.Credential, error) {
	return credential(ctx, provider, modelID)
}

func credential(ctx context.Context, provider, modelID string) (llm.Credential, error) {
	rec, ok, err := Get(provider)
	if err != nil {
		return llm.Credential{}, err
	}
	if provider == llm.OpenAIProviderID {
		if key := os.Getenv("OPENAI_API_KEY"); key != "" {
			if !ok || rec.Kind != KindOAuth || (modelID != "" && !llm.IsCodexSubscriptionModel(modelID)) {
				return apiKeyCred(key), nil
			}
		}
	}
	if provider == llm.XAIProviderID {
		// The env key wins even over a stored OAuth record: it is the
		// documented escape hatch when the subscription OAuth surface
		// tier-gates a model with 403, and it must work without a logout.
		// Unset the variable to return to the subscription.
		if key := os.Getenv("XAI_API_KEY"); key != "" {
			return apiKeyCred(key), nil
		}
	}
	if !ok {
		return llm.Credential{Kind: llm.AuthNone}, nil
	}
	switch rec.Kind {
	case KindAPIKey:
		return apiKeyCred(rec.Key), nil
	case KindOAuth:
		oauthKind := llm.AuthOAuthChatGPT
		if provider == llm.XAIProviderID {
			oauthKind = llm.AuthOAuthXAI
		}
		src := sharedSource(ctx, provider, rec)
		return llm.Credential{
			Kind:      oauthKind,
			AccountID: rec.AccountID,
			Token: func(context.Context) (string, error) {
				tok, err := src.Token()
				if err != nil {
					return "", err
				}
				return tok.AccessToken, nil
			},
		}, nil
	}
	return llm.Credential{Kind: llm.AuthNone}, nil
}

func apiKeyCred(key string) llm.Credential {
	return llm.Credential{Kind: llm.AuthAPIKey, Token: func(context.Context) (string, error) { return key, nil }}
}

// OpenModel resolves ref against the registry, fetches the provider's stored
// credential, and builds the backend. It fails clearly when the model's provider
// needs authentication and none is stored. maxTokensFlag is used only when the
// resolved model carries no MaxTokens of its own.
func OpenModel(reg *llm.Registry, ref string, maxTokensFlag int) (llm.Backend, llm.Provider, llm.ModelInfo, error) {
	p, m, err := reg.Resolve(ref)
	if err != nil {
		return nil, p, m, err
	}
	cred, err := CredentialForModel(context.Background(), p.ID, m.ID)
	if err != nil {
		return nil, p, m, err
	}
	if p.NeedsAuth() && cred.Kind == llm.AuthNone {
		return nil, p, m, fmt.Errorf("model %s requires authentication - run: aigem auth login %s", m.Ref(), p.ID)
	}
	maxTokens := m.MaxTokens
	if maxTokens == 0 {
		maxTokens = maxTokensFlag
	}
	b, err := llm.Open(p, m, cred, maxTokens)
	if err != nil {
		return nil, p, m, err
	}
	// The backend may narrow the model - the ChatGPT subscription serves a
	// smaller input window than the same model's API-key path - so report what
	// was actually opened. Callers size the context gauge and compaction from it.
	return b, p, b.Model(), nil
}

// sourceCache holds one token source per provider for the life of the process.
//
// Every bot in the process asks for the same provider's credential, and a source
// per bot means several of them can refresh the same token at once. OpenAI's
// refresh tokens are single-use, so the losers of that race get their token
// rejected. One shared source per provider serializes refreshes behind its own
// mutex and makes the race impossible rather than survivable.
//
// The cache is keyed by provider AND by the stored record, so a login that replaces the
// credential is picked up at once. Keying by provider alone would let a source built from the old
// record keep serving its unexpired access token after the operator logged in again - and
// "log in again and restart me" is advice a bot gives itself, now answered by an in-process
// restart that shares this cache.
var (
	sourceMu    sync.Mutex
	sourceCache = map[string]oauth2.TokenSource{}
)

// sharedSource returns the process-wide token source for a provider, building it on first use.
func sharedSource(ctx context.Context, provider string, rec Record) oauth2.TokenSource {
	key := sourceKey(provider, rec)
	sourceMu.Lock()
	defer sourceMu.Unlock()
	if src, ok := sourceCache[key]; ok {
		return src
	}
	src := refreshingSource(ctx, provider, rec)
	// Drop any source built from an older record for this provider: it is superseded, and keeping
	// it would grow the map by one entry per token rotation.
	for k := range sourceCache {
		if strings.HasPrefix(k, provider+"\x00") {
			delete(sourceCache, k)
		}
	}
	sourceCache[key] = src
	return src
}

// sourceKey identifies the credential a source was built from. The refresh token is the stable
// half of the record - it survives access-token rotation, which the source performs itself - so a
// key built from it changes exactly when the operator replaces the credential.
func sourceKey(provider string, rec Record) string {
	refresh := ""
	if rec.Token != nil {
		refresh = rec.Token.RefreshToken
	}
	sum := sha256.Sum256([]byte(refresh))
	return provider + "\x00" + hex.EncodeToString(sum[:8])
}

// ResetSources drops cached token sources. A login or logout replaces the stored
// record outright, and a source built from the old one would keep presenting a
// credential the user just replaced. The refresh path does not call it: it
// rewrites the record through the very source being cached.
func ResetSources() {
	sourceMu.Lock()
	defer sourceMu.Unlock()
	clear(sourceCache)
}

// refreshingSource returns a token source that refreshes before expiry and
// persists rotated tokens, so an active session never re-prompts.
func refreshingSource(ctx context.Context, provider string, rec Record) oauth2.TokenSource {
	return oauth2.ReuseTokenSource(rec.Token, &persistSource{
		ctx:      context.WithoutCancel(ctx),
		provider: provider,
		rec:      rec,
	})
}

// persistSource refreshes the access token and writes any rotated token back to
// the store. Before each refresh it reloads the record from the shared store:
// OpenAI's refresh tokens are one-time use, and several bot processes share one
// auth.json, so a peer may have already rotated the token; the in-memory copy
// would then be rejected as reused. Reloading uses the peer's fresh token (often
// still valid, so no refresh is needed at all) instead of a stale snapshot.
type persistSource struct {
	ctx      context.Context
	provider string
	mu       sync.Mutex
	rec      Record
	// unsaved marks that the store holds a refresh token this source has already
	// spent, because the write of its replacement failed.
	unsaved bool
}

func (s *persistSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.unsaved {
		// Only while our own copy is the one on disk. After a failed write the file still
		// carries the refresh token the provider has already spent, and reloading it would
		// hand that burned credential to the refresh below.
		if disk, ok, err := Get(s.provider); err == nil && ok && disk.Token != nil &&
			disk.Token.RefreshToken != "" {
			s.rec = disk
		}
	}
	tok, err := oauthConfigFor(s.provider, s.rec).TokenSource(s.ctx, s.rec.Token).Token()
	if err != nil {
		return nil, err
	}
	rotated := s.rec.Token == nil || tok.AccessToken != s.rec.Token.AccessToken ||
		(tok.RefreshToken != "" && tok.RefreshToken != s.rec.Token.RefreshToken)
	if rotated || s.unsaved {
		s.rec.Token = tok
		// The provider has already spent the old refresh token by the time we get here.
		// Failing the call would throw the replacement away and leave every copy we still
		// hold - memory and disk - pointing at a credential the provider will reject, so a
		// single bad write (a full disk) would cost the operator a re-login. Keep the
		// rotated token in memory, where it carries this process until it exits, retry the
		// write at the next refresh, and make the failure loud meanwhile.
		if err := Put(s.provider, s.rec); err != nil {
			s.unsaved = true
			slog.Error("could not persist the rotated OAuth token; it now lives only in this "+
				"process, and a restart will need a fresh login",
				"provider", s.provider, "err", err)
		} else {
			s.unsaved = false
		}
	}
	return tok, nil
}
