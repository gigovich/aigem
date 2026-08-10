package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gigovich/aigem/internal/auth"
)

// The browser login is the point at which this endpoint gains the credential
// store, so the gates matter here more than anywhere else.
func TestLoginRoutesAreGuarded(t *testing.T) {
	srv := testServer(t)
	base := "http://" + srv.Addr().String()

	for _, path := range []string{"/api/models", "/api/auth/login/f-1"} {
		res, err := http.Get(base + path) //nolint:bodyclose // closed below
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %s, want 401", path, res.Status)
		}
	}

	req, _ := http.NewRequest(http.MethodPost, base+"/api/auth/login/openai", nil)
	req.Header.Set("Authorization", "Bearer "+srv.token)
	req.Header.Set("Origin", "http://evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin login = %s, want 403", res.Status)
	}
}

// A provider with no interactive flow must be refused before anything is
// started, rather than leaving a half-open flow nobody can finish.
func TestLoginRefusesProvidersWithoutAFlow(t *testing.T) {
	srv := testServer(t)
	req, _ := http.NewRequest(http.MethodPost,
		"http://"+srv.Addr().String()+"/api/auth/login/anthropic", nil)
	req.Header.Set("Authorization", "Bearer "+srv.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %s, want 400", res.Status)
	}
}

// An unknown flow is a 404, and a paste against one is not an opening to
// deliver a code into some other login.
func TestPasteToAnUnknownFlowIsNotFound(t *testing.T) {
	srv := testServer(t)
	req, _ := http.NewRequest(http.MethodPost,
		"http://"+srv.Addr().String()+"/api/auth/login/f-nope/paste",
		bytes.NewReader([]byte("http://localhost:1455/auth/callback?code=x")))
	req.Header.Set("Authorization", "Bearer "+srv.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %s, want 404", res.Status)
	}
}

// The model list says which models are reachable. A list that does not would
// make the user find out by picking one.
func TestModelsReportAuthentication(t *testing.T) {
	srv := testServer(t)
	res := srv.get(t, "/api/models")
	defer res.Body.Close()
	var out []modelView
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	// The test daemon has no registry, so the list is empty rather than absent -
	// a client should not have to tell those apart.
	if out == nil {
		t.Fatal("the model list was null, not an empty list")
	}
}

// Beginning a login writes into a map on the server. A nil one compiles, passes
// every test that is refused before it, and panics on the first real attempt -
// so the flow registry is exercised directly rather than trusted.
func TestFlowRegistryIsUsable(t *testing.T) {
	srv := testServer(t)
	srv.mu.Lock()
	srv.flowSeq++
	srv.flows["f-test"] = &loginFlow{id: "f-test", flow: &auth.Flow{Provider: "xai"}}
	n := len(srv.flows)
	srv.mu.Unlock()
	if n != 1 {
		t.Fatalf("the server holds %d flows after storing one", n)
	}
	res := srv.get(t, "/api/auth/login/f-test")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", res.Status)
	}
}
