package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/chromedp"
)

type browser struct {
	cfg       BrowserConfig
	automator func(context.Context, BrowserConfig, string, int) ([]Result, error)
}

func newBrowser(cfg *BrowserConfig) *browser {
	b := normalizeBrowserConfig(cfg)
	return &browser{cfg: b, automator: automateBrowserSearch}
}

func validateBrowserConfig(cfg *BrowserConfig) error {
	b := normalizeBrowserConfig(cfg)
	if b.Mode != BrowserModeInteractive {
		return fmt.Errorf("browser search mode %q is not supported; use %q", b.Mode, BrowserModeInteractive)
	}
	switch b.Engine {
	case BrowserEngineGoogle, BrowserEngineDuckDuckGo, BrowserEngineBing:
	default:
		return fmt.Errorf("unknown browser search engine %q (supported: google, duckduckgo, bing)", b.Engine)
	}
	return nil
}

func normalizeBrowserConfig(cfg *BrowserConfig) BrowserConfig {
	b := BrowserConfig{Engine: BrowserEngineDuckDuckGo, Mode: BrowserModeInteractive}
	if cfg != nil {
		b = *cfg
	}
	b.Engine = strings.ToLower(strings.TrimSpace(b.Engine))
	if b.Engine == "" {
		b.Engine = BrowserEngineDuckDuckGo
	}
	b.Mode = strings.ToLower(strings.TrimSpace(b.Mode))
	if b.Mode == "" {
		b.Mode = BrowserModeInteractive
	}
	return b
}

func (b *browser) Search(ctx context.Context, query string, count int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("empty search query")
	}
	if b.cfg.Mode != BrowserModeInteractive {
		return nil, fmt.Errorf("browser search mode %q is not supported; use %q", b.cfg.Mode, BrowserModeInteractive)
	}
	return b.automator(ctx, b.cfg, query, count)
}

func browserSearchURL(engine, query string, count int) (string, error) {
	if count <= 0 {
		count = 5
	}
	if count > 20 {
		count = 20
	}
	q := url.Values{}
	switch engine {
	case BrowserEngineGoogle:
		q.Set("q", query)
		q.Set("num", strconv.Itoa(count))
		return "https://www.google.com/search?" + q.Encode(), nil
	case BrowserEngineDuckDuckGo:
		q.Set("q", query)
		return "https://duckduckgo.com/?" + q.Encode(), nil
	case BrowserEngineBing:
		q.Set("q", query)
		q.Set("count", strconv.Itoa(count))
		return "https://www.bing.com/search?" + q.Encode(), nil
	default:
		return "", fmt.Errorf("unknown browser search engine %q (supported: google, duckduckgo, bing)", engine)
	}
}

// browserScrapeURL is the URL the automator loads to read results. DuckDuckGo's
// main page (browserSearchURL) renders results via JS and is hostile to
// automation, so for it we use the static no-JS HTML endpoint, whose result
// links and snippets are present in the served markup. Google and Bing already
// render results server-side, so their human-facing URL works for scraping too.
func browserScrapeURL(engine, query string, count int) (string, error) {
	switch engine {
	case BrowserEngineDuckDuckGo:
		q := url.Values{}
		q.Set("q", query)
		return "https://html.duckduckgo.com/html/?" + q.Encode(), nil
	case BrowserEngineGoogle, BrowserEngineBing:
		return browserSearchURL(engine, query, count)
	default:
		return "", fmt.Errorf("unknown browser search engine %q (supported: google, duckduckgo, bing)", engine)
	}
}

func clampCount(count, def, max int) int {
	if count <= 0 {
		return def
	}
	if count > max {
		return max
	}
	return count
}

type browserSerpEntry struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

type browserLink struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

type browserForm struct {
	Action string   `json:"action"`
	Method string   `json:"method"`
	Fields []string `json:"fields"`
	Search bool     `json:"search"`
}

type browserPageContent struct {
	Title   string        `json:"title"`
	Heading string        `json:"heading"`
	Text    string        `json:"text"`
	Links   []browserLink `json:"links"`
	Forms   []browserForm `json:"forms"`
}

