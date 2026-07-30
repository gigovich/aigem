package search

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Tool is the web_search tool the agent invokes. It wraps a configured Searcher.
// It satisfies tools.Tool structurally, so the search package need not import the
// tools package (avoiding an import cycle).
type Tool struct {
	provider string
	searcher Searcher
}

// NewTool builds the web_search tool for an enabled config. It returns nil when
// the config has no usable provider, so callers can register unconditionally.
func NewTool(c Config) *Tool {
	if !c.Enabled() {
		return nil
	}
	s, err := c.Searcher()
	if err != nil {
		return nil
	}
	return &Tool{provider: c.Provider, searcher: s}
}

func (t *Tool) Name() string { return "web_search" }

func (t *Tool) NeedsConfirm() bool { return false }

func (t *Tool) Description() string {
	if t.provider == ProviderBrowser {
		return "Search the public web in the user's configured local browser/profile and return " +
			"ranked results (title, URL, snippet). This finds entry points only; it does NOT open the " +
			"result pages. To read a result, follow a link, or use a site's own search/navigation/" +
			"filters, call open_url with the URL - open_url returns the page text plus the page's links " +
			"and search forms so you keep navigating within that site instead of running another " +
			"search-engine query. If a captcha or human verification appears, aigem pauses in that same " +
			"browser and asks the user to solve and confirm it. Do not fetch web pages with bash, curl, " +
			"Python, or other direct HTTP requests; page browsing must go through the managed browser. " +
			"This covers web pages only - calling an HTTP API (a REST/JMAP/GraphQL endpoint, a service's " +
			"own CLI) with bash or curl is allowed and is the right tool for that job."
	}
	return "Search the public web for current information and return ranked results " +
		"(title, URL, snippet). Use it to verify facts that may be newer than your training " +
		"data: latest package/dependency versions, current library APIs and CLI flags, recent " +
		"releases or breaking changes, and unfamiliar error messages. Never put secrets, " +
		"credentials, file contents, or the user's personal data in a query."
}

func (t *Tool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"query":{"type":"string","description":"The search query. Keep it focused and specific."},
			"count":{"type":"integer","description":"Number of results to return (1-20, default 5)."}
		},
		"required":["query"]
	}`)
}

func (t *Tool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Query) == "" {
		return "", fmt.Errorf("query is required")
	}
	results, err := t.searcher.Search(ctx, a.Query, a.Count)
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return fmt.Sprintf("No results for %q.", a.Query), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Results for %q:\n", a.Query)
	for i, r := range results {
		fmt.Fprintf(&b, "\n%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Description != "" {
			fmt.Fprintf(&b, "   %s\n", truncate(r.Description, 300))
		}
	}
	return b.String(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// BrowseTool is the open_url tool: it navigates the managed browser to one URL
// and returns the page's text plus its links and search forms. It is the
// browser provider's way to read a found page and drive a site's own
// navigation/search, instead of going back to the search engine. It exists only
// for the browser provider (Brave has no managed browser to drive).
type BrowseTool struct {
	cfg  BrowserConfig
	open func(context.Context, BrowserConfig, string) (Result, error)
}

// NewBrowseTool builds the open_url tool for an enabled browser config, or nil
// otherwise, so callers can register it unconditionally alongside web_search.
func NewBrowseTool(c Config) *BrowseTool {
	if c.Provider != ProviderBrowser || !c.Enabled() {
		return nil
	}
	return &BrowseTool{cfg: normalizeBrowserConfig(c.Browser), open: openBrowserPage}
}

func (t *BrowseTool) Name() string { return "open_url" }

func (t *BrowseTool) NeedsConfirm() bool { return false }

func (t *BrowseTool) Description() string {
	return "Open one URL in the user's configured local browser/profile and return the page's " +
		"extracted text plus its in-page links and search forms. Use it to read a page found via " +
		"web_search, to follow a link from a page you already opened, or to run a site's own search: " +
		"for a GET search form listed in a page's output, call open_url with action?field=YOUR+QUERY. " +
		"Prefer this over repeating a search-engine query once the target site is known. Web pages go " +
		"through the managed browser; do not fetch them with bash, curl, Python, or other direct HTTP " +
		"requests. That covers pages only - calling an HTTP API (a REST/JMAP/GraphQL endpoint, a " +
		"service's own CLI) with bash or curl is allowed. If a captcha appears, aigem pauses for the " +
		"user to solve and confirm it."
}

func (t *BrowseTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"url":{"type":"string","description":"Absolute http(s) URL to open in the managed browser."}
		},
		"required":["url"]
	}`)
}

