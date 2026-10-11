package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/web"
)

var _ web.ModelSetupBackend = (*webBackend)(nil)

func webModel(m llm.ModelInfo) web.Model {
	return web.Model{
		Ref: m.Ref(), Provider: m.Provider, Name: m.Name,
		ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens, Reasoning: m.Reasoning,
	}
}

func webModels(models []llm.ModelInfo) []web.Model {
	out := make([]web.Model, 0, len(models))
	for _, m := range models {
		out = append(out, webModel(m))
	}
	return out
}

func (b *webBackend) Providers(ctx context.Context) (web.Providers, error) {
	var out web.Providers
	models := b.models.Load()
	for _, p := range models.AddableProviders() {
		out.Providers = append(out.Providers, web.Provider{
			ID: p.ID, NeedsAuth: p.NeedsAuth(), Authenticated: !p.NeedsAuth() || auth.IsAuthenticated(p.ID),
			CanLogin: p.ID == llm.OpenAIProviderID || p.ID == llm.XAIProviderID,
		})
	}
	for _, s := range models.DiscoverServers(ctx) {
		out.Local = append(out.Local, web.LocalServer{Name: s.Name, BaseURL: s.BaseURL, Models: webModels(s.Models)})
	}
	return out, nil
}

// ProviderModels lists what a configured provider serves to its credential, or
// what the server at baseURL serves without one: a typed URL is never sent a
// stored key.
func (b *webBackend) ProviderModels(ctx context.Context, provider, baseURL string) ([]web.Model, error) {
	reg := b.models.Load()
	var models []llm.ModelInfo
	var err error
	if provider != "" {
		models, err = auth.ProviderModels(ctx, reg, provider)
	} else if baseURL, err = serverURL(baseURL); err == nil {
		models, err = llm.ListModels(ctx, llm.Provider{BaseURL: baseURL}, nil)
		provider = reg.ServerAddition(baseURL).ProviderID
	}
	if err != nil {
		return nil, web.Refuse(err)
	}
	return webModels(reg.NotAdded(provider, models)), nil
}

// serverURL is the base of the server at a typed URL, which is often given with
// the /v1 every request path adds.
func serverURL(typed string) (string, error) {
	base := strings.TrimSuffix(strings.TrimRight(typed, "/"), "/v1")
	return base, llm.CheckBaseURL(base)
}

func (b *webBackend) AddModels(_ context.Context, req web.NewModels) ([]web.Model, error) {
	b.modelsMu.Lock()
	defer b.modelsMu.Unlock()
	models := b.models.Load()
	add := llm.ModelAddition{ProviderID: req.Provider}
	if req.BaseURL != "" {
		base, err := serverURL(req.BaseURL)
		if err != nil {
			return nil, web.Refuse(err)
		}
		add = models.ServerAddition(base)
	}
	for _, m := range req.Models {
		if m.ContextWindow <= 0 {
			return nil, web.Refuse(fmt.Errorf("give the context window of %s", m.Ref))
		}
		id := m.Ref
		if req.Provider != "" {
			id = strings.TrimPrefix(id, req.Provider+"/")
		}
		add.Models = append(add.Models, llm.ModelInfo{
			Provider: add.ProviderID, ID: id, Name: m.Name,
			ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens, Reasoning: m.Reasoning,
		})
	}
	fresh, err := models.AddModel(add)
	if err != nil {
		return nil, web.Refuse(err)
	}
	b.setModels(fresh, "model.added", fmt.Sprintf("Added %d model(s) from %s", len(add.Models), add.ProviderID))
	return webModels(add.Models), nil
}

func (b *webBackend) RemoveModel(_ context.Context, ref string) error {
	b.modelsMu.Lock()
	defer b.modelsMu.Unlock()
	fresh, err := b.models.Load().RemoveModel(ref)
	if err != nil {
		return web.Refuse(err)
	}
	b.setModels(fresh, "model.removed", "Removed "+ref)
	return nil
}

func (b *webBackend) setModels(models *llm.Registry, kind, text string) {
	b.models.Store(models)
	if b.runs != nil {
		b.runs.SetModels(models)
	}
	b.recordActivity(web.Activity{Kind: kind, Text: text})
	b.publish("models.updated", nil)
	slog.Info(text)
}