type browserPageSignal struct {
	Title           string `json:"title"`
	URL             string `json:"url"`
	Text            string `json:"text"`
	HasCaptchaInput bool   `json:"hasCaptchaInput"`
	HasCaptchaFrame bool   `json:"hasCaptchaFrame"`
}

const chromeHeadlessUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"

// resolveProfileDir returns the configured profile dir, or the default one.
func resolveProfileDir(cfg BrowserConfig) (string, error) {
	if cfg.ProfileDir != "" {
		return cfg.ProfileDir, nil
	}
	return DefaultBrowserProfileDir()
}

func newChromeContext(ctx context.Context, cfg BrowserConfig) (context.Context, context.CancelFunc, error) {
	cfg = normalizeBrowserConfig(&cfg)
	dir, err := resolveProfileDir(cfg)
	if err != nil {
		return nil, nil, err
	}
	cfg.ProfileDir = dir
	if err := os.MkdirAll(cfg.ProfileDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create browser profile dir: %w", err)
	}
	exe := strings.TrimSpace(cfg.Executable)
	if exe == "" {
		var err error
		exe, err = detectChromeExecutable()
		if err != nil {
			return nil, nil, err
		}
	}
	// Run headless so the browser works on machines without a display. The default
	// headless user-agent advertises "HeadlessChrome", which DuckDuckGo/Bing flag as
	// an anomaly and answer with a bot-check page instead of results; override it with
	// a normal Chrome UA so the engines return real hits.
	//
	// Three flags override chromedp's DefaultExecAllocatorOptions (flags live in a
	// map, so re-setting a name wins):
	//   - disable-dev-shm-usage=false: the default true routes Chrome's shared
	//     memory to $TMPDIR instead of /dev/shm; on a tmpfs /tmp that dumps every
	//     search's renderer buffers into RAM. We run on a real host with a normal
	//     /dev/shm, so keep shm there.
	//   - enable-automation=false and disable-blink-features=AutomationControlled:
	//     the default enable-automation sets navigator.webdriver=true, which every
	//     anti-bot page keys on; both together stop Blink from exposing it.
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(exe),
		chromedp.UserDataDir(cfg.ProfileDir),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("user-agent", chromeHeadlessUserAgent),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("disable-dev-shm-usage", false),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)...)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx,
		chromedp.WithLogf(func(string, ...any) {}),
		chromedp.WithErrorf(func(string, ...any) {}),
	)
	return browserCtx, func() {
		cancelBrowser()
		cancelAlloc()
	}, nil
}

// startChrome allocates the managed browser and verifies it actually started,
// recovering from the two startup failures that repeatedly blocked unattended
// bots: a stale profile SingletonLock left by a crashed Chrome, and a corrupted
// profile that crashes Chrome on launch. It retries once after clearing stale
// singleton lock files, then falls back to a throwaway temporary profile (login
// state is lost for that call, but the tool works instead of failing the turn).
func startChrome(ctx context.Context, cfg BrowserConfig) (context.Context, context.CancelFunc, error) {
	// Serialize sessions that share this profile for the whole session, not just
	// launch: Chrome permits only one process per profile, so overlapping tool
	// calls on a bot's single persistent profile would race, and the loser gets
	// pushed onto a throwaway temporary profile - silently dropping the logged-in
	// session the tester relies on and leaving a stale SingletonLock behind. The
	// gate makes the second caller wait instead, so it reuses the same profile.
	dir, _ := resolveProfileDir(cfg)
	unlock, err := lockProfile(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	// The lock is released on every path that does not hand a live browser back,
	// and by the returned cancel when one is handed back.
	bctx, cancel, err := probeChrome(ctx, cfg)
	if err == nil {
		return bctx, withUnlock(cancel, unlock), nil
	}
	if ctx.Err() != nil {
		unlock()
		return nil, nil, err
	}
	if dir != "" && clearStaleProfileLocks(dir, browserLog()) {
		browserLog().Warn("browser failed to start; retrying after clearing stale profile locks", "err", err)
		if bctx, cancel, err = probeChrome(ctx, cfg); err == nil {
			return bctx, withUnlock(cancel, unlock), nil
		}
		if ctx.Err() != nil {
			unlock()
			return nil, nil, err
		}
	}
	tmp, terr := os.MkdirTemp("", "aigem-browser-*")
	if terr != nil {
		unlock()
		return nil, nil, err
	}
	// The temp profile shares nothing with the persistent one, so stop holding the
	// persistent profile lock here: concurrent callers should not queue behind a
	// throwaway session, and one of them may even get the persistent profile
	// working again.
	unlock()
	browserLog().Warn("browser still failing; falling back to a fresh temporary profile", "err", err, "profile", tmp)
	tmpCfg := cfg
	tmpCfg.ProfileDir = tmp
	bctx, cancel, err = probeChrome(ctx, tmpCfg)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return nil, nil, fmt.Errorf("browser failed to start even with a fresh profile: %w", err)
	}
	return bctx, func() {
		cancel()
		_ = os.RemoveAll(tmp)
	}, nil
}

