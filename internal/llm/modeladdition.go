package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"unicode"

	"github.com/gigovich/aigem/internal/store"
)

// ModelAddition registers a model without selecting it or storing credentials.
// Provider is nil for an existing trusted provider; otherwise it defines a new
// OpenAI-compatible provider whose ID must equal ProviderID.
type ModelAddition struct {
	ProviderID string
	Provider   *Provider
	Model      ModelInfo
}

func cloneProvider(p Provider) Provider {
	p.Headers = maps.Clone(p.Headers)
	p.Models = slices.Clone(p.Models)
	for i := range p.Models {
		if p.Models[i].Temperature != nil {
			temperature := *p.Models[i].Temperature
			p.Models[i].Temperature = &temperature
		}
	}
	return p
}

func registrySources(local Provider, files []string, projectFile, userFile string) *Registry {
	r := &Registry{
		localSource: cloneProvider(local),
		files:       slices.Clone(files),
		projectFile: projectFile,
		userFile:    userFile,
	}
	r.applySource(cloneProvider(local), true)
	r.applySource(openAIPresets(), true)
	r.applySource(xaiPresets(), true)
	return r
}

func (r *Registry) applySource(p Provider, trusted bool) {
	r.upsert(p, trusted)
	if trusted {
		t := Registry{providers: r.trusted}
		t.upsert(cloneProvider(p), true)
		r.trusted = t.providers
	}
}

// AddableProviders returns a detached snapshot of trusted built-in/user
// providers. It performs no IO, excludes managed local models and never exposes
// project-only endpoints as destinations for saved credentials.
func (r *Registry) AddableProviders() []Provider {
	var providers []Provider
	for _, p := range r.trusted {
		if p.ID != LocalProviderID && p.ID != "" {
			providers = append(providers, cloneProvider(p))
		}
	}
	return providers
}

