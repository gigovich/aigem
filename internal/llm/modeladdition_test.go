package llm

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/gigovich/aigem/internal/config"
)

func modelAdditionRegistry(t *testing.T, user, project string) (*Registry, string, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := config.UserModelsFile()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if user != "" {
		write(path, user)
	}
	if project != "" {
		write(config.ProjectModelsFile(cwd), project)
	}
	r, _ := NewRegistry(cwd, testLocal())
	return r, path, cwd
}

func TestAddModelPreservesShapesAndUnknownFields(t *testing.T) {
	provider := `{"id":"custom","base_url":"https://example.com/v1","api":"openai-completions","auth":"none","extra":{"nested":[1,true]},"models":[{"id":"old","future":{"enabled":true}}]}`
	for _, object := range []bool{false, true} {
		name := "array"
		original := "[" + provider + "]"
		if object {
			name = "object"
			original = `{"version":13,"future":{"value":"keep"},"providers":` + original + `}`
		}
		t.Run(name, func(t *testing.T) {
			r, path, cwd := modelAdditionRegistry(t, original, "")
			add := ModelAddition{
				ProviderID: "custom",
				Models:     []ModelInfo{{ID: "org/new", Name: "New model", ContextWindow: 32000, MaxTokens: 4000}},
			}
			fresh, err := r.AddModel(add)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var before, after any
			if err := json.Unmarshal([]byte(original), &before); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &after); err != nil {
				t.Fatal(err)
			}
			entries := after
			if object {
				entries = after.(map[string]any)["providers"]
			}
			p := entries.([]any)[0].(map[string]any)
			models := p["models"].([]any)
			if len(models) != 2 {
				t.Fatalf("models = %v", models)
			}
			p["models"] = models[:1]
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("existing data changed: before=%v after=%v", before, after)
			}
			if _, _, err := r.Resolve("custom/org/new"); err == nil {
				t.Fatal("old registry changed")
			}
			reloaded, _ := NewRegistry(cwd, testLocal())
			for _, reg := range []*Registry{fresh, reloaded} {
				p, m, err := reg.Resolve("custom/org/new")
				if err != nil || p.BaseURL != "https://example.com/v1" || m.Name != "New model" || m.ContextWindow != 32000 || m.MaxTokens != 4000 {
					t.Fatalf("saved model not usable: provider=%+v model=%+v err=%v", p, m, err)
				}
			}
		})
	}
}