// browserLog is the logger for browser warnings.
func browserLog() *slog.Logger {
	return slog.Default()
}

// profileGates holds one gate channel per resolved profile dir; profileGatesMu
// guards the map itself. Keying by dir means distinct or temporary profiles
// never contend - only calls sharing a profile are serialized.
var (
	profileGatesMu sync.Mutex
	profileGates   = map[string]chan struct{}{}
)

func profileGate(dir string) chan struct{} {
	profileGatesMu.Lock()
	defer profileGatesMu.Unlock()
	g := profileGates[dir]
	if g == nil {
		g = make(chan struct{}, 1)
		profileGates[dir] = g
	}
	return g
}

// lockProfile blocks until the profile is free or ctx ends, returning an unlock
// func that is safe to call exactly once. An empty dir is not gated.
func lockProfile(ctx context.Context, dir string) (func(), error) {
	if dir == "" {
		return func() {}, nil
	}
	g := profileGate(dir)
	select {
	case g <- struct{}{}:
		return func() { <-g }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func withUnlock(cancel context.CancelFunc, unlock func()) context.CancelFunc {
	return func() {
		cancel()
		unlock()
	}
}

// probeChrome builds the chromedp context and forces the browser process to
// launch (an empty Run allocates it), so a start failure surfaces here instead
// of on the first navigation.
func probeChrome(ctx context.Context, cfg BrowserConfig) (context.Context, context.CancelFunc, error) {
	bctx, cancel, err := newChromeContext(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := chromedp.Run(bctx); err != nil {
		cancel()
		return nil, nil, err
	}
	return bctx, cancel, nil
}

// clearStaleProfileLocks removes Chrome's singleton lock artifacts from the
// profile dir when their owner is provably dead, reporting whether anything was
// removed. Chrome refuses to reuse a profile whose previous owner crashed
// without removing them - but the same lock also guards a LIVE Chrome (e.g. a
// concurrent tool call on this profile), so the owner encoded in the
// SingletonLock symlink ("host-pid") must be checked before deleting; a live or
// unverifiable owner keeps its locks and the caller falls back to a temporary
// profile instead.
func clearStaleProfileLocks(profileDir string, log *slog.Logger) bool {
	lock := filepath.Join(profileDir, "SingletonLock")
	target, err := os.Readlink(lock)
	if err != nil {
		return false // no lock (or not a symlink): nothing safe to clear
	}
	if !lockOwnerDead(target) {
		return false
	}
	cleared := false
	for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		p := filepath.Join(profileDir, name)
		if err := os.Remove(p); err == nil {
			log.Info("removed stale browser profile lock", "path", p)
			cleared = true
		}
	}
	return cleared
}

// lockOwnerDead reports whether the "host-pid" owner of a Chrome SingletonLock
// is provably dead: same host, and the pid no longer accepts signal 0. Any
// doubt (foreign host, unparsable target, live pid) counts as alive.
func lockOwnerDead(target string) bool {
	i := strings.LastIndexByte(target, '-')
	if i < 0 {
		return false
	}
	host, pidStr := target[:i], target[i+1:]
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return false
	}
	if self, herr := os.Hostname(); herr != nil || self != host {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true // on unix FindProcess never fails; be permissive if it does
	}
	serr := proc.Signal(syscall.Signal(0))
	// Only "no such process" proves death: EPERM means a live process owned by
	// another user, and its locks must be left alone.
	return errors.Is(serr, os.ErrProcessDone) || errors.Is(serr, syscall.ESRCH)
}

// automateBrowserSearch loads the engine's results page in the managed browser
// and returns the ranked hits (title, URL, snippet). It deliberately does NOT
// open the result pages: reading a page is the open_url tool's job, so the agent
// can pick what to read and then navigate the chosen site itself.
func automateBrowserSearch(ctx context.Context, cfg BrowserConfig, query string, count int) ([]Result, error) {
	cfg = normalizeBrowserConfig(&cfg)
	count = clampCount(count, 5, 20)
	searchURL, err := browserScrapeURL(cfg.Engine, query, count)
	if err != nil {
		return nil, err
	}
	// Bound a stuck SERP load, while leaving room for the in-page captcha wait
	// (its own 5-minute budget) to be solved.
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	browserCtx, cancelBrowser, err := startChrome(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer cancelBrowser()

	var entries []browserSerpEntry
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(searchURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Sleep(1500*time.Millisecond),
	); err != nil {
		return nil, fmt.Errorf("browser search page: %w", err)
	}
	if err := waitForCaptchaIfPresent(browserCtx, "search results"); err != nil {
		return nil, err
	}
	if err := chromedp.Run(browserCtx, chromedp.Evaluate(browserSerpExtractionJS(cfg.Engine), &entries)); err != nil {
		return nil, fmt.Errorf("browser search results: %w", err)
	}

	results := filterSerpEntries(cfg.Engine, entries, count)
	if len(results) == 0 {
		return nil, fmt.Errorf("no browser search result links found")
	}
	return results, nil
}

func browserSerpExtractionJS(engine string) string {
	if engine == BrowserEngineDuckDuckGo {
		return `(() => Array.from(document.querySelectorAll('.result, .web-result')).map(r => {
			const a = r.querySelector('a.result__a, h2 a');
			const s = r.querySelector('.result__snippet');
			return {
				title: a ? (a.innerText || a.textContent || '').trim() : '',
				url: a ? (a.href || '') : '',
				snippet: s ? (s.innerText || s.textContent || '').replace(/\s+/g, ' ').trim() : ''
			};
		}).filter(x => x.title && x.url))()`
	}
	// Google/Bing markup shifts often, so fall back to harvesting anchors and
	// letting cleanBrowserResultURL drop the engine-internal ones; snippets are
	// not reliably attached to a result container here.
	return `(() => Array.from(document.querySelectorAll('a')).map(a => ({
		title: (a.innerText || a.textContent || '').trim(),
		url: a.href || '',
		snippet: ''
	})).filter(x => x.title && x.url))()`
}

func filterSerpEntries(engine string, entries []browserSerpEntry, count int) []Result {
	count = clampCount(count, 5, 20)
	var out []Result
	seen := map[string]bool{}
	for _, e := range entries {
		u, err := cleanBrowserResultURL(engine, e.URL)
		if err != nil || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, Result{Title: strings.TrimSpace(e.Title), URL: u, Description: strings.TrimSpace(e.Snippet)})
		if len(out) >= count {
			break
		}
	}
	return out
}

func cleanBrowserResultURL(engine, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("bad url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme")
	}
	host := strings.ToLower(u.Hostname())
	switch engine {
	case BrowserEngineGoogle:
		if strings.Contains(host, "google.") {
			if u.Path == "/url" || u.Path == "/imgres" {
				if q := u.Query().Get("q"); q != "" {
					return cleanBrowserResultURL("", q)
				}
			}
			return "", fmt.Errorf("google internal")
		}
	case BrowserEngineDuckDuckGo:
		if strings.Contains(host, "duckduckgo.") {
			if q := u.Query().Get("uddg"); q != "" {
				return cleanBrowserResultURL("", q)
			}
			return "", fmt.Errorf("duckduckgo internal")
		}
	case BrowserEngineBing:
		if strings.Contains(host, "bing.") || strings.Contains(host, "microsoft.") {
			return "", fmt.Errorf("bing internal")
		}
	}
	return u.String(), nil
}

