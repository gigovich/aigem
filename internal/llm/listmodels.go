package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxListedModels = 1000
	maxModelIDLen   = 200
)

var listClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// openAIChat and openAINotChat pick, out of everything OpenAI lists, the models
// its chat-completions API serves.
var (
	openAIChat    = regexp.MustCompile(`^(gpt-|chatgpt-|o\d)`)
	openAINotChat = regexp.MustCompile(`realtime|audio|transcribe|tts|search|image|embedding|moderation|codex|-pro\b`)
)

// ListModels asks an OpenAI-compatible server which models it serves, at
// {base}/v1/models. token is nil for a server that needs none. What the listing
// leaves out is filled from the catalog.
func ListModels(ctx context.Context, p Provider, token func(context.Context) (string, error)) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.BaseURL, "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	if token != nil {
		tok, err := token(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := listClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing models: %s", resp.Status)
	}
	var body struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
			MaxModelLen   int    `json:"max_model_len"`
			Meta          *struct {
				NCtxTrain int `json:"n_ctx_train"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("listing models: %w", err)
	}
	var out []ModelInfo
	for _, d := range body.Data {
		if !validModelID(d.ID) || len(d.ID) > maxModelIDLen || len(out) == maxListedModels {
			continue
		}
		if p.ID == OpenAIProviderID && (!openAIChat.MatchString(d.ID) || openAINotChat.MatchString(d.ID)) {
			continue
		}
		m := ModelInfo{Provider: p.ID, ID: d.ID, Name: d.ID, ContextWindow: d.ContextLength}
		if m.ContextWindow == 0 {
			m.ContextWindow = d.MaxModelLen
		}
		if m.ContextWindow == 0 && d.Meta != nil {
			m.ContextWindow = d.Meta.NCtxTrain
		}
		out = append(out, mergeModelInfo(catalogModel(p.ID, d.ID), m))
	}
	slices.SortFunc(out, func(a, b ModelInfo) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// LocalServer is an OpenAI-compatible server found on this machine.
type LocalServer struct {
	Name    string      `json:"name"`
	BaseURL string      `json:"baseUrl"`
	Models  []ModelInfo `json:"models"`
}

// localPorts are the default ports of the common local model servers. The
// managed llama-server's own port is left out: it is set up, not discovered.
var localPorts = []struct {
	port int
	name string
}{
	{11434, "ollama"},
	{1234, "lmstudio"},
	{9931, "llamacpp"},
	{8080, "local-8080"},
	{8000, "vllm"},
	{1337, "jan"},
	{5001, "koboldcpp"},
}

// localServerName names the server at baseURL: by its default port when it is
// a known one on this machine, else by its host.
func localServerName(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "server"
	}
	if h := u.Hostname(); net.ParseIP(h).IsLoopback() || h == "localhost" {
		for _, lp := range localPorts {
			if u.Port() == strconv.Itoa(lp.port) {
				return lp.name
			}
		}
	}
	return u.Hostname()
}

// discoverLocal probes the loopback ports local model servers use by default and
// returns the ones that answer with at least one model.
func discoverLocal(ctx context.Context) []LocalServer {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	found := make([]*LocalServer, len(localPorts))
	var wg sync.WaitGroup
	for i, lp := range localPorts {
		wg.Go(func() {
			base := "http://127.0.0.1:" + strconv.Itoa(lp.port)
			if models, err := ListModels(ctx, Provider{BaseURL: base}, nil); err == nil && len(models) > 0 {
				found[i] = &LocalServer{Name: lp.name, BaseURL: base, Models: models}
			}
		})
	}
	wg.Wait()
	var out []LocalServer
	for _, s := range found {
		if s != nil {
			out = append(out, *s)
		}
	}
	return out
}
