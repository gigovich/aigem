package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type shutdownBackend struct {
	*fakeBackend
	closed int
}

func (b *shutdownBackend) CloseBackend() { b.closed++ }

type phaseBackend struct {
	*fakeBackend
	models []Model
	skills Skills
	detail Skill
	login  Login
	acts   []Activity
	ref    string
}

func (b *phaseBackend) Models(context.Context) ([]Model, error) { return b.models, nil }
func (b *phaseBackend) SetDefaultModel(_ context.Context, ref string) (Model, error) {
	b.ref = ref
	return Model{Ref: ref, Default: true}, nil
}
func (b *phaseBackend) BeginLogin(_ context.Context, req LoginRequest) (Login, error) {
	b.login.Provider = req.Provider
	return b.login, nil
}
func (b *phaseBackend) Login(context.Context, string) (Login, error) { return b.login, nil }
func (b *phaseBackend) PasteLogin(_ context.Context, _ string, raw string) (Login, error) {
	b.login.Code = raw
	return b.login, nil
}
func (b *phaseBackend) CancelLogin(context.Context, string) error { return nil }
func (b *phaseBackend) Skills(context.Context) (Skills, error)    { return b.skills, nil }
func (b *phaseBackend) Skill(_ context.Context, name string) (Skill, error) {
	if name != b.detail.Name {
		return Skill{}, ErrNoSkill
	}
	return b.detail, nil
}
func (b *phaseBackend) TrustSkills(context.Context) (SkillApproval, error) {
	return SkillApproval{}, nil
}
func (b *phaseBackend) Commands(context.Context) ([]Command, error) { return nil, nil }
func (b *phaseBackend) Usage(context.Context) ([]ProviderUsage, error) {
	return []ProviderUsage{{Provider: "openai"}}, nil
}
func (b *phaseBackend) Activity(_ context.Context, since uint64, limit int) ([]Activity, error) {
	var out []Activity
	for _, a := range b.acts {
		if a.Seq > since {
			out = append(out, a)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func phaseRequest(t *testing.T, srv *Server, method, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.Base()+strings.TrimPrefix(path, "/"), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token())
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestServerCloseStopsBackendOwnedWorkOnce(t *testing.T) {
	b := &shutdownBackend{fakeBackend: &fakeBackend{}}
	srv, err := New(Config{Backend: b})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if b.closed != 1 {
		t.Fatalf("CloseBackend called %d times, want once", b.closed)
	}
}

func TestPhaseOneCollectionsAreGuardedArraysAndHaveMethodFallbacks(t *testing.T) {
	b := &phaseBackend{fakeBackend: &fakeBackend{}, detail: Skill{SkillSummary: SkillSummary{Name: "ship"}}}
	srv := newTestServer(t, Config{Backend: b})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/models"},
		{http.MethodPost, "/api/models/default"},
		{http.MethodPost, "/api/auth/login"},
		{http.MethodGet, "/api/auth/login/LOGIN-1"},
		{http.MethodGet, "/api/skills"},
		{http.MethodGet, "/api/skills/ship"},
		{http.MethodPost, "/api/skills/trust"},
		{http.MethodGet, "/api/commands"},
		{http.MethodGet, "/api/usage"},
		{http.MethodGet, "/api/activity"},
	} {
		req, err := http.NewRequest(tc.method, srv.Base()+strings.TrimPrefix(tc.path, "/"), strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without auth = %d, want 401", tc.method, tc.path, res.StatusCode)
		}
	}
	for _, path := range []string{"/api/models", "/api/commands", "/api/activity"} {
		res := phaseRequest(t, srv, http.MethodGet, path, "")
		data, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			t.Errorf("GET %s = %d %s, want a non-null collection", path, res.StatusCode, data)
		}
	}
	for _, tc := range []struct{ method, path, allow string }{
		{http.MethodDelete, "/api/models", "GET, HEAD"},
		{http.MethodGet, "/api/models/default", "POST"},
		{http.MethodGet, "/api/auth/login", "POST"},
		{http.MethodPost, "/api/auth/login/LOGIN-1", "GET, HEAD, DELETE"},
		{http.MethodGet, "/api/auth/login/LOGIN-1/paste", "POST"},
		{http.MethodPost, "/api/skills", "GET, HEAD"},
		{http.MethodPost, "/api/commands", "GET, HEAD"},
		{http.MethodPost, "/api/usage", "GET, HEAD"},
		{http.MethodPost, "/api/activity", "GET, HEAD"},
	} {
		res := phaseRequest(t, srv, tc.method, tc.path, "")
		_ = res.Body.Close()
		if res.StatusCode != http.StatusMethodNotAllowed || res.Header.Get("Allow") != tc.allow {
			t.Errorf("%s %s = %d Allow=%q, want 405 %q", tc.method, tc.path,
				res.StatusCode, res.Header.Get("Allow"), tc.allow)
		}
	}
}

func TestLoginPasteAndCancelAreGuardedBoundedAndExact(t *testing.T) {
	b := &phaseBackend{fakeBackend: &fakeBackend{}, login: Login{ID: "LOGIN-1", State: "pending"}}
	srv := newTestServer(t, Config{Backend: b})
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/auth/login/LOGIN-1/paste"},
		{http.MethodDelete, "/api/auth/login/LOGIN-1"},
	} {
		req, _ := http.NewRequest(tc.method, srv.Base()+strings.TrimPrefix(tc.path, "/"), strings.NewReader("code"))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s unguarded: status %d", tc.method, tc.path, res.StatusCode)
		}
	}
	res := phaseRequest(t, srv, http.MethodPost, "/api/auth/login/LOGIN-1/paste", "callback-code")
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || b.login.Code != "callback-code" {
		t.Fatalf("paste = %d, code %q", res.StatusCode, b.login.Code)
	}
	res = phaseRequest(t, srv, http.MethodPost, "/api/auth/login/LOGIN-1/paste", strings.Repeat("x", maxLoginPasteBody+1))
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized paste = %d, want 400", res.StatusCode)
	}
	res = phaseRequest(t, srv, http.MethodDelete, "/api/auth/login/LOGIN-1", "")
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel = %d, want 204", res.StatusCode)
	}
}