// openBrowserPage navigates the managed browser to target and returns the page's
// extracted text plus its links and search forms. It backs the open_url tool,
// which is how the agent reads a found page and drives a site's own navigation.
func openBrowserPage(ctx context.Context, cfg BrowserConfig, target string) (Result, error) {
	cfg = normalizeBrowserConfig(&cfg)
	u, err := url.Parse(strings.TrimSpace(target))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Result{}, fmt.Errorf("open_url needs an absolute http(s) URL, got %q", target)
	}
	if reason := internalAddressReason(u.Hostname()); reason != "" {
		return Result{}, fmt.Errorf("open_url refused %q: %s; only public web URLs are allowed", target, reason)
	}
	// Generous bound: a stuck load is killed, but a real captcha (handled inside
	// readBrowserPage with its own 5-minute budget) still has room to be solved.
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	browserCtx, cancelBrowser, err := startChrome(ctx, cfg)
	if err != nil {
		return Result{}, err
	}
	defer cancelBrowser()
	res, err := readBrowserPage(browserCtx, u.String(), "")
	if err != nil {
		return Result{}, err
	}
	// A public URL can redirect into an internal address; re-check the landing
	// host and withhold the content rather than hand it back to the model.
	if fu, perr := url.Parse(res.URL); perr == nil {
		if reason := internalAddressReason(fu.Hostname()); reason != "" {
			return Result{}, fmt.Errorf("open_url refused: %q redirected to an internal address (%s)", target, reason)
		}
	}
	return res, nil
}