func validModelID(id string) bool {
	return id != "" && !strings.ContainsFunc(id, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

func validProviderID(id string) bool {
	return validModelID(id) && !strings.ContainsAny(id, "/\\")
}

func (add ModelAddition) validate() error {
	if !validProviderID(add.ProviderID) || add.ProviderID == LocalProviderID {
		return fmt.Errorf("invalid or reserved provider ID")
	}
	m := add.Model
	if !validModelID(m.ID) {
		return fmt.Errorf("model ID must be nonempty and contain no whitespace or control characters")
	}
	if m.Provider != "" && m.Provider != add.ProviderID {
		return fmt.Errorf("model provider does not match provider ID")
	}
	if m.ContextWindow < 0 || m.MaxTokens < 0 {
		return fmt.Errorf("context and output limits must be positive when specified")
	}
	if m.ContextWindow > 0 && m.MaxTokens > m.ContextWindow {
		return fmt.Errorf("output limit must not exceed context window")
	}
	if p := add.Provider; p != nil {
		if p.ID != add.ProviderID {
			return fmt.Errorf("new provider ID does not match provider ID")
		}
		if p.API != APICompletions || (p.Auth != AuthNone && p.Auth != AuthAPIKey) {
			return fmt.Errorf("new providers must use OpenAI completions with none or apikey authentication")
		}
		if len(p.Headers) != 0 || len(p.Models) != 0 {
			return fmt.Errorf("new provider headers and embedded models are not supported")
		}
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
			u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(p.BaseURL, "#") {
			return fmt.Errorf("base URL must use http or https with a host and no userinfo, query or fragment")
		}
	}
	return nil
}

func (r *Registry) checkAddition(add ModelAddition) error {
	if add.Provider != nil {
		if _, exists := r.provider(add.ProviderID); exists {
			return fmt.Errorf("provider %q already exists", add.ProviderID)
		}
	} else {
		trusted := false
		for _, p := range r.trusted {
			if p.ID == add.ProviderID {
				trusted = true
				break
			}
		}
		if !trusted {
			return fmt.Errorf("provider %q is not a trusted user or built-in provider", add.ProviderID)
		}
	}
	if _, _, err := r.Resolve(add.ProviderID + "/" + add.Model.ID); err == nil {
		return fmt.Errorf("model %q already exists", add.ProviderID+"/"+add.Model.ID)
	}
	return nil
}

// AddModel atomically adds to the user models.json and returns a fresh registry.
// It does not probe endpoints, store credentials, or change the active/default
// model. Validation errors and collisions leave the original bytes unchanged.
func (r *Registry) AddModel(add ModelAddition) (*Registry, error) {
	if err := add.validate(); err != nil {
		return nil, err
	}
	if err := r.checkAddition(add); err != nil {
		return nil, err
	}
	if r.userFile == "" {
		return nil, fmt.Errorf("user model configuration path is unavailable")
	}
	var result *Registry
	err := store.New[json.RawMessage](r.userFile).Update(func(raw *json.RawMessage) error {
		doc, err := parseModelDocument(*raw)
		if err != nil {
			return err
		}
		// Rebuild while holding the user-file update lock, so additions made
		// since this registry was loaded cannot be lost or replaced.
		fresh, err := r.reloadForAddition(doc.providers)
		if err != nil {
			return err
		}
		if err := fresh.checkAddition(add); err != nil {
			return err
		}
		if err := doc.append(add); err != nil {
			return err
		}
		*raw, err = doc.marshal()
		if err != nil {
			return err
		}
		// The ref is new in the complete effective registry, so no project
		// model can override this addition. Keep every existing overlay intact.
		p := Provider{ID: add.ProviderID}
		if add.Provider != nil {
			p = *add.Provider
		}
		p.Models = []ModelInfo{add.Model}
		fresh.applySource(cloneProvider(p), true)
		result = fresh
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (r *Registry) reloadForAddition(user []Provider) (*Registry, error) {
	fresh := registrySources(r.localSource, r.files, r.projectFile, r.userFile)
	for i := len(r.files) - 1; i >= 0; i-- {
		path := r.files[i]
		providers := user
		if path != r.userFile {
			data, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read models: %w", err)
			}
			doc, err := parseModelDocument(data)
			if err != nil {
				return nil, fmt.Errorf("invalid models configuration %s: %w", path, err)
			}
			providers = doc.providers
		}
		for _, p := range providers {
			fresh.applySource(p, path != r.projectFile)
		}
	}
	return fresh, nil
}

// Raw objects preserve unknown fields at the document, provider and model
// levels, including when appending to an existing provider's models array.
type modelDocument struct {
	object    map[string]json.RawMessage // nil for the bare-array shape
	entries   []json.RawMessage
	providers []Provider
}

func parseModelDocument(data []byte) (*modelDocument, error) {
	d := &modelDocument{}
	if data == nil {
		d.object = make(map[string]json.RawMessage)
		return d, nil
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || (data[0] != '[' && data[0] != '{') {
		return nil, fmt.Errorf("models configuration must be an object or provider array")
	}
	entries := data
	if data[0] == '{' {
		if err := json.Unmarshal(data, &d.object); err != nil {
			return nil, err
		}
		var exists bool
		entries, exists = d.object["providers"]
		if !exists {
			entries = []byte("[]")
		}
	}
	if len(bytes.TrimSpace(entries)) == 0 || bytes.TrimSpace(entries)[0] != '[' {
		return nil, fmt.Errorf("providers must be an array")
	}
	if err := json.Unmarshal(entries, &d.entries); err != nil {
		return nil, err
	}
	for _, entry := range d.entries {
		var p Provider
		if err := json.Unmarshal(entry, &p); err != nil {
			return nil, err
		}
		if !validProviderID(p.ID) {
			return nil, fmt.Errorf("models configuration contains an invalid provider ID")
		}
		for _, m := range p.Models {
			if !validModelID(m.ID) {
				return nil, fmt.Errorf("models configuration contains an invalid model ID")
			}
		}
		d.providers = append(d.providers, p)
	}
	return d, nil
}

func (d *modelDocument) append(add ModelAddition) error {
	model, err := json.Marshal(add.Model)
	if err != nil {
		return err
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(model, &metadata); err != nil {
		return err
	}
	delete(metadata, "provider")
	model, err = json.Marshal(metadata)
	if err != nil {
		return err
	}
	index := -1
	for i, p := range d.providers {
		if p.ID == add.ProviderID {
			index = i
		}
	}
	entry := make(map[string]json.RawMessage)
	var models []json.RawMessage
	if index >= 0 {
		if err := json.Unmarshal(d.entries[index], &entry); err != nil {
			return err
		}
		if raw, ok := entry["models"]; ok {
			if err := json.Unmarshal(raw, &models); err != nil {
				return err
			}
		}
	} else {
		entry["id"], _ = json.Marshal(add.ProviderID)
		if p := add.Provider; p != nil {
			entry["base_url"], _ = json.Marshal(p.BaseURL)
			entry["api"], _ = json.Marshal(p.API)
			entry["auth"], _ = json.Marshal(p.Auth)
		}
	}
	models = append(models, model)
	entry["models"], err = json.Marshal(models)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if index >= 0 {
		d.entries[index] = encoded
	} else {
		d.entries = append(d.entries, encoded)
	}
	return nil
}

func (d *modelDocument) marshal() (json.RawMessage, error) {
	entries, err := json.Marshal(d.entries)
	if err != nil {
		return nil, err
	}
	if d.object == nil {
		return entries, nil
	}
	d.object["providers"] = entries
	return json.Marshal(d.object)
}
