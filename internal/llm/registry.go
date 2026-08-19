package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/gigovich/aigem/internal/config"
)

// Provider transports for a family of models. The API field discriminates the
// transport; Auth says how requests are authenticated.
type Provider struct {
	ID      string            `json:"id"`
	BaseURL string            `json:"base_url"`
	API     string            `json:"api"`  // openai-completions | openai-responses
	Auth    string            `json:"auth"` // none | apikey | oauth-chatgpt | openai | oauth-xai | xai
	Headers map[string]string `json:"headers,omitempty"`
	Models  []ModelInfo       `json:"models"`
}

// API identifiers.
const (
	APICompletions = "openai-completions"
	APIResponses   = "openai-responses"
)

// Auth identifiers. "openai" is the dual path: it accepts either a stored API
// key (chat-completions) or a ChatGPT OAuth token (Responses/Codex). "xai" is
// the analogous dual path for xAI: an API key or a Grok subscription OAuth
// token - both are plain bearers on the same OpenAI-compatible completions API.
const (
	AuthNone         = "none"
	AuthAPIKey       = "apikey"
	AuthOAuthChatGPT = "oauth-chatgpt"
	AuthOpenAI       = "openai"
	AuthOAuthXAI     = "oauth-xai"
	AuthXAI          = "xai"
)

// LocalProviderID, OpenAIProviderID and XAIProviderID are the built-in provider ids.
const (
	LocalProviderID  = "local"
	OpenAIProviderID = "openai"
	XAIProviderID    = "xai"
)

// CodexResponsesURL is the undocumented ChatGPT subscription backend (Codex),
// used only with an OAuth credential. The API-key path uses api.openai.com.
const CodexResponsesURL = "https://chatgpt.com/backend-api"

var codexSubscriptionModels = map[string]bool{
	"gpt-5.6-sol":   true,
	"gpt-5.6-terra": true,
	"gpt-5.6-luna":  true,
}

// IsCodexSubscriptionModel reports whether the ChatGPT subscription/Codex
// backend is expected to accept this model. Other OpenAI models need an API key.
func IsCodexSubscriptionModel(id string) bool { return codexSubscriptionModels[id] }

// codexSubscriptionContext holds the input window reported by the ChatGPT
// subscription model registry when it is smaller than the API-key window. A
// session sized from the API preset would otherwise compact too late and fail
// the turn on context_length_exceeded rather than shrinking in time.
var codexSubscriptionContext = map[string]int{
	"gpt-5.6-sol":  272000,
	"gpt-5.6-luna": 272000,
}

// SubscriptionContextWindow returns the ChatGPT subscription input window for a
// model, or 0 when the subscription serves the model's full context window.
func SubscriptionContextWindow(id string) int { return codexSubscriptionContext[id] }

// applySubscriptionContext narrows a model's context window to what the
// subscription actually serves. It only ever lowers the value, so a deliberately
// smaller models.json window is kept.
func applySubscriptionContext(m ModelInfo) ModelInfo {
	cw := codexSubscriptionContext[m.ID]
	if cw > 0 && (m.ContextWindow == 0 || cw < m.ContextWindow) {
		m.ContextWindow = cw
	}
	return m
}