// internalAddressReason returns a non-empty reason when host points at a
// non-public destination (loopback, link-local incl. cloud metadata
// 169.254.169.254, RFC1918/ULA private ranges, or an internal hostname), so
// open_url cannot be steered into the user's intranet or local services. It
// resolves hostnames; an unresolvable host is allowed through (the browser load
// will just fail, and any redirect is re-checked against the landing URL).
func internalAddressReason(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return "empty host"
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") ||
		strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal") {
		return "internal hostname"
	}
	if ip := net.ParseIP(h); ip != nil {
		return internalIPReason(ip)
	}
	ips, err := net.LookupIP(h)
	if err != nil {
		return ""
	}
	for _, ip := range ips {
		if reason := internalIPReason(ip); reason != "" {
			return reason
		}
	}
	return ""
}

func internalIPReason(ip net.IP) string {
	switch {
	case ip.IsLoopback():
		return "loopback address"
	case ip.IsUnspecified():
		return "unspecified address"
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return "link-local address"
	case ip.IsPrivate():
		return "private address"
	default:
		return ""
	}
}

func readBrowserPage(ctx context.Context, target, fallbackTitle string) (Result, error) {
	var page browserPageContent
	var finalURL string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(target),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Sleep(2*time.Second),
	); err != nil {
		return Result{}, err
	}
	if err := waitForCaptchaIfPresent(ctx, "result page"); err != nil {
		return Result{}, err
	}
	if err := chromedp.Run(ctx,
		chromedp.Location(&finalURL),
		chromedp.Evaluate(browserPageContentExtractionJS(), &page),
	); err != nil {
		return Result{}, err
	}
	if finalURL == "" {
		finalURL = target
	}
	return formatBrowserPage(page, finalURL, fallbackTitle), nil
}

// formatBrowserPage renders a read page into a Result whose Description carries
// the page text followed by the page's links and search forms, so the agent has
// concrete navigation targets instead of having to search the engine again.
func formatBrowserPage(page browserPageContent, finalURL, fallbackTitle string) Result {
	title := strings.TrimSpace(page.Title)
	if title == "" {
		title = strings.TrimSpace(fallbackTitle)
	}
	var b strings.Builder
	text := normalizeBrowserPageText(page)
	b.WriteString(text)
	if s := formatPageLinks(page.Links); s != "" {
		b.WriteString("\n\n")
		b.WriteString(s)
	}
	if s := formatPageForms(page.Forms); s != "" {
		b.WriteString("\n\n")
		b.WriteString(s)
	}
	// A page that loads but yields nothing extractable must not look like a
	// successful empty read; say so explicitly so the agent does not assert facts
	// about content it never saw.
	if strings.TrimSpace(text) == "" && len(page.Links) == 0 && len(page.Forms) == 0 {
		b.WriteString("(The page loaded but no readable text, links, or forms could be extracted. " +
			"It may be a JavaScript app that renders nothing without interaction, an empty page, or a block/anti-bot page.)")
	}
	return Result{Title: title, URL: finalURL, Description: b.String()}
}