func TestAddModelPreservesRuntimeAndProjectTrust(t *testing.T) {
	r, path, _ := modelAdditionRegistry(t, "", `{"providers":[{"id":"openai","base_url":"https://attacker.invalid","headers":{"Authorization":"secret"},"models":[{"id":"project-model","context_window":12345}]},{"id":"project-only","base_url":"https://attacker.invalid","models":[]}]}`)
	r.ReplaceLocal(LocalProvider("http://127.0.0.1:9999", "current.gguf", 98765, 4321))
	for _, p := range r.AddableProviders() {
		if p.ID == "local" || p.ID == "project-only" || p.BaseURL == "https://attacker.invalid" {
			t.Fatalf("untrusted/managed registration destination: %+v", p)
		}
	}
	fresh, err := r.AddModel(ModelAddition{ProviderID: "openai", Models: []ModelInfo{{ID: "new"}}})
	if err != nil {
		t.Fatal(err)
	}
	p, m, err := fresh.Resolve("local/current.gguf")
	if err != nil || p.BaseURL != "http://127.0.0.1:9999" || m.ContextWindow != 98765 || m.MaxTokens != 4321 {
		t.Fatalf("runtime local lost: %+v %+v %v", p, m, err)
	}
	p, m, err = fresh.Resolve("openai/project-model")
	if err != nil || p.BaseURL != "https://api.openai.com" || len(p.Headers) != 0 || m.ContextWindow != 12345 {
		t.Fatalf("project precedence/trust lost: %+v %+v %v", p, m, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Providers []map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Providers) != 1 || len(doc.Providers[0]) != 2 || doc.Providers[0]["id"] == nil || doc.Providers[0]["models"] == nil {
		t.Fatalf("model addition persisted transport/runtime data: %s", data)
	}
}

func TestAddModelRejectsMalformedWithoutWriting(t *testing.T) {
	for _, text := range []string{
		`{"providers":`, `null`, `{"providers":{}}`, `{"providers":[null]}`,
		`{"providers":[{"id":"p","models":[{}]}]}`, `{"providers":[{"id":"p","models":"bad"}]}`,
	} {
		t.Run(text, func(t *testing.T) {
			r, path, _ := modelAdditionRegistry(t, text, "")
			result, err := r.AddModel(ModelAddition{ProviderID: "openai", Models: []ModelInfo{{ID: "new"}}})
			if err == nil || result != nil {
				t.Fatal("malformed data was accepted")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != text {
				t.Fatalf("malformed file changed: %q, %v", data, err)
			}
		})
	}
}

func TestAddModelRejectsInvalidAndReplacingAdditions(t *testing.T) {
	original := `{"providers":[{"id":"custom","base_url":"https://example.com","models":[{"id":"old"}]}]}`
	r, path, _ := modelAdditionRegistry(t, original, `[{"id":"project-only","base_url":"https://project.invalid","models":[]}]`)
	newProvider := func(id, endpoint string) *Provider {
		return &Provider{ID: id, BaseURL: endpoint, API: APICompletions, Auth: AuthNone}
	}
	cases := []ModelAddition{
		{ProviderID: "openai", Models: []ModelInfo{{ID: "new"}, {ID: "new"}}},
		{ProviderID: "custom", Models: []ModelInfo{{ID: "old"}}},
		{ProviderID: "project-only", Models: []ModelInfo{{ID: "new"}}},
		{ProviderID: "local", Models: []ModelInfo{{ID: "new"}}},
		{ProviderID: "bad/provider", Models: []ModelInfo{{ID: "new"}}},
		{ProviderID: "openai", Models: []ModelInfo{{ID: "bad model"}}},
		{ProviderID: "openai", Models: []ModelInfo{{ID: "new", MaxTokens: -1}}},
		{ProviderID: "openai", Models: []ModelInfo{{ID: "new", ContextWindow: 10, MaxTokens: 11}}},
		{ProviderID: "openai", Models: []ModelInfo{{ID: "new", Provider: "other"}}},
	}
	for _, id := range []string{"openai", "custom", "project-only"} {
		cases = append(cases, ModelAddition{
			ProviderID: id,
			Provider:   newProvider(id, "https://replacement.invalid"),
			Models:     []ModelInfo{{ID: "new"}},
		})
	}
	for _, endpoint := range []string{"ftp://host", "https:///path", "https://key@host", "https://host?key=secret", "https://host#secret", "https://host?"} {
		cases = append(cases, ModelAddition{
			ProviderID: "new",
			Provider:   newProvider("new", endpoint),
			Models:     []ModelInfo{{ID: "new"}},
		})
	}
	withHeaders := newProvider("new", "https://host")
	withHeaders.Headers = map[string]string{"Authorization": "secret"}
	cases = append(cases, ModelAddition{ProviderID: "new", Provider: withHeaders, Models: []ModelInfo{{ID: "new"}}})
	for i, add := range cases {
		result, err := r.AddModel(add)
		if err == nil || result != nil {
			t.Fatalf("case %d accepted invalid addition", i)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != original {
			t.Fatalf("case %d changed original bytes: %s, %v", i, data, err)
		}
	}
}

func TestAddModelRechecksConcurrentUserChanges(t *testing.T) {
	r, path, cwd := modelAdditionRegistry(t, "", "")
	provider := &Provider{ID: "custom", BaseURL: "https://example.com/v1", API: APICompletions, Auth: AuthAPIKey}
	fresh, err := r.AddModel(ModelAddition{
		ProviderID: "custom",
		Provider:   provider,
		Models:     []ModelInfo{{ID: "first"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := ModelAddition{ProviderID: "custom", Provider: provider, Models: []ModelInfo{{ID: "replacement"}}}
	if result, err := r.AddModel(replacement); err == nil || result != nil {
		t.Fatal("stale registry replaced provider")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("collision changed bytes: %s %v", after, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{"second", "third"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := fresh.AddModel(ModelAddition{ProviderID: "custom", Models: []ModelInfo{{ID: id}}})
			errs <- err
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	reloaded, _ := NewRegistry(cwd, testLocal())
	for _, id := range []string{"first", "second", "third"} {
		if _, _, err := reloaded.Resolve("custom/" + id); err != nil {
			t.Fatal(err)
		}
	}
	before, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second := ModelAddition{ProviderID: "custom", Models: []ModelInfo{{ID: "second"}}}
	if result, err := fresh.AddModel(second); err == nil || result != nil {
		t.Fatal("stale registry replaced model")
	}
	after, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("duplicate changed bytes: %s %v", after, err)
	}
}

func TestAddModelBatch(t *testing.T) {
	r, path, cwd := modelAdditionRegistry(t, "", "")
	fresh, err := r.AddModel(ModelAddition{
		ProviderID: "openai",
		Models:     []ModelInfo{{ID: "gpt-5.6-sol"}, {ID: "other"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reloaded, _ := NewRegistry(cwd, testLocal())
	for _, reg := range []*Registry{fresh, reloaded} {
		for _, ref := range []string{"openai/gpt-5.6-sol", "openai/other"} {
			if _, _, err := reg.Resolve(ref); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, m, _ := fresh.Resolve("openai/gpt-5.6-sol"); m.ContextWindow != 1050000 {
		t.Fatalf("catalog metadata not merged: %+v", m)
	}
	if data, err := os.ReadFile(path); err != nil || bytes.Count(data, []byte(`"id"`)) != 3 {
		t.Fatalf("saved file = %s, %v", data, err)
	}
}

func TestRemoveModel(t *testing.T) {
	r, path, cwd := modelAdditionRegistry(t,
		`{"providers":[{"id":"custom","base_url":"https://example.com","models":[{"id":"a"},{"id":"b"}]}]}`, "")
	fresh, err := r.RemoveModel("custom/a")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err = fresh.RemoveModel("custom/b")
	if err != nil {
		t.Fatal(err)
	}
	reloaded, _ := NewRegistry(cwd, testLocal())
	for _, reg := range []*Registry{fresh, reloaded} {
		if _, _, err := reg.Resolve("custom/a"); err == nil {
			t.Fatal("removed model still resolves")
		}
		if p, ok := reg.Provider("custom"); !ok || p.BaseURL != "https://example.com" {
			t.Fatalf("provider entry lost: %+v %v", p, ok)
		}
	}
	before, _ := os.ReadFile(path)
	for _, ref := range []string{"custom/a", "custom/nope", "openai/gpt-5.6-sol", "local/gemma.gguf"} {
		if result, err := fresh.RemoveModel(ref); err == nil || result != nil {
			t.Fatalf("%s: removing an unknown model succeeded", ref)
		}
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatalf("failed removal changed bytes: %s", after)
	}
}

func TestServerAddition(t *testing.T) {
	r, _, _ := modelAdditionRegistry(t, `[
		{"id":"mine","base_url":"http://127.0.0.1:11434/","models":[]},
		{"id":"lmstudio","base_url":"http://10.0.0.1:1234","models":[]}]`, "")
	for _, tc := range []struct {
		url, id string
		isNew   bool
	}{
		{"http://127.0.0.1:11434", "mine", false},
		{"http://127.0.0.1:1234/", "lmstudio-2", true},
		{"http://localhost:8000", "vllm", true},
		{"https://models.example.com", "models.example.com", true},
	} {
		add := r.ServerAddition(tc.url)
		if add.ProviderID != tc.id || (add.Provider != nil) != tc.isNew {
			t.Fatalf("%s: addition = %+v", tc.url, add)
		}
		if tc.isNew &&
			(add.Provider.ID != tc.id || add.Provider.Auth != AuthNone || add.Provider.API != APICompletions) {
			t.Fatalf("%s: new provider = %+v", tc.url, add.Provider)
		}
	}
}

func TestNotAdded(t *testing.T) {
	models := `[{"id":"custom","base_url":"https://example.com","models":[{"id":"old"}]}]`
	r, _, _ := modelAdditionRegistry(t, models, "")
	got := r.NotAdded("custom", []ModelInfo{{ID: "old"}, {ID: "new"}})
	if len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("NotAdded = %+v", got)
	}
}

func TestAddModelOnTheReturnedRegistrySeesEarlierAdditions(t *testing.T) {
	r, _, _ := modelAdditionRegistry(t, "", "")
	first, err := r.AddModel(ModelAddition{ProviderID: "openai", Models: []ModelInfo{{ID: "a"}}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := first.AddModel(ModelAddition{ProviderID: "openai", Models: []ModelInfo{{ID: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, _, err := second.Resolve("openai/" + id); err != nil || !second.Trusted("openai", id) {
			t.Fatalf("%s: missing from the returned registry: %v", id, err)
		}
	}
}

func TestProjectOnlyModelOfATrustedProviderCanBeAdded(t *testing.T) {
	r, _, cwd := modelAdditionRegistry(t, "", `[{"id":"openai","models":[{"id":"foo"}]}]`)
	if got := r.NotAdded("openai", []ModelInfo{{ID: "foo"}}); len(got) != 1 {
		t.Fatalf("NotAdded = %+v, want the project-only model offered", got)
	}
	fresh, err := r.AddModel(ModelAddition{ProviderID: "openai", Models: []ModelInfo{{ID: "foo"}}})
	if err != nil {
		t.Fatal(err)
	}
	reloaded, _ := NewRegistry(cwd, testLocal())
	for _, reg := range []*Registry{fresh, reloaded} {
		if !reg.Trusted("openai", "foo") {
			t.Fatal("added model is not trusted")
		}
	}
}

func TestValidNamesRejectFormatCharacters(t *testing.T) {
	for _, s := range []string{"a\u202eb", "a\u200bb"} {
		if validModelID(s) || validName(s) {
			t.Errorf("%q accepted", s)
		}
	}
	if !validModelID("org/model-1.5") || !validName("Model 1.5") {
		t.Error("plain id or name rejected")
	}
}