func TestPhaseOneMutationsValidateBodiesAndSkillDispatcher(t *testing.T) {
	b := &phaseBackend{fakeBackend: &fakeBackend{}, detail: Skill{SkillSummary: SkillSummary{Name: "ship"}}}
	srv := newTestServer(t, Config{Backend: b})

	res := phaseRequest(t, srv, http.MethodPost, "/api/models/default", `{"ref":"openai/gpt","extra":true}`)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest || b.ref != "" {
		t.Fatalf("unknown model field = %d, backend ref %q", res.StatusCode, b.ref)
	}
	res = phaseRequest(t, srv, http.MethodPost, "/api/models/default", strings.Repeat(" ", maxRunBody)+`{"ref":"x"}`)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized model body = %d, want 400", res.StatusCode)
	}
	res = phaseRequest(t, srv, http.MethodPost, "/api/models/default", `{"ref":"openai/gpt"}`)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || b.ref != "openai/gpt" {
		t.Fatalf("valid default = %d, backend ref %q", res.StatusCode, b.ref)
	}
	b.login = Login{ID: "LOGIN-1", State: "pending"}
	res = phaseRequest(t, srv, http.MethodPost, "/api/auth/login", `{"provider":"openai"}`)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusCreated || b.login.Provider != "openai" {
		t.Fatalf("login start = %d, provider %q", res.StatusCode, b.login.Provider)
	}
	res = phaseRequest(t, srv, http.MethodPost, "/api/skills/trust", "")
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST skill trust = %d", res.StatusCode)
	}
	res = phaseRequest(t, srv, http.MethodGet, "/api/skills/trust", "")
	_ = res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed || res.Header.Get("Allow") != "POST" {
		t.Fatalf("GET skill trust = %d Allow=%q", res.StatusCode, res.Header.Get("Allow"))
	}
	res = phaseRequest(t, srv, http.MethodPost, "/api/skills/ship", `{}`)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed || res.Header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST skill detail = %d Allow=%q", res.StatusCode, res.Header.Get("Allow"))
	}
}

func TestActivityValidatesAndAppliesItsCursor(t *testing.T) {
	b := &phaseBackend{fakeBackend: &fakeBackend{}, acts: []Activity{
		{Seq: 1, At: time.Unix(1, 0), Kind: "one", Text: "one"},
		{Seq: 2, At: time.Unix(2, 0), Kind: "two", Text: "two"},
		{Seq: 3, At: time.Unix(3, 0), Kind: "three", Text: "three"},
	}}
	srv := newTestServer(t, Config{Backend: b})
	res := phaseRequest(t, srv, http.MethodGet, "/api/activity?since=1&limit=1", "")
	defer res.Body.Close()
	var got []Activity
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("activity = %+v, want only seq 2", got)
	}
	for _, query := range []string{"since=-1", "since=1.2", "limit=-1"} {
		res := phaseRequest(t, srv, http.MethodGet, "/api/activity?"+query, "")
		_ = res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", query, res.StatusCode)
		}
	}
}

func TestLoginWireContainsNoHiddenCompletionField(t *testing.T) {
	b := &phaseBackend{fakeBackend: &fakeBackend{}, login: Login{
		ID: "LOGIN-1", Provider: "openai", URL: "https://example.test/authorize",
		State: "done",
	}}
	srv := newTestServer(t, Config{Backend: b})
	res := phaseRequest(t, srv, http.MethodGet, "/api/auth/login/LOGIN-1", "")
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if bytes.Contains(data, []byte("completedNow")) {
		t.Fatalf("internal transition leaked onto wire: %s", data)
	}
}