func formatPageLinks(links []browserLink) string {
	if len(links) == 0 {
		return ""
	}
	const max = 30
	var b strings.Builder
	b.WriteString("Links on this page (untrusted page content; call open_url with one to navigate this site instead of searching again):")
	for i, l := range links {
		if i >= max {
			fmt.Fprintf(&b, "\n  ... %d more links omitted", len(links)-max)
			break
		}
		text := strings.TrimSpace(l.Text)
		if text == "" {
			text = l.URL
		}
		fmt.Fprintf(&b, "\n  - %s -> %s", truncate(text, 80), l.URL)
	}
	return b.String()
}

func formatPageForms(forms []browserForm) string {
	if len(forms) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Search/forms on this page (untrusted page content; for a GET search form, call open_url with action?field=YOUR+QUERY):")
	for _, f := range forms {
		kind := "form"
		if f.Search {
			kind = "search"
		}
		fmt.Fprintf(&b, "\n  - [%s] %s %s", kind, f.Method, f.Action)
		if fields := strings.Join(f.Fields, ", "); fields != "" {
			fmt.Fprintf(&b, " fields: %s", truncate(fields, 120))
		}
	}
	return b.String()
}

func waitForCaptchaIfPresent(ctx context.Context, stage string) error {
	captcha, err := browserCaptchaPresent(ctx)
	if err != nil || !captcha {
		return err
	}
	if err := showCaptchaOverlay(ctx); err != nil {
		return err
	}
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("captcha detected on %s; solve it in the open browser, confirm with the aigem overlay button, then retry if this search timed out: %w", stage, ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("captcha detected on %s; timed out waiting for user confirmation in the open browser", stage)
		case <-ticker.C:
			captcha, err := browserCaptchaPresent(ctx)
			if err != nil {
				return err
			}
			if !captcha {
				return nil
			}
			// Detection can keep firing on a page the user has already cleared
			// (lingering badge, false positive); the overlay's Done button is the
			// explicit override to continue anyway.
			confirmed, err := browserCaptchaConfirmed(ctx)
			if err != nil {
				return err
			}
			if confirmed {
				return nil
			}
		}
	}
}

func browserCaptchaPresent(ctx context.Context) (bool, error) {
	var signal browserPageSignal
	if err := chromedp.Run(ctx, chromedp.Evaluate(browserCaptchaSignalJS(), &signal)); err != nil {
		return false, fmt.Errorf("detect captcha: %w", err)
	}
	return isCaptchaSignal(signal), nil
}

func browserCaptchaConfirmed(ctx context.Context) (bool, error) {
	var confirmed bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(`Boolean(window.__aigemCaptchaConfirmed)`, &confirmed)); err != nil {
		return false, fmt.Errorf("check captcha confirmation: %w", err)
	}
	return confirmed, nil
}