func (t *BrowseTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.URL) == "" {
		return "", fmt.Errorf("url is required")
	}
	res, err := t.open(ctx, t.cfg, a.URL)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if res.Title != "" {
		fmt.Fprintf(&b, "%s\n", res.Title)
	}
	fmt.Fprintf(&b, "%s\n", res.URL)
	if res.Description != "" {
		fmt.Fprintf(&b, "\n%s\n", res.Description)
	}
	return b.String(), nil
}

// BrowserActionTool is the browser_action tool: it drives the managed browser
// through an ordered step sequence (navigate, set_viewport, fill, click,
// press_key, hover, wait) in one session and reports DOM observations. It exists
// only for the browser provider and gives the tester the interactive controls
// open_url lacks - POST login, viewport, keyboard, and DOM inspection.
type BrowserActionTool struct {
	cfg BrowserConfig
	run func(context.Context, BrowserConfig, browserActionRequest) (string, error)
}

// NewBrowserActionTool builds the browser_action tool for an enabled browser
// config, or nil otherwise, so callers can register it unconditionally.
func NewBrowserActionTool(c Config) *BrowserActionTool {
	if c.Provider != ProviderBrowser || !c.Enabled() {
		return nil
	}
	return &BrowserActionTool{cfg: normalizeBrowserConfig(c.Browser), run: runBrowserActions}
}

func (t *BrowserActionTool) Name() string { return "browser_action" }

func (t *BrowserActionTool) NeedsConfirm() bool { return false }

func (t *BrowserActionTool) Description() string {
	return "Drive the managed browser interactively to test a web page: run an ordered list of steps " +
		"in one session, then report DOM observations. Use it when open_url is not enough - to log in " +
		"(fill fields then click submit; a POST form works), set the viewport (e.g. 375px vs desktop), " +
		"press keys (Tab/Enter for focus checks), hover for tooltips, and inspect the DOM. Login persists " +
		"to later calls through the browser profile's cookies. For credentials in a fill step, read them " +
		"from wherever they live (a local file such as ~/.config/keys/<app>, or the ticket) and type the " +
		"value directly. Internal/app hosts must be listed in browser.test_hosts. Report what to check via " +
		"'observe': text (page text), active_element (focused element, for focus checks), query (per-named " +
		"selector: exists/count/text/attrs/computed style, for badge/tooltip/visibility checks), and " +
		"screenshot (saves a PNG and returns its path)."
}