// CodexSubscriptionModels returns the currently known subscription allow-list.
func CodexSubscriptionModels() []string {
	ids := make([]string, 0, len(codexSubscriptionModels))
	for id := range codexSubscriptionModels {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// errUnsupportedSubscriptionModel reports a model the ChatGPT subscription
// backend will not serve, listing the ones it will.
func errUnsupportedSubscriptionModel(m ModelInfo) error {
	return fmt.Errorf("model %s is not supported by ChatGPT subscription; supported: %s; "+
		"use an OpenAI API key for other models", m.Ref(), strings.Join(CodexSubscriptionModels(), ", "))
}

// Credential is the resolved authentication for one provider, supplied by the
// caller (internal/auth) from the on-disk store or environment.
type Credential struct {
	// Kind holds an llm.Auth* value (AuthNone | AuthAPIKey | AuthOAuthChatGPT |
	// AuthOAuthXAI) - the transport selector, distinct from auth.Record's
	// storage-format Kind.
	Kind      string
	Token     func(context.Context) (string, error) // bearer token; nil for none
	AccountID string                                // chatgpt-account-id (subscription path)
}

// Registry resolves provider/model references from presets overlaid by user and
// project models.json files.
type Registry struct {
	providers []Provider // ordered for stable listing
}

// openAIPresets is the compiled-in curated OpenAI model list (decision: presets
// shipped in the binary, overridable via models.json). The "openai" provider's
// Auth is dual: an API key uses chat-completions, a ChatGPT OAuth token uses the
// Codex Responses backend for the allow-listed ChatGPT subscription models; the
// API key can use these plus others added via models.json.
func openAIPresets() Provider {
	return Provider{
		ID:      OpenAIProviderID,
		BaseURL: "https://api.openai.com",
		API:     APICompletions,
		Auth:    AuthOpenAI,
		Models: []ModelInfo{
			{ID: "gpt-5.6-sol", Name: "GPT-5.6 Sol", ContextWindow: 1050000, MaxTokens: 128000, Reasoning: true},
			{ID: "gpt-5.6-terra", Name: "GPT-5.6 Terra", ContextWindow: 1050000, MaxTokens: 128000, Reasoning: true},
			{ID: "gpt-5.6-luna", Name: "GPT-5.6 Luna", ContextWindow: 1050000, MaxTokens: 128000, Reasoning: true},
		},
	}
}

// xaiPresets is the compiled-in curated xAI model list. The provider is dual
// like "openai": a Grok subscription OAuth token (SuperGrok / X Premium+, the
// Grok CLI device-code flow) or an API key - both are bearers on the same
// OpenAI-compatible completions endpoint. The model ids mirror the set the
// subscription OAuth surface is known to serve; the API-key path also accepts
// other xAI models added via models.json.
func xaiPresets() Provider {
	return Provider{
		ID:      XAIProviderID,
		BaseURL: "https://api.x.ai",
		API:     APICompletions,
		Auth:    AuthXAI,
		// MaxTokens is left 0 (callers fall back to their flag default): a
		// preset cap above a model's true completion limit would 400 every
		// request. Both limits can be corrected per model via models.json.
		// grok-4.3 leads: it is the default model unattended bots pick, and in
		// live use grok-build-0.1 mishandled the reply protocol (looped on
		// post_message with invented channel names until the repeated-call
		// guard stopped the turn), while 4.3 follows it.
		Models: []ModelInfo{
			{ID: "grok-4.3", Name: "Grok 4.3", ContextWindow: 262144, Reasoning: true},
			{ID: "grok-build-0.1", Name: "Grok Build 0.1", ContextWindow: 262144, Reasoning: true},
			{ID: "grok-4.20-0309-reasoning", Name: "Grok 4.20 Reasoning", ContextWindow: 262144, Reasoning: true},
			{ID: "grok-4.1-fast", Name: "Grok 4.1 Fast", ContextWindow: 262144},
		},
	}
}

// NewRegistry builds the registry: the given local provider (from --url /
// --model / --ctx-size) plus compiled OpenAI and xAI presets, overlaid by the
// user and project models.json files. It returns any non-fatal file warnings.
func NewRegistry(cwd string, local Provider) (*Registry, []string) {
	return newRegistry(local, config.ModelsFiles(cwd), config.ProjectModelsFile(cwd))
}

// NewUserRegistry builds the registry from the compiled presets and the user's
// own models.json only. It is for decisions that outlive the current directory -
// pinning a bot's model, which that bot opens later from its own cwd - so the
// result cannot depend on which repo the command happened to run in, and a
// project-local file can never be the source of a pinned provider.
func NewUserRegistry(local Provider) (*Registry, []string) {
	return newRegistry(local, config.UserModelsFiles(), "")
}

func newRegistry(local Provider, files []string, projectFile string) (*Registry, []string) {
	r := &Registry{}
	r.upsert(local, true)
	r.upsert(openAIPresets(), true)
	r.upsert(xaiPresets(), true)

	var warns []string
	// Apply user first, then project, so project wins (highest precedence applied
	// last). config.ModelsFiles returns project-first, so iterate in reverse. The
	// project file is untrusted (it ships with a possibly-cloned repo): it may add
	// or tweak models but must not redirect a built-in provider's endpoint/auth,
	// which would attach the user's credential to an attacker-chosen host.
	for i := len(files) - 1; i >= 0; i-- {
		provs, err := loadModelFile(files[i])
		if err != nil {
			warns = append(warns, fmt.Sprintf("%s: %v", files[i], err))
			continue
		}
		trusted := files[i] != projectFile
		for _, p := range provs {
			r.upsert(p, trusted)
		}
	}
	return r, warns
}

// LocalProvider builds the local llama.cpp provider from runtime flags.
func LocalProvider(baseURL, model string, ctxWindow, maxTokens int) Provider {
	return Provider{
		ID:      LocalProviderID,
		BaseURL: baseURL,
		API:     APICompletions,
		Auth:    AuthNone,
		Models: []ModelInfo{
			{ID: model, Name: model, ContextWindow: ctxWindow, MaxTokens: maxTokens},
		},
	}
}

// upsert merges p into the registry: an existing provider keeps its slot but has
// non-empty scalar fields overridden and models upserted by id; a new provider
// is appended. Callers apply lowest precedence first. When trusted is false (a
// project-local file), an already-known provider's endpoint/auth/headers are left
// intact - only its models may be added or tweaked - so an untrusted repo cannot
// redirect a credential the user stored for that provider to another host. That
// holds for every provider the user configured, not just the compiled-in ones:
// a credential is keyed by provider id, so any provider that already exists here
// may have one.
func (r *Registry) upsert(p Provider, trusted bool) {
	for i := range r.providers {
		if r.providers[i].ID != p.ID {
			continue
		}
		ex := &r.providers[i]
		if trusted {
			if p.BaseURL != "" {
				ex.BaseURL = p.BaseURL
			}
			if p.API != "" {
				ex.API = p.API
			}
			if p.Auth != "" {
				ex.Auth = p.Auth
			}
			if len(p.Headers) > 0 {
				ex.Headers = p.Headers
			}
		}
		for _, m := range p.Models {
			ex.upsertModel(m)
		}
		return
	}
	r.providers = append(r.providers, p)
}

func (p *Provider) upsertModel(m ModelInfo) {
	for i := range p.Models {
		if p.Models[i].ID == m.ID {
			p.Models[i] = mergeModelInfo(p.Models[i], m)
			return
		}
	}
	p.Models = append(p.Models, m)
}

func mergeModelInfo(base, override ModelInfo) ModelInfo {
	if override.Provider != "" {
		base.Provider = override.Provider
	}
	if override.ID != "" {
		base.ID = override.ID
	}
	if override.Name != "" {
		base.Name = override.Name
	}
	if override.ContextWindow != 0 {
		base.ContextWindow = override.ContextWindow
	}
	if override.MaxTokens != 0 {
		base.MaxTokens = override.MaxTokens
	}
	if override.Reasoning {
		base.Reasoning = true
	}
	if override.Temperature != nil {
		base.Temperature = override.Temperature
	}
	return base
}

// Providers returns the resolved providers in load order.
func (r *Registry) Providers() []Provider { return r.providers }

// Provider returns one resolved provider by id.
func (r *Registry) Provider(id string) (Provider, bool) { return r.provider(id) }

// ReplaceLocal swaps the local provider for p (used after a mid-session
// local-model init changes its URL/model). If no local provider exists yet, p is
// prepended so the local model stays first in listings and Default().
func (r *Registry) ReplaceLocal(p Provider) {
	for i := range r.providers {
		if r.providers[i].ID == LocalProviderID {
			r.providers[i] = p
			return
		}
	}
	r.providers = append([]Provider{p}, r.providers...)
}

// Models returns every model with its Provider field populated, provider order
// preserved.
func (r *Registry) Models() []ModelInfo {
	var out []ModelInfo
	for _, p := range r.providers {
		for _, m := range p.Models {
			m.Provider = p.ID
			out = append(out, m)
		}
	}
	return out
}

// Default returns the model used when no --model is given: the local model when
// present, else the first listed model.
func (r *Registry) Default() (ModelInfo, bool) {
	for _, p := range r.providers {
		if p.ID == LocalProviderID && len(p.Models) > 0 {
			m := p.Models[0]
			m.Provider = p.ID
			return m, true
		}
	}
	if models := r.Models(); len(models) > 0 {
		return models[0], true
	}
	return ModelInfo{}, false
}

// DefaultPreferring returns the startup model when no --model is given: the
// first model of the first authenticated provider that requires auth (so an
// authenticated OpenAI is preferred), else the plain Default (local).
func (r *Registry) DefaultPreferring(authed func(provider string) bool) (ModelInfo, bool) {
	if authed != nil {
		for _, p := range r.providers {
			if p.NeedsAuth() && len(p.Models) > 0 && authed(p.ID) {
				m := p.Models[0]
				m.Provider = p.ID
				return m, true
			}
		}
	}
	return r.Default()
}

// Resolve accepts "provider/id", a bare "id" (searched across providers), or ""
// (the default model).
func (r *Registry) Resolve(ref string) (Provider, ModelInfo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		m, ok := r.Default()
		if !ok {
			return Provider{}, ModelInfo{}, fmt.Errorf("no models configured")
		}
		p, _ := r.provider(m.Provider)
		return p, m, nil
	}
	if prov, id, ok := strings.Cut(ref, "/"); ok {
		p, found := r.provider(prov)
		if !found {
			return Provider{}, ModelInfo{}, fmt.Errorf("unknown provider %q", prov)
		}
		for _, m := range p.Models {
			if m.ID == id {
				m.Provider = p.ID
				return p, m, nil
			}
		}
		return Provider{}, ModelInfo{}, fmt.Errorf("provider %q has no model %q", prov, id)
	}
	// Bare id: search every provider.
	for _, p := range r.providers {
		for _, m := range p.Models {
			if m.ID == ref {
				m.Provider = p.ID
				return p, m, nil
			}
		}
	}
	return Provider{}, ModelInfo{}, fmt.Errorf("unknown model %q", ref)
}

func (r *Registry) provider(id string) (Provider, bool) {
	for _, p := range r.providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

// NeedsAuth reports whether a provider requires a credential to be used.
func (p Provider) NeedsAuth() bool { return p.Auth != "" && p.Auth != AuthNone }

// Open builds the Backend for the given provider/model and credential. It
// validates the provider API and auth mode, then opens chat-completions or the
// Responses adapter. The built-in dual OpenAI provider keys off credential kind:
// API key uses completions; ChatGPT OAuth uses the Codex Responses backend.
func Open(p Provider, m ModelInfo, cred Credential, maxTokens int) (Backend, error) {
	m.Provider = p.ID
	api := p.API
	if api == "" {
		api = APICompletions
	}
	if api != APICompletions && api != APIResponses {
		return nil, fmt.Errorf("provider %q has unknown api %q", p.ID, p.API)
	}

	switch p.Auth {
	case AuthNone, "":
		if api != APICompletions {
			return nil, fmt.Errorf("provider %q api %q requires authentication", p.ID, api)
		}
		return openCompletions(p, m, nil, maxTokens), nil

	case AuthAPIKey:
		if cred.Kind != AuthAPIKey || cred.Token == nil {
			return nil, fmt.Errorf("provider %q needs an API key (run: aigem auth login %s)", p.ID, p.ID)
		}
		if api != APICompletions {
			return nil, fmt.Errorf("provider %q api %q is not supported with API-key auth", p.ID, api)
		}
		return openCompletions(p, m, cred.Token, maxTokens), nil

	case AuthOAuthChatGPT:
		if api != APIResponses {
			return nil, fmt.Errorf("provider %q auth %q requires api %q", p.ID, p.Auth, APIResponses)
		}
		return openResponses(p, m, cred)

	case AuthOpenAI:
		switch cred.Kind {
		case AuthOAuthChatGPT:
			// openResponses enforces the subscription model allow-list.
			return openResponses(p, m, cred)
		case AuthAPIKey:
			if api != APICompletions {
				return nil, fmt.Errorf("provider %q api %q is not supported with API-key auth", p.ID, api)
			}
			return openCompletions(p, m, cred.Token, maxTokens), nil
		default:
			return nil, fmt.Errorf("provider %q is not authenticated (run: aigem auth login %s)", p.ID, p.ID)
		}

	case AuthXAI:
		// Both credential kinds are plain bearers on the OpenAI-compatible
		// completions API; only the token's origin (key vs OAuth) differs.
		switch cred.Kind {
		case AuthOAuthXAI, AuthAPIKey:
			if api != APICompletions {
				return nil, fmt.Errorf("provider %q api %q is not supported with auth %q", p.ID, api, p.Auth)
			}
			return openCompletions(p, m, cred.Token, maxTokens), nil
		default:
			return nil, fmt.Errorf("provider %q is not authenticated (run: aigem auth login %s)", p.ID, p.ID)
		}
	}
	return nil, fmt.Errorf("provider %q has unknown auth %q", p.ID, p.Auth)
}

func openCompletions(p Provider, m ModelInfo, token func(context.Context) (string, error), maxTokens int) Backend {
	return NewClient(ClientConfig{
		BaseURL:     p.BaseURL,
		Info:        m,
		Auth:        token,
		Headers:     p.Headers,
		MaxTokens:   maxTokens,
		TokenizeURL: tokenizeURLFor(p),
	})
}

// openResponses builds the subscription Responses adapter, requiring an OAuth
// credential. The Codex backend manages output length itself, so maxTokens does
// not apply here.
func openResponses(p Provider, m ModelInfo, cred Credential) (Backend, error) {
	if cred.Kind != AuthOAuthChatGPT || cred.Token == nil {
		return nil, fmt.Errorf("provider %q needs ChatGPT login (run: aigem auth login %s)", p.ID, p.ID)
	}
	if p.ID == OpenAIProviderID && !IsCodexSubscriptionModel(m.ID) {
		return nil, errUnsupportedSubscriptionModel(m)
	}
	base := p.BaseURL
	if p.Auth == AuthOpenAI || base == "" {
		base = CodexResponsesURL
	}
	return NewResponsesClient(ResponsesConfig{
		BaseURL:   base,
		Info:      applySubscriptionContext(m),
		Token:     cred.Token,
		AccountID: cred.AccountID,
	}), nil
}

// tokenizeURLFor returns the /tokenize endpoint for a local llama provider, else
// "" (chars/4 estimate).
func tokenizeURLFor(p Provider) string {
	if p.ID == LocalProviderID {
		return strings.TrimRight(p.BaseURL, "/") + "/tokenize"
	}
	return ""
}

// loadModelFile parses a models.json file into providers. It accepts either
// {"providers":[...]} or a bare [...] array.
func loadModelFile(path string) ([]Provider, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 {
		return nil, nil
	}
	if data[0] == '[' {
		var provs []Provider
		if err := json.Unmarshal(data, &provs); err != nil {
			return nil, err
		}
		return provs, nil
	}
	var doc struct {
		Providers []Provider `json:"providers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return doc.Providers, nil
}

// SortModelsByRef sorts models by "provider/id" for stable display.
func SortModelsByRef(models []ModelInfo) {
	sort.Slice(models, func(i, j int) bool { return models[i].Ref() < models[j].Ref() })
}
