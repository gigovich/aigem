package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListModels(t *testing.T) {
	long := strings.Repeat("x", maxModelIDLen+1)
	body := `{"data":[
		{"id":"b","context_length":100},
		{"id":"a","max_model_len":200},
		{"id":"c","meta":{"n_ctx_train":300}},
		{"id":"bad id"},{"id":""},{"id":"` + long + `"},
		{"id":"gpt-5.6-sol"},{"id":"gpt-4o-realtime"},{"id":"dall-e-3"},{"id":"o3"}]}`
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	got, err := ListModels(context.Background(), Provider{ID: "srv", BaseURL: srv.URL + "/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := map[string]int{}
	for _, m := range got {
		ctx[m.ID] = m.ContextWindow
	}
	if len(got) != 7 || got[0].ID != "a" || ctx["a"] != 200 || ctx["b"] != 100 || ctx["c"] != 300 || auth != "" {
		t.Fatalf("models = %+v, auth = %q", got, auth)
	}

	token := func(context.Context) (string, error) { return "tok", nil }
	got, err = ListModels(context.Background(), Provider{ID: OpenAIProviderID, BaseURL: srv.URL}, token)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "gpt-5.6-sol" || got[0].ContextWindow != 1050000 || got[1].ID != "o3" {
		t.Fatalf("openai models = %+v", got)
	}
	if auth != "Bearer tok" {
		t.Fatalf("authorization = %q", auth)
	}
}

func TestListModelsFailures(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer target.Close()
	for name, handler := range map[string]http.HandlerFunc{
		"status": func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusUnauthorized) },
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/v1/models", http.StatusFound)
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()
			if got, err := ListModels(context.Background(), Provider{BaseURL: srv.URL}, nil); err == nil {
				t.Fatalf("listing succeeded: %+v", got)
			}
		})
	}
}
