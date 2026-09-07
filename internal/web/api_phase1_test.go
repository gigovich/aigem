package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// err is what Commands answers with, so one route can be pointed at each
	// arm of the error mapping without a fake per arm.
	err error
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
func (b *phaseBackend) Commands(context.Context) ([]Command, error) { return nil, b.err }
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

// Login is a hand-built view rather than a marshalled auth.Flow, and the reason
// is that a flow holds a token. A field added to the view without being thought
// about is how that stops being true, so the wire's key set is pinned: a new
// key has to be added here too, which is where somebody asks what it carries.
func TestTheLoginWireCarriesOnlyTheKeysItIsMeantTo(t *testing.T) {
	b := &phaseBackend{fakeBackend: &fakeBackend{}, login: Login{
		ID: "LOGIN-1", Provider: "openai", URL: "https://example.test/authorize",
		Code: "ABCD-1234", AcceptsPaste: true, State: "failed", Error: "provider login failed",
	}}
	srv := newTestServer(t, Config{Backend: b})
	res := phaseRequest(t, srv, http.MethodGet, "/api/auth/login/LOGIN-1", "")
	defer res.Body.Close()
	var got map[string]json.RawMessage
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"id": true, "provider": true, "url": true, "code": true,
		"acceptsPaste": true, "state": true, "error": true,
	}
	for key := range got {
		if !want[key] {
			t.Errorf("the login wire carries %q, which nothing asked for", key)
		}
	}
	for key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("the login wire lost %q", key)
		}
	}
}

// Every failure a phase-one route can produce, and the status each one is meant
// to become. Without this the whole mapping can be deleted and the suite stays
// green: a page would be told "the daemon could not carry that out" for an
// unknown login, for a name it mistyped, and for a turn it only had to wait for.
func TestEveryPhaseOneFailureBecomesTheStatusItMeans(t *testing.T) {
	b := &phaseBackend{fakeBackend: &fakeBackend{}}
	srv := newTestServer(t, Config{Backend: b})
	for _, tc := range []struct {
		name   string
		err    error
		status int
		body   string
		retry  string
	}{
		{"an unknown login", ErrNoLogin, http.StatusNotFound, "no such login", ""},
		{"an unknown skill", ErrNoSkill, http.StatusNotFound, "no such skill", ""},
		{"a busy daemon", ErrBusy, http.StatusServiceUnavailable, "", "5"},
		{"a refusal", Refuse(errors.New("that project has no skills")),
			http.StatusBadRequest, "that project has no skills", ""},
		{"the daemon's own fault", errNope, http.StatusInternalServerError,
			"the daemon could not carry that out", ""},
	} {
		b.err = tc.err
		res := phaseRequest(t, srv, http.MethodGet, "/api/commands", "")
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != tc.status {
			t.Errorf("%s = %d, want %d", tc.name, res.StatusCode, tc.status)
		}
		if tc.body != "" && !strings.Contains(string(body), tc.body) {
			t.Errorf("%s said %q, want it to name %q", tc.name, body, tc.body)
		}
		if tc.retry != "" && res.Header.Get("Retry-After") != tc.retry {
			t.Errorf("%s Retry-After = %q, want %q", tc.name, res.Header.Get("Retry-After"), tc.retry)
		}
		if errors.Is(tc.err, errNope) && strings.Contains(string(body), errNope.Error()) {
			t.Errorf("the daemon's own error text reached the client: %q", body)
		}
	}
}

// The feature map is how a page decides which screens exist. Each flag has to
// answer for its own seam: a map that lost them all would hide every phase-one
// screen, and nothing else in the daemon would notice.
func TestTheFeatureMapNamesEverySeamTheBackendImplements(t *testing.T) {
	srv := newTestServer(t, Config{Backend: &phaseBackend{fakeBackend: &fakeBackend{}}})
	_, body := getMeta(t, srv)
	want := []string{
		"activity", "commands", "controlSocket", "models", "providerLogin", "runs", "skills", "usage",
	}
	for _, name := range want {
		if !body.Features[name] {
			t.Errorf("the feature map does not name %q, so a page hides that screen", name)
		}
	}
	if len(body.Features) != len(want) {
		t.Errorf("features = %v, want exactly %v", body.Features, want)
	}
}

