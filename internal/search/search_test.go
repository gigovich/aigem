package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTempState points StateDir (via XDG_STATE_HOME) at a temp dir so config
// read/write does not touch the real user state.
func withTempState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

func TestConfigRoundTrip(t *testing.T) {
	withTempState(t)

	if Exists() {
		t.Fatal("Exists() should be false before any save")
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if c.Provider != "" {
		t.Fatalf("missing config should yield empty provider, got %q", c.Provider)
	}

	want := Config{Provider: ProviderBrave, Brave: &BraveConfig{APIKey: "secret-key"}}
	if err := Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !Exists() {
		t.Fatal("Exists() should be true after save")
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Provider != ProviderBrave || got.Brave == nil || got.Brave.APIKey != "secret-key" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	if err := Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if Exists() {
		t.Fatal("Exists() should be false after Clear")
	}
	if err := Clear(); err != nil {
		t.Fatalf("Clear on missing file should be a no-op: %v", err)
	}
}

func TestSaveUses0600(t *testing.T) {
	withTempState(t)
	if err := Save(Config{Provider: ProviderBrave, Brave: &BraveConfig{APIKey: "k"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path := filepath.Join(os.Getenv("XDG_STATE_HOME"), "aigem", "search.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("search.json perms = %o, want 600 (holds a secret)", perm)
	}
}

func TestEnabled(t *testing.T) {
	cases := []struct {
		name string
		c    Config
		want bool
	}{
		{"empty", Config{}, false},
		{"brave no key", Config{Provider: ProviderBrave, Brave: &BraveConfig{}}, false},
		{"brave nil", Config{Provider: ProviderBrave}, false},
		{"brave ok", Config{Provider: ProviderBrave, Brave: &BraveConfig{APIKey: "k"}}, true},
		{"browser", Config{Provider: ProviderBrowser}, true},
		{"browser invalid", Config{Provider: ProviderBrowser, Browser: &BrowserConfig{Engine: "bad"}}, false},
		{"unknown", Config{Provider: "duck"}, false},
	}
	for _, tc := range cases {
		if got := tc.c.Enabled(); got != tc.want {
			t.Errorf("%s: Enabled()=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestSearcherSelection(t *testing.T) {
	if _, err := (Config{Provider: ProviderBrave, Brave: &BraveConfig{APIKey: "k"}}).Searcher(); err != nil {
		t.Errorf("brave with key should build a searcher: %v", err)
	}
	if _, err := (Config{Provider: ProviderBrowser}).Searcher(); err != nil {
		t.Errorf("browser should build a searcher: %v", err)
	}
	for _, c := range []Config{
		{},
		{Provider: ProviderBrave},
		{Provider: ProviderBrowser, Browser: &BrowserConfig{Engine: "bad"}},
		{Provider: "duck"},
	} {
		if _, err := c.Searcher(); err == nil {
			t.Errorf("Searcher() for %+v should error", c)
		}
	}
}

func TestBraveSearchSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Subscription-Token"); got != "test-key" {
			t.Errorf("missing/wrong token header: %q", got)
		}
		if got := r.URL.Query().Get("q"); got != "go version" {
			t.Errorf("query = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"web":{"results":[
			{"title":"Go 1.25 <strong>release</strong>","url":"https://go.dev","description":"the  <strong>latest</strong>   stable"},
			{"title":"second","url":"https://example.com","description":"desc"}
		]}}`))
	}))
	defer srv.Close()

	b := &brave{apiKey: "test-key", endpoint: srv.URL, http: srv.Client()}
	res, err := b.Search(context.Background(), "go version", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2", len(res))
	}
	if res[0].Title != "Go 1.25 release" {
		t.Errorf("strong tags not stripped: %q", res[0].Title)
	}
	if res[0].Description != "the latest stable" {
		t.Errorf("whitespace/tags not cleaned: %q", res[0].Description)
	}
}

func TestBrowserSearchDelegatesToAutomator(t *testing.T) {
	var gotQuery string
	var gotCount int
	want := []Result{{Title: "Go", URL: "https://go.dev", Description: "the Go site"}}
	b := &browser{
		cfg: normalizeBrowserConfig(&BrowserConfig{}),
		automator: func(_ context.Context, cfg BrowserConfig, query string, count int) ([]Result, error) {
			gotQuery, gotCount = query, count
			if cfg.Engine != BrowserEngineDuckDuckGo {
				t.Fatalf("engine = %q", cfg.Engine)
			}
			return want, nil
		},
	}
	res, err := b.Search(context.Background(), "  latest Go stable version  ", 7)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotQuery != "latest Go stable version" {
		t.Fatalf("query not trimmed/forwarded: %q", gotQuery)
	}
	if gotCount != 7 {
		t.Fatalf("count not forwarded: %d", gotCount)
	}
	if len(res) != 1 || res[0].URL != "https://go.dev" {
		t.Fatalf("unexpected browser result: %+v", res)
	}
	if _, err := b.Search(context.Background(), "   ", 5); err == nil {
		t.Fatal("blank query should error before delegating")
	}
}

func TestBrowserSearchURLByEngine(t *testing.T) {
	cases := []struct {
		engine string
		want   string
	}{
		{BrowserEngineGoogle, "https://www.google.com/search?"},
		{BrowserEngineDuckDuckGo, "https://duckduckgo.com/?"},
		{BrowserEngineBing, "https://www.bing.com/search?"},
	}
	for _, tc := range cases {
		got, err := browserSearchURL(tc.engine, "a b", 3)
		if err != nil {
			t.Fatalf("%s: %v", tc.engine, err)
		}
		if !strings.HasPrefix(got, tc.want) || !strings.Contains(got, "q=a+b") {
			t.Errorf("%s URL = %q", tc.engine, got)
		}
	}
}

func TestBrowserScrapeURL(t *testing.T) {
	got, err := browserScrapeURL(BrowserEngineDuckDuckGo, "a b", 5)
	if err != nil {
		t.Fatalf("ddg: %v", err)
	}
	if !strings.HasPrefix(got, "https://html.duckduckgo.com/html/?") || !strings.Contains(got, "q=a+b") {
		t.Errorf("ddg scrape URL should use the no-JS HTML endpoint, got %q", got)
	}
	for _, engine := range []string{BrowserEngineGoogle, BrowserEngineBing} {
		got, err := browserScrapeURL(engine, "a b", 5)
		if err != nil {
			t.Fatalf("%s: %v", engine, err)
		}
		if !strings.Contains(got, "q=a+b") {
			t.Errorf("%s scrape URL = %q", engine, got)
		}
	}
	if _, err := browserScrapeURL("bad", "q", 5); err == nil {
		t.Error("unknown engine should error")
	}
}

func TestFilterSerpEntries(t *testing.T) {
	entries := []browserSerpEntry{
		{Title: "Go", URL: "https://duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F", Snippet: "  the Go site "},
		{Title: "dup", URL: "https://duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F", Snippet: "again"},
		{Title: "internal", URL: "https://duckduckgo.com/about", Snippet: "x"},
		{Title: "Example", URL: "https://example.com/", Snippet: "ex"},
	}
	out := filterSerpEntries(BrowserEngineDuckDuckGo, entries, 5)
	if len(out) != 2 {
		t.Fatalf("got %d results, want 2 (dedup + internal dropped): %+v", len(out), out)
	}
	if out[0].URL != "https://go.dev/" {
		t.Errorf("uddg redirect not unwrapped: %q", out[0].URL)
	}
	if out[0].Description != "the Go site" {
		t.Errorf("snippet not trimmed: %q", out[0].Description)
	}
	if out[1].URL != "https://example.com/" {
		t.Errorf("second result = %q", out[1].URL)
	}
}

func TestFilterSerpEntriesGoogleBing(t *testing.T) {
	google := filterSerpEntries(BrowserEngineGoogle, []browserSerpEntry{
		{Title: "real", URL: "https://www.google.com/url?q=https%3A%2F%2Fgo.dev%2Fdoc"},
		{Title: "internal", URL: "https://www.google.com/search?q=x"},
	}, 5)
	if len(google) != 1 || google[0].URL != "https://go.dev/doc" {
		t.Fatalf("google /url should unwrap and internal links drop: %+v", google)
	}
	bing := filterSerpEntries(BrowserEngineBing, []browserSerpEntry{
		{Title: "internal", URL: "https://www.bing.com/search?q=x"},
		{Title: "real", URL: "https://example.org/page"},
	}, 5)
	if len(bing) != 1 || bing[0].URL != "https://example.org/page" {
		t.Fatalf("bing internal link should drop: %+v", bing)
	}
}

func TestFormatBrowserPageIncludesLinksAndForms(t *testing.T) {
	res := formatBrowserPage(browserPageContent{
		Title:   "Docs",
		Heading: "Docs",
		Text:    "Some body text about the API.",
		Links:   []browserLink{{Text: "Reference", URL: "https://site/ref"}},
		Forms:   []browserForm{{Action: "https://site/search", Method: "GET", Fields: []string{"q"}, Search: true}},
	}, "https://site/docs", "fallback")

	if res.Title != "Docs" || res.URL != "https://site/docs" {
		t.Fatalf("unexpected title/url: %+v", res)
	}
	for _, want := range []string{
		"Some body text about the API.",
		"Links on this page",
		"Reference -> https://site/ref",
		"Search/forms on this page",
		"[search] GET https://site/search",
		"fields: q",
	} {
		if !strings.Contains(res.Description, want) {
			t.Errorf("description missing %q\n%s", want, res.Description)
		}
	}
}

func TestFormatBrowserPageFallbackTitle(t *testing.T) {
	res := formatBrowserPage(browserPageContent{Text: "x"}, "https://site", "fallback")
	if res.Title != "fallback" {
		t.Errorf("empty page title should fall back, got %q", res.Title)
	}
}

func TestFormatBrowserPageEmptySignalsFailure(t *testing.T) {
	res := formatBrowserPage(browserPageContent{}, "https://site", "")
	if !strings.Contains(res.Description, "no readable text") {
		t.Errorf("empty extraction should signal failure, not return blank: %q", res.Description)
	}
}

func TestInternalAddressReason(t *testing.T) {
	blocked := []string{
		"localhost", "app.localhost", "db.internal", "printer.local",
		"127.0.0.1", "::1", "0.0.0.0", "169.254.169.254", "10.0.0.5",
		"192.168.1.1", "172.16.0.1", "fe80::1", "fd00::1",
	}
	for _, h := range blocked {
		if internalAddressReason(h) == "" {
			t.Errorf("%q should be blocked as internal", h)
		}
	}
	public := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:2800:220:1::"}
	for _, h := range public {
		if r := internalAddressReason(h); r != "" {
			t.Errorf("%q should be allowed, blocked as %q", h, r)
		}
	}
}

func TestOpenBrowserPageRejectsInternalAndBadScheme(t *testing.T) {
	cfg := normalizeBrowserConfig(&BrowserConfig{})
	for _, target := range []string{
		"http://127.0.0.1:8080/admin",
		"http://localhost/x",
		"http://169.254.169.254/latest/meta-data/",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"not a url",
	} {
		if _, err := openBrowserPage(context.Background(), cfg, target); err == nil {
			t.Errorf("openBrowserPage(%q) should error before launching a browser", target)
		}
	}
}

func TestNewBrowseToolGating(t *testing.T) {
	if NewBrowseTool(Config{Provider: ProviderBrave, Brave: &BraveConfig{APIKey: "k"}}) != nil {
		t.Error("open_url is browser-only; brave should yield nil")
	}
	if NewBrowseTool(Config{}) != nil {
		t.Error("disabled config should yield nil")
	}
	if NewBrowseTool(Config{Provider: ProviderBrowser, Browser: &BrowserConfig{Engine: "bad"}}) != nil {
		t.Error("invalid browser config should yield nil")
	}
	if NewBrowseTool(Config{Provider: ProviderBrowser}) == nil {
		t.Error("enabled browser config should build open_url")
	}
}

func TestBrowseToolRun(t *testing.T) {
	var gotURL string
	tool := &BrowseTool{
		cfg: normalizeBrowserConfig(&BrowserConfig{}),
		open: func(_ context.Context, _ BrowserConfig, target string) (Result, error) {
			gotURL = target
			return Result{Title: "Page", URL: "https://x/final", Description: "body\nLinks on this page"}, nil
		},
	}
	out, err := tool.Run(context.Background(), json.RawMessage(`{"url":"https://x/start"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotURL != "https://x/start" {
		t.Errorf("url not forwarded, got %q", gotURL)
	}
	for _, want := range []string{"Page", "https://x/final", "body", "Links on this page"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"url":"  "}`)); err == nil {
		t.Error("blank url should error")
	}
}

func TestBrowserConfigValidation(t *testing.T) {
	cases := []BrowserConfig{
		{Engine: "bad"},
		{Mode: "cdp"},
	}
	for _, c := range cases {
		if err := validateBrowserConfig(&c); err == nil {
			t.Errorf("validateBrowserConfig(%+v) should fail", c)
		}
	}
	if err := validateBrowserConfig(&BrowserConfig{ProfileDir: "/tmp/aigem-profile"}); err != nil {
		t.Errorf("profile without explicit executable should validate: %v", err)
	}
}

func TestPrepareBrowserConfigCreatesDefaultProfile(t *testing.T) {
	withTempState(t)
	cfg, err := PrepareBrowserConfig(&BrowserConfig{Engine: BrowserEngineDuckDuckGo})
	if err != nil {
		t.Fatalf("PrepareBrowserConfig: %v", err)
	}
	want := filepath.Join(os.Getenv("XDG_STATE_HOME"), "aigem", "browser-profile")
	if cfg.ProfileDir != want {
		t.Fatalf("ProfileDir = %q, want %q", cfg.ProfileDir, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("default profile dir was not created: info=%+v err=%v", info, err)
	}
	if cfg.Executable != "" {
		t.Fatalf("Executable should remain auto-detected at runtime, got %q", cfg.Executable)
	}
}

func TestIsCaptchaSignal(t *testing.T) {
	cases := []browserPageSignal{
		{Title: "Attention Required! | Cloudflare", Text: "Checking if the site connection is secure"},
		{Text: "Our systems have detected unusual traffic from your computer network"},
		{Text: "Verify you are human to continue"},
		{HasCaptchaFrame: true},
		{HasCaptchaInput: true},
	}
	for _, tc := range cases {
		if !isCaptchaSignal(tc) {
			t.Fatalf("expected captcha signal for %+v", tc)
		}
	}
	if isCaptchaSignal(browserPageSignal{Title: "Normal page", Text: "A regular article about Go releases."}) {
		t.Fatal("normal page should not be classified as captcha")
	}
	if isCaptchaSignal(browserPageSignal{Text: "This site is protected by reCAPTCHA and the Google Privacy Policy and Terms of Service apply."}) {
		t.Fatal("passive protected-by-recaptcha notice should not be classified as active captcha")
	}
	if !isCaptchaSignal(browserPageSignal{Text: "Please solve the reCAPTCHA to continue."}) {
		t.Fatal("active recaptcha instruction should be classified as captcha")
	}
}

func TestNormalizeBrowserPageTextDedupesAndKeepsStructure(t *testing.T) {
	got := normalizeBrowserPageText(browserPageContent{
		Heading: "  Product Price  ",
		Text:    "Product Price\n\n Menu   Menu \nFirst paragraph with   spaces.\nFirst paragraph with spaces.\nSecond paragraph.\n",
	})
	if strings.Contains(got, "Product Price\nProduct Price") {
		t.Fatalf("heading should be deduped: %q", got)
	}
	if strings.Contains(got, "First paragraph with spaces.\nFirst paragraph with spaces.") {
		t.Fatalf("duplicate lines should be removed: %q", got)
	}
	for _, want := range []string{"Product Price", "First paragraph with spaces.", "Second paragraph."} {
		if !strings.Contains(got, want) {
			t.Fatalf("normalized text missing %q: %q", want, got)
		}
	}
}

func TestNormalizeBrowserPageTextTruncates(t *testing.T) {
	got := normalizeBrowserPageText(browserPageContent{Text: strings.Repeat("word ", 1200)})
	if len(got) > 4003 || !strings.HasSuffix(got, "...") {
		t.Fatalf("expected truncated text with ellipsis, len=%d suffix=%q", len(got), got[len(got)-3:])
	}
}

func TestBraveSearchEmptyQuery(t *testing.T) {
	b := &brave{apiKey: "k", endpoint: "http://unused", http: http.DefaultClient}
	if _, err := b.Search(context.Background(), "   ", 5); err == nil {
		t.Error("empty query should error before any request")
	}
}

func TestBraveSearchAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()

	b := &brave{apiKey: "bad", endpoint: srv.URL, http: srv.Client()}
	_, err := b.Search(context.Background(), "q", 5)
	if err == nil {
		t.Fatal("expected auth error")
	}
	if !strings.Contains(err.Error(), "API key") {
		t.Errorf("auth error should mention the key: %v", err)
	}
	if strings.Contains(err.Error(), "bad") && strings.Contains(err.Error(), "key") && strings.Contains(err.Error(), "\"bad key\"") {
		t.Errorf("auth error should not leak response body verbatim: %v", err)
	}
}

func TestBraveCountClamped(t *testing.T) {
	var gotCount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCount = r.URL.Query().Get("count")
		_, _ = w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer srv.Close()

	b := &brave{apiKey: "k", endpoint: srv.URL, http: srv.Client()}
	if _, err := b.Search(context.Background(), "q", 999); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotCount != "20" {
		t.Errorf("count not clamped to 20, got %q", gotCount)
	}
}

// fakeSearcher returns canned results for tool tests.
type fakeSearcher struct {
	results []Result
	err     error
	gotN    int
}

func (f *fakeSearcher) Search(_ context.Context, _ string, count int) ([]Result, error) {
	f.gotN = count
	return f.results, f.err
}

func TestToolRunFormatsResults(t *testing.T) {
	f := &fakeSearcher{results: []Result{
		{Title: "First", URL: "https://a", Description: "alpha"},
		{Title: "Second", URL: "https://b", Description: ""},
	}}
	tool := &Tool{provider: ProviderBrave, searcher: f}
	out, err := tool.Run(context.Background(), json.RawMessage(`{"query":"hi","count":3}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.gotN != 3 {
		t.Errorf("count not forwarded, got %d", f.gotN)
	}
	for _, want := range []string{"1. First", "https://a", "alpha", "2. Second", "https://b"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestToolRunNoResults(t *testing.T) {
	tool := &Tool{provider: ProviderBrave, searcher: &fakeSearcher{}}
	out, err := tool.Run(context.Background(), json.RawMessage(`{"query":"zzz"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "No results") {
		t.Errorf("expected no-results message, got %q", out)
	}
}

func TestToolRunEmptyQuery(t *testing.T) {
	tool := &Tool{provider: ProviderBrave, searcher: &fakeSearcher{}}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"query":"  "}`)); err == nil {
		t.Error("blank query should error")
	}
}

func TestNewToolNilWhenDisabled(t *testing.T) {
	if NewTool(Config{}) != nil {
		t.Error("NewTool should be nil for an unconfigured provider")
	}
	if NewTool(Config{Provider: ProviderBrave, Brave: &BraveConfig{APIKey: "k"}}) == nil {
		t.Error("NewTool should build a tool for an enabled config")
	}
}

func TestPromptGating(t *testing.T) {
	if Prompt(Config{}) != "" {
		t.Error("Prompt should be empty when search is disabled")
	}
	p := Prompt(Config{Provider: ProviderBrave, Brave: &BraveConfig{APIKey: "k"}})
	if !strings.Contains(p, "web_search") {
		t.Errorf("enabled prompt should mention web_search: %q", p)
	}
	bp := Prompt(Config{Provider: ProviderBrowser, Browser: &BrowserConfig{Engine: BrowserEngineGoogle}})
	for _, want := range []string{"Browser-provider rules", "Do NOT use bash, curl, Python", "managed browser", "Use the external search engine only for discovery", "site's own UI/navigation/search/filter controls", "open_url"} {
		if !strings.Contains(bp, want) {
			t.Errorf("browser prompt missing %q: %q", want, bp)
		}
	}
}