func (t *BrowserActionTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"steps":{
				"type":"array",
				"description":"Ordered actions to run in one browser session.",
				"items":{
					"type":"object",
					"properties":{
						"action":{"type":"string","enum":["navigate","set_viewport","fill","click","press_key","hover","wait_for","wait"],"description":"The step type."},
						"url":{"type":"string","description":"navigate: absolute http(s) URL."},
						"selector":{"type":"string","description":"CSS selector for fill/click/hover/wait_for, and optionally press_key (focus first)."},
						"text":{"type":"string","description":"fill: text to type."},
						"key":{"type":"string","description":"press_key: one of Tab, Enter, Escape, ArrowUp, ArrowDown, Home, End, Space."},
						"width":{"type":"integer","description":"set_viewport: width in px."},
						"height":{"type":"integer","description":"set_viewport: height in px."},
						"timeout_ms":{"type":"integer","description":"wait_for: max wait in ms (default 10000)."},
						"ms":{"type":"integer","description":"wait: sleep in ms."}
					},
					"required":["action"]
				}
			},
			"observe":{
				"type":"object",
				"description":"What to report after the steps run.",
				"properties":{
					"text":{"type":"boolean","description":"Include the page's extracted text."},
					"active_element":{"type":"boolean","description":"Report the currently focused element."},
					"query":{
						"type":"array",
						"description":"Named selectors to inspect.",
						"items":{
							"type":"object",
							"properties":{
								"name":{"type":"string","description":"Label for this query in the result."},
								"selector":{"type":"string","description":"CSS selector to match."},
								"attrs":{"type":"array","items":{"type":"string"},"description":"Attributes to read from the first match (e.g. title, aria-label)."},
								"computed":{"type":"array","items":{"type":"string"},"description":"Computed CSS properties to read (e.g. visibility, outline)."}
							},
							"required":["name","selector"]
						}
					},
					"screenshot":{"type":"boolean","description":"Save a full-page PNG and return its path."}
				}
			}
		},
		"required":["steps"]
	}`)
}

func (t *BrowserActionTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var req browserActionRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return "", err
	}
	return t.run(ctx, t.cfg, req)
}

// Prompt returns the system-prompt fragment describing the web_search tool, to
// be appended when search is enabled. It returns "" when search is off so the
// prompt never advertises a tool the agent does not have.
func Prompt(c Config) string {
	if !c.Enabled() {
		return ""
	}
	extra := ""
	if c.Provider == ProviderBrowser {
		extra = "\n## Browser-provider rules\n" +
			"- This provider controls the user's configured local browser/profile. web_search returns ranked results (title, URL, snippet); open_url opens one URL and returns that page's text plus its in-page links and search forms.\n" +
			"- Use the external search engine only for discovery: to find the initial site, product page, documentation page, or entry point. Do not keep asking DuckDuckGo/Google/Bing what is on a site after the site has already been found.\n" +
			"- Once the target site/page is found, treat that site as the source of truth. open_url the page, then follow the links it lists or use the site's own UI/navigation/search/filter controls (open_url the relevant link; for a GET search form, open_url its action with ?field=YOUR+QUERY) rather than a site: query or another search-engine query.\n" +
			"- If the task is to understand controls, fields, filters, forms, menus, or other UI elements on a found site, open_url the page and read the links and forms it returns; do not substitute search-engine snippets for site interaction.\n" +
			"- If a captcha/human verification appears, aigem pauses in the same browser window and asks the user to solve it and click the confirmation overlay before continuing. Do not restart or bypass the browser.\n" +
			"- Treat the managed browser as the only network path for web pages found through this provider. Do NOT use bash, curl, Python, wget, or other direct HTTP requests to fetch search results or result pages. This rule is about browsing pages, not about the network in general: calling an HTTP API (a REST/JMAP/GraphQL endpoint, a service's own CLI) with bash or curl is allowed, and is the correct tool for an API - do not route an API call through the browser or report yourself blocked on one.\n" +
			"- Base answers on the text returned by web_search and open_url. If a page could not be read or the site's own controls are inaccessible, explain that limitation; refine the external web_search query only when you need to find a different entry point or another authoritative source.\n" +
			"- open_url only navigates (GET) and reads. When a task needs interaction - logging in (POST forms), typing, clicking, keyboard, a specific viewport (e.g. 375px vs desktop), hovering for tooltips, or inspecting the DOM - use browser_action: it runs an ordered list of steps in one session and reports DOM observations. Login persists to later calls via the profile's cookies. For credentials in a fill step, read them from wherever they live (a local file or the ticket) and type the value directly; internal app hosts must be in browser.test_hosts.\n"
	}
	return `# Web search

You have a web_search tool (provider: ` + c.Provider + `). Your training data has a cutoff and is
likely stale on fast-moving details. Treat web_search as the source of truth for "what is current":
- BEFORE you pick a package, a dependency version, an API, a CLI flag, or a config option - and
  whenever the user asks about the latest/current/recent state of anything - call web_search to
  confirm against up-to-date sources instead of answering from memory. Do not guess a version number.
- Good uses: latest stable release of a library, breaking changes between versions, a newly added
  API, current best practice, or an error message you do not recognize.
- Do NOT put a year, a date, or a specific version number into a query when you are asking for the
  latest/current/most-recent state of something. That value is exactly what you do not yet know, and
  pasting your remembered guess (e.g. "...2021 election results" or "Go 1.22 release") biases the
  search toward stale results. Query open-endedly ("latest Go stable version", "most recent Armenia
  election results") and read the actual date/version from the results. Only add a specific
  year/version to a follow-up query once the results have told you which one is current.
- ANSWER FROM THE RESULTS, not from memory. Once you have searched, read the result titles and
  snippets and base your answer on them. If a result contradicts what you remembered (e.g. it shows
  a newer version number), the result wins - report the value from the results, never substitute a
  number you recalled. If the results are unclear, refine the query rather than guessing.
- Do NOT search for things answerable from the code in front of you, or for the project's own
  internals. Search deliberately: one focused query, read the results, refine only if needed - do
  not spam it.
- Privacy: never include secrets, credentials, file contents, or the user's personal data in a
  query. Search for general facts, not the user's private information.` + extra
}