// The wire never carries null where a collection belongs: a page that iterates
// what it was given must not have to test every field for it first.
func TestEveryPhaseOneCollectionIsAnArrayWhenItIsEmpty(t *testing.T) {
	b := &phaseBackend{
		fakeBackend: &fakeBackend{},
		skills:      Skills{Pending: &PendingSkills{}},
		detail:      Skill{SkillSummary: SkillSummary{Name: "ship"}},
	}
	srv := newTestServer(t, Config{Backend: b})
	for _, tc := range []struct {
		method, path string
		fields       []string
	}{
		{http.MethodGet, "/api/models", nil},
		{http.MethodGet, "/api/commands", nil},
		{http.MethodGet, "/api/activity", nil},
		{http.MethodGet, "/api/usage", nil},
		{http.MethodGet, "/api/skills", []string{"items", "pending.names"}},
		{http.MethodGet, "/api/skills/ship", []string{"allowedTools", "disallowedTools", "paths"}},
		{http.MethodPost, "/api/skills/trust", []string{"loaded", "notices"}},
	} {
		res := phaseRequest(t, srv, tc.method, tc.path, "")
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d: %s", tc.path, res.StatusCode, body)
		}
		if bytes.Contains(body, []byte("null")) {
			t.Errorf("%s answered with a null collection: %s", tc.path, body)
		}
		if tc.fields == nil && !bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) {
			t.Errorf("%s = %s, want an array", tc.path, body)
		}
	}
}

// Usage exists to show quota, so the windows are the answer. Dropping them
// would leave every provider listed with nothing under it, which reads as "no
// limits" rather than as a daemon that lost them.
func TestUsageCarriesEveryWindowInAStableOrder(t *testing.T) {
	b := &phaseBackend{fakeBackend: &fakeBackend{}}
	srv := newTestServer(t, Config{Backend: b})
	res := phaseRequest(t, srv, http.MethodGet, "/api/usage", "")
	defer res.Body.Close()
	var got []ProviderUsage
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Provider != "openai" {
		t.Fatalf("usage = %+v, want the one provider the backend reported", got)
	}
	if got[0].Windows == nil {
		t.Error("a provider with no snapshot lost its windows array")
	}
}

// A daemon whose backend implements a seam but was handed nothing to work with
// is a daemon that cannot serve that route, and says so the same way one built
// without the seam does. One rule, decided in one place: the feature map and
// the routes cannot drift apart, because they are the same list.
type withdrawnBackend struct{ *phaseBackend }

func (b withdrawnBackend) Unavailable() []string { return []string{"activity", "skills"} }

func TestAWithdrawnFeatureIsRefusedByItsRoutesToo(t *testing.T) {
	b := withdrawnBackend{phaseBackend: &phaseBackend{
		fakeBackend: &fakeBackend{}, detail: Skill{SkillSummary: SkillSummary{Name: "ship"}},
	}}
	srv := newTestServer(t, Config{Backend: b})
	_, meta := getMeta(t, srv)
	for _, name := range []string{"activity", "skills"} {
		if meta.Features[name] {
			t.Errorf("the feature map still names %q, which the backend withdrew", name)
		}
	}
	for _, path := range []string{
		"/api/activity", "/api/activity?since=abc", "/api/skills", "/api/skills/ship",
	} {
		res := phaseRequest(t, srv, http.MethodGet, path, "")
		_ = res.Body.Close()
		if res.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s on a withdrawn feature = %d, want 501", path, res.StatusCode)
		}
	}
	// A bad cursor on a withdrawn route is still 501: the route not existing is
	// decided before the query string is read.
	res := phaseRequest(t, srv, http.MethodPost, "/api/skills/trust", "")
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotImplemented {
		t.Errorf("the trust mutation on a withdrawn feature = %d, want 501", res.StatusCode)
	}
	// What was not withdrawn still works.
	res = phaseRequest(t, srv, http.MethodGet, "/api/commands", "")
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("a feature that was not withdrawn = %d, want 200", res.StatusCode)
	}
}

// The text of a "not now" is read by a person. ErrBusy names its package,
// because a Go error is written for a log, and that must not reach a browser.
func TestABusyAnswerReadsAsASentence(t *testing.T) {
	b := &phaseBackend{fakeBackend: &fakeBackend{}, err: Busy("close a conversation first")}
	srv := newTestServer(t, Config{Backend: b})
	res := phaseRequest(t, srv, http.MethodGet, "/api/commands", "")
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a busy answer = %d, want 503", res.StatusCode)
	}
	if got := strings.TrimSpace(string(body)); got != "close a conversation first" {
		t.Fatalf("the busy answer reads %q, want the sentence the backend wrote", got)
	}
	b.err = ErrBusy
	res = phaseRequest(t, srv, http.MethodGet, "/api/commands", "")
	body, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()
	if strings.Contains(string(body), "web:") {
		t.Fatalf("the bare busy answer reads as a package error: %q", body)
	}
}