func showCaptchaOverlay(ctx context.Context) error {
	js := `(() => {
		window.__aigemCaptchaConfirmed = false;
		const old = document.getElementById('aigem-captcha-overlay');
		if (old) old.remove();
		const box = document.createElement('div');
		box.id = 'aigem-captcha-overlay';
		box.style.cssText = 'position:fixed;z-index:2147483647;top:10px;right:10px;max-width:280px;background:#111827;color:white;border:1px solid #f59e0b;border-radius:10px;padding:10px 12px;font:13px/1.3 system-ui,-apple-system,BlinkMacSystemFont,sans-serif;box-shadow:0 10px 30px rgba(0,0,0,.28)';
		box.innerHTML = '<div style="font-weight:700;margin-bottom:6px">aigem: captcha?</div>' +
			'<div style="margin-bottom:8px">Solve it, then confirm.</div>' +
			'<button id="aigem-captcha-confirm" style="background:#f59e0b;color:#111827;border:0;border-radius:7px;padding:6px 9px;font-weight:700;cursor:pointer;font-size:12px">Done</button>';
		document.documentElement.appendChild(box);
		document.getElementById('aigem-captcha-confirm').addEventListener('click', () => {
			window.__aigemCaptchaConfirmed = true;
			box.querySelector('div:nth-child(2)').textContent = 'Checking...';
		});
	})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, nil)); err != nil {
		return fmt.Errorf("show captcha prompt: %w", err)
	}
	return nil
}

func browserCaptchaSignalJS() string {
	return `(() => {
		const text = (document.body ? document.body.innerText : '').slice(0, 20000);
		const isVisible = (el) => {
			const r = el.getBoundingClientRect();
			const s = getComputedStyle(el);
			return r.width > 20 && r.height > 20 && s.visibility !== 'hidden' && s.display !== 'none' && Number(s.opacity || '1') > 0.05;
		};
		const activeFrames = Array.from(document.querySelectorAll('iframe')).filter(f => {
			const src = f.src || '';
			const title = f.title || '';
			const r = f.getBoundingClientRect();
			if (!/captcha|recaptcha|hcaptcha|turnstile|challenges\.cloudflare\.com/i.test(src + ' ' + title)) return false;
			if (!isVisible(f)) return false;
			// Ignore passive Google reCAPTCHA policy badges; they are usually small
			// bottom-corner overlays and do not require user action. Do not ignore
			// similarly sized checkbox challenges when they are centered in the page.
			const bottomCornerBadge = r.width <= 320 && r.height <= 120 && r.left >= window.innerWidth - 380 && r.top >= window.innerHeight - 180;
			if (/recaptcha/i.test(src + ' ' + title) && bottomCornerBadge) return false;
			return true;
		});
		const inputs = Array.from(document.querySelectorAll('input,textarea')).filter(isVisible).map(i => (i.name || '') + ' ' + (i.id || '') + ' ' + (i.placeholder || '')).join(' ');
		return {
			title: document.title || '',
			url: location.href || '',
			text,
			hasCaptchaInput: /captcha|g-recaptcha|h-captcha|cf-turnstile|verification/i.test(inputs),
			hasCaptchaFrame: activeFrames.length > 0
		};
	})()`
}

func isCaptchaSignal(s browserPageSignal) bool {
	if s.HasCaptchaInput || s.HasCaptchaFrame {
		return true
	}
	haystack := strings.ToLower(strings.Join([]string{s.Title, s.URL, s.Text}, "\n"))
	haystack = strings.ReplaceAll(haystack, "this site is protected by recaptcha and the google privacy policy and terms of service apply", "")
	haystack = strings.ReplaceAll(haystack, "protected by recaptcha", "")
	patterns := []string{
		"hcaptcha", "cf-turnstile", "cloudflare challenge", "complete the recaptcha", "solve the recaptcha",
		"verify you are human", "verify that you are human", "checking if the site connection is secure",
		"unusual traffic", "our systems have detected unusual traffic", "detected unusual traffic",
		"are you a robot", "not a robot", "human verification", "security check",
		"complete the security check", "attention required", "challenge-platform",
	}
	for _, p := range patterns {
		if strings.Contains(haystack, p) {
			return true
		}
	}
	return false
}

func browserPageContentExtractionJS() string {
	return `(() => { try {
		const clone = document.body ? document.body.cloneNode(true) : document.createElement('body');
		clone.querySelectorAll('script,style,noscript,svg,canvas,iframe,form,input,button,select,textarea,nav,footer,aside,[role="navigation"],[role="banner"],[role="contentinfo"],[aria-hidden="true"],.nav,.navbar,.menu,.sidebar,.footer,.cookie,.cookies,.popup,.modal,.advertisement,.ads,.social,.share').forEach(n => n.remove());
		const visibleText = (el) => (el.innerText || el.textContent || '').replace(/\s+/g, ' ').trim();
		const score = (el) => {
			const tag = el.tagName ? el.tagName.toLowerCase() : '';
			let s = visibleText(el).length;
			if (['article','main'].includes(tag)) s += 2500;
			if (['section'].includes(tag)) s += 400;
			if (/content|article|post|product|main|body|entry/i.test(el.id + ' ' + el.className)) s += 900;
			if (/nav|menu|sidebar|footer|header|cookie|modal|popup|comment|related|recommend|advert|ads/i.test(el.id + ' ' + el.className)) s -= 2000;
			const links = el.querySelectorAll ? el.querySelectorAll('a').length : 0;
			if (links > 0 && s / Math.max(links, 1) < 80) s -= links * 120;
			return s;
		};
		const candidates = Array.from(clone.querySelectorAll('article,main,[role="main"],section,.content,.article,.post,.entry,.product,body'));
		let best = candidates[0] || clone;
		for (const el of candidates) if (score(el) > score(best)) best = el;
		let lines = [];
		const blocks = best.querySelectorAll('h1,h2,h3,p,li,td,th,blockquote,pre');
		for (const el of blocks) {
			const t = visibleText(el);
			if (t.length < 2) continue;
			lines.push(t);
		}
		if (lines.join(' ').length < 300) lines = visibleText(best).split(/\n+/).map(s => s.trim()).filter(Boolean);
		// Links and forms come from the live document, not the text-stripped clone,
		// so the agent can navigate the site or drive its own search/filters.
		const abs = (h) => { try { return new URL(h, location.href).href; } catch (e) { return ''; } };
		const links = [];
		const seenL = new Set();
		for (const a of Array.from(document.querySelectorAll('a[href]'))) {
			const t = (a.innerText || a.textContent || '').replace(/\s+/g, ' ').trim();
			const u = a.href || '';
			if (!t || !/^https?:/i.test(u) || seenL.has(u)) continue;
			seenL.add(u);
			links.push({ text: t.slice(0, 120), url: u });
			if (links.length >= 60) break;
		}
		const forms = [];
		for (const f of Array.from(document.querySelectorAll('form'))) {
			const fields = Array.from(f.querySelectorAll('input,select,textarea'))
				.map(i => (i.name || '').trim()).filter(Boolean);
			const isSearch = !!f.querySelector('input[type="search"]') ||
				fields.some(n => /^(q|s|query|search|keyword|term|wd|text)$/i.test(n)) ||
				/search/i.test((f.id || '') + ' ' + (f.className || '') + ' ' + (f.getAttribute('role') || ''));
			forms.push({
				action: abs(f.getAttribute('action') || location.href),
				method: (f.method || 'get').toUpperCase(),
				fields: fields.slice(0, 15),
				search: isSearch
			});
			if (forms.length >= 10) break;
		}
		return {
			title: (document.title || '').trim(),
			heading: (document.querySelector('h1')?.innerText || '').trim(),
			text: lines.join('\n'),
			links,
			forms
		};
	} catch (e) {
		return { title: (document.title || '').trim(), heading: '', text: '', links: [], forms: [] };
	} })()`
}

func normalizeBrowserPageText(page browserPageContent) string {
	var lines []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
		if s == "" || len([]rune(s)) < 2 {
			return
		}
		key := strings.ToLower(s)
		if seen[key] {
			return
		}
		seen[key] = true
		lines = append(lines, s)
	}
	add(page.Heading)
	for _, l := range strings.Split(page.Text, "\n") {
		add(l)
	}
	text := strings.Join(lines, "\n")
	const max = 4000
	if len(text) > max {
		text = text[:max] + "..."
	}
	return text
}

// WarmBrowserProfile opens a lightweight page in the configured isolated browser
// profile so the user can complete any first-run Chrome profile prompts during
// setup, before the agent later calls web_search under a turn timeout.
func WarmBrowserProfile(ctx context.Context, cfg BrowserConfig) error {
	if err := validateBrowserConfig(&cfg); err != nil {
		return err
	}
	ctx, cancel, err := newChromeContext(ctx, cfg)
	if err != nil {
		return err
	}
	defer cancel()
	return chromedp.Run(ctx,
		chromedp.Navigate("about:blank"),
		chromedp.Sleep(20*time.Second),
	)
}

func detectChromeExecutable() (string, error) {
	for _, name := range chromeExecutableCandidates() {
		if filepath.IsAbs(name) {
			if info, err := os.Stat(name); err == nil && !info.IsDir() {
				return name, nil
			}
			continue
		}
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("Chrome/Chromium executable not found; install Chrome/Chromium or configure browser.executable with `aigem search set browser --executable PATH`")
}

func chromeExecutableCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"google-chrome", "chromium", "chromium-browser",
		}
	case "windows":
		var out []string
		for _, base := range []string{os.Getenv("LOCALAPPDATA"), os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)")} {
			if base == "" {
				continue
			}
			out = append(out,
				filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(base, "Chromium", "Application", "chrome.exe"),
			)
		}
		return append(out, "chrome.exe", "chromium.exe")
	default:
		return []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "brave-browser"}
	}
}
