package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
)

type setupBackend struct {
	*phaseBackend
	provider, url, removed string
	added                  NewModels
}

func (b *setupBackend) Providers(context.Context) (Providers, error) {
	return Providers{Providers: []Provider{{ID: "openai", NeedsAuth: true}}}, nil
}
func (b *setupBackend) ProviderModels(_ context.Context, provider, url string) ([]Model, error) {
	b.provider, b.url = provider, url
	if url == "http://bad" {
		return nil, Refuse(errors.New("no server there"))
	}
	return []Model{{Ref: "m"}}, nil
}
func (b *setupBackend) AddModels(_ context.Context, req NewModels) ([]Model, error) {
	b.added = req
	return req.Models, nil
}
func (b *setupBackend) RemoveModel(_ context.Context, ref string) error {
	b.removed = ref
	return nil
}

func TestModelSetupRoutes(t *testing.T) {
	b := &setupBackend{phaseBackend: &phaseBackend{fakeBackend: &fakeBackend{}}}
	srv := newTestServer(t, Config{Backend: b})
	if _, meta := getMeta(t, srv); !meta.Features["modelSetup"] {
		t.Fatal("the feature map does not name modelSetup")
	}
	for _, tc := range []struct {
		method, path, body string
		status             int
		want               string
	}{
		{http.MethodGet, "/api/providers", "", http.StatusOK, `{"providers":[{"id":"openai","needsAuth":true,` +
			`"authenticated":false,"canLogin":false}],"local":[]}`},
		{http.MethodGet, "/api/providers/models", "", http.StatusBadRequest, ""},
		{http.MethodGet, "/api/providers/models?provider=openai&url=http://x", "", http.StatusBadRequest, ""},
		{http.MethodGet, "/api/providers/models?url=http://bad", "", http.StatusBadRequest, ""},
		{http.MethodGet, "/api/providers/models?provider=openai", "", http.StatusOK, ""},
		{http.MethodPost, "/api/models", `{"provider":"openai"}`, http.StatusBadRequest, ""},
		{http.MethodPost, "/api/models", `{"models":[{"ref":"m"}]}`, http.StatusBadRequest, ""},
		{http.MethodPost, "/api/models", `{"provider":"p","baseUrl":"http://x","models":[{"ref":"m"}]}`,
			http.StatusBadRequest, ""},
		{http.MethodPost, "/api/models", `{"baseUrl":"http://x","models":[{"ref":"m","contextWindow":10}]}`,
			http.StatusOK, ""},
		{http.MethodDelete, "/api/models", "", http.StatusBadRequest, ""},
		{http.MethodDelete, "/api/models?ref=openai/m", "", http.StatusNoContent, ""},
	} {
		res := phaseRequest(t, srv, tc.method, tc.path, tc.body)
		data, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != tc.status {
			t.Errorf("%s %s %s = %d %s, want %d", tc.method, tc.path, tc.body, res.StatusCode, data, tc.status)
		}
		if tc.want != "" && string(data) != tc.want+"\n" {
			t.Errorf("%s %s = %s, want %s", tc.method, tc.path, data, tc.want)
		}
	}
	if b.provider != "openai" || b.url != "" {
		t.Errorf("ProviderModels got provider=%q url=%q", b.provider, b.url)
	}
	if b.added.BaseURL != "http://x" || len(b.added.Models) != 1 || b.added.Models[0].ContextWindow != 10 {
		t.Errorf("AddModels got %+v", b.added)
	}
	if b.removed != "openai/m" {
		t.Errorf("RemoveModel got %q", b.removed)
	}
	var models []Model
	res := phaseRequest(t, srv, http.MethodGet, "/api/providers/models?url=http://ok", "")
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(&models); err != nil || len(models) != 1 || b.url != "http://ok" {
		t.Fatalf("models by url = %+v, %v, url %q", models, err, b.url)
	}
}
