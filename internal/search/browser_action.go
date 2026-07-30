package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gigovich/aigem/internal/config"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// Interactive browser actions. Unlike open_url (navigate + read), these drive the
// managed browser: fill, click, submit (POST), keyboard, viewport, hover, and DOM
// observation - what a tester needs to log in and verify a page. Each call is one
// ephemeral session; login and server state carry across calls through the
// persistent profile's cookies, so a POST login in one call authenticates later
// ones.
const (
	actNavigate    = "navigate"
	actSetViewport = "set_viewport"
	actFill        = "fill"
	actClick       = "click"
	actPressKey    = "press_key"
	actHover       = "hover"
	actWaitFor     = "wait_for"
	actWait        = "wait"
)

const maxActionSteps = 50

type browserStep struct {
	Action    string `json:"action"`
	URL       string `json:"url,omitempty"`
	Selector  string `json:"selector,omitempty"`
	Text      string `json:"text,omitempty"`
	Key       string `json:"key,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
	MS        int    `json:"ms,omitempty"`
}

type browserQuery struct {
	Name     string   `json:"name"`
	Selector string   `json:"selector"`
	Attrs    []string `json:"attrs,omitempty"`
	Computed []string `json:"computed,omitempty"`
}

type browserObserve struct {
	Text          bool           `json:"text,omitempty"`
	ActiveElement bool           `json:"active_element,omitempty"`
	Query         []browserQuery `json:"query,omitempty"`
	Screenshot    bool           `json:"screenshot,omitempty"`
}

type browserActionRequest struct {
	Steps   []browserStep  `json:"steps"`
	Observe browserObserve `json:"observe,omitempty"`
}

type activeElementInfo struct {
	Tag      string `json:"tag"`
	ID       string `json:"id"`
	Selector string `json:"selector"`
	Name     string `json:"name"`
}

type queryResult struct {
	Exists   bool              `json:"exists"`
	Count    int               `json:"count"`
	Text     string            `json:"text"`
	Attrs    map[string]string `json:"attrs"`
	Computed map[string]string `json:"computed"`
}

type namedQuery struct {
	Name     string
	Selector string
	Result   queryResult
}

type actionObservation struct {
	URL            string
	Title          string
	Text           string
	HasActive      bool
	Active         activeElementInfo
	Queries        []namedQuery
	ScreenshotPath string
}

// runBrowserActions executes the step sequence in one managed-browser session,
// then reports the requested observations. Navigate hosts are validated before
// launching Chrome so a refused target fails fast.
func runBrowserActions(ctx context.Context, cfg BrowserConfig, req browserActionRequest) (string, error) {
	cfg = normalizeBrowserConfig(&cfg)
	if err := validateActionRequest(&req); err != nil {
		return "", err
	}
	for _, s := range req.Steps {
		if s.Action == actNavigate {
			if reason := actionURLReason(s.URL, cfg); reason != "" {
				return "", fmt.Errorf("browser_action refused %q: %s", s.URL, reason)
			}
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	bctx, cancelBrowser, err := startChrome(ctx, cfg)
	if err != nil {
		return "", err
	}
	defer cancelBrowser()

	for i, s := range req.Steps {
		if err := execStep(bctx, s); err != nil {
			return "", fmt.Errorf("step %d (%s): %w", i+1, s.Action, err)
		}
	}
	// A navigate can redirect, and a click/submit can land, on a host the caller
	// never named. Re-check the final host before reading anything, so the tester
	// cannot be steered onto internal infra outside test_hosts.
	var landing string
	if err := chromedp.Run(bctx, chromedp.Location(&landing)); err != nil {
		return "", err
	}
	if reason := landingHostReason(landing, cfg); reason != "" {
		return "", fmt.Errorf("browser_action refused to read %q: %s", landing, reason)
	}
	obs, err := observePage(bctx, req.Observe)
	if err != nil {
		return "", err
	}
	return formatActionResult(obs), nil
}

// landingHostReason guards the page the observation will read, mirroring
// open_url's post-redirect re-check. An http(s) internal host must be in
// TestHosts. Inert non-web landings (about:, data:, empty) are not a network
// read and pass; any other scheme (e.g. file:) is rejected rather than allowed
// by omission.
func landingHostReason(target string, cfg BrowserConfig) string {
	u, err := url.Parse(strings.TrimSpace(target))
	if err != nil {
		return "unreadable landing URL"
	}
	switch u.Scheme {
	case "http", "https":
		host := u.Hostname()
		if internalAddressReason(host) != "" && !testHostAllowed(host, cfg) {
			return "landed on an internal host not in browser.test_hosts"
		}
		return ""
	case "about", "data", "":
		return ""
	default:
		return fmt.Sprintf("landed on an unsupported scheme %q", u.Scheme)
	}
}

// validateActionRequest rejects malformed requests before any browser launch:
// unknown actions and missing per-action fields become clear errors instead of
// opaque chromedp failures mid-run.
func validateActionRequest(req *browserActionRequest) error {
	if len(req.Steps) == 0 {
		return fmt.Errorf("steps is required and must not be empty")
	}
	if len(req.Steps) > maxActionSteps {
		return fmt.Errorf("too many steps (%d); max is %d", len(req.Steps), maxActionSteps)
	}
	for i, s := range req.Steps {
		if err := validateStep(s); err != nil {
			return fmt.Errorf("step %d (%s): %w", i+1, s.Action, err)
		}
	}
	seen := map[string]bool{}
	for _, q := range req.Observe.Query {
		if strings.TrimSpace(q.Name) == "" {
			return fmt.Errorf("observe.query: each entry needs a name")
		}
		if strings.TrimSpace(q.Selector) == "" {
			return fmt.Errorf("observe.query %q: selector is required", q.Name)
		}
		if seen[q.Name] {
			return fmt.Errorf("observe.query: duplicate name %q", q.Name)
		}
		seen[q.Name] = true
	}
	return nil
}

func validateStep(s browserStep) error {
	switch s.Action {
	case actNavigate:
		if strings.TrimSpace(s.URL) == "" {
			return fmt.Errorf("url is required")
		}
	case actSetViewport:
		if s.Width <= 0 || s.Height <= 0 {
			return fmt.Errorf("width and height must be positive")
		}
	case actFill, actClick, actHover, actWaitFor:
		if strings.TrimSpace(s.Selector) == "" {
			return fmt.Errorf("selector is required")
		}
	case actPressKey:
		if _, ok := keySequence(s.Key); !ok {
			return fmt.Errorf("unknown or missing key %q", s.Key)
		}
	case actWait:
		if s.MS <= 0 {
			return fmt.Errorf("ms must be positive")
		}
	default:
		return fmt.Errorf("unknown action %q", s.Action)
	}
	return nil
}

// actionURLReason returns why a navigate target is refused, or "" if allowed. It
// mirrors open_url's public-only guard but lets the tester reach hosts listed in
// TestHosts, which is how it reaches the internal app under test.
func actionURLReason(target string, cfg BrowserConfig) string {
	u, err := url.Parse(strings.TrimSpace(target))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "needs an absolute http(s) URL"
	}
	host := u.Hostname()
	if reason := internalAddressReason(host); reason != "" {
		if testHostAllowed(host, cfg) {
			return ""
		}
		return reason + "; add it to browser.test_hosts to allow the tester to reach it"
	}
	return ""
}

func testHostAllowed(host string, cfg BrowserConfig) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, h := range cfg.TestHosts {
		if strings.ToLower(strings.TrimSpace(h)) == host {
			return true
		}
	}
	return false
}

// keySequence maps a friendly key name to the sequence chromedp.KeyEvent sends.
func keySequence(name string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "tab":
		return kb.Tab, true
	case "enter", "return":
		return kb.Enter, true
	case "escape", "esc":
		return kb.Escape, true
	case "arrowdown", "down":
		return kb.ArrowDown, true
	case "arrowup", "up":
		return kb.ArrowUp, true
	case "home":
		return kb.Home, true
	case "end":
		return kb.End, true
	case "space":
		return " ", true
	default:
		return "", false
	}
}

func execStep(ctx context.Context, s browserStep) error {
	switch s.Action {
	case actNavigate:
		if err := chromedp.Run(ctx,
			chromedp.Navigate(s.URL),
			chromedp.WaitReady("body", chromedp.ByQuery),
			chromedp.Sleep(1500*time.Millisecond),
		); err != nil {
			return err
		}
		return waitForCaptchaIfPresent(ctx, "browser_action page")
	case actSetViewport:
		return chromedp.Run(ctx, chromedp.EmulateViewport(int64(s.Width), int64(s.Height)))
	case actFill:
		// Clear first so fill replaces rather than appends. Not chromedp.Clear: it
		// hard-rejects anything but <input>/<textarea>, which would break fills on
		// contenteditable and custom components that SendKeys otherwise handles.
		if err := chromedp.Run(ctx, chromedp.WaitVisible(s.Selector, chromedp.ByQuery)); err != nil {
			return err
		}
		if err := clearField(ctx, s.Selector); err != nil {
			return err
		}
		return chromedp.Run(ctx, chromedp.SendKeys(s.Selector, s.Text, chromedp.ByQuery))
	case actClick:
		return chromedp.Run(ctx,
			chromedp.WaitVisible(s.Selector, chromedp.ByQuery),
			chromedp.Click(s.Selector, chromedp.ByQuery),
		)
	case actPressKey:
		seq, _ := keySequence(s.Key)
		if s.Selector != "" {
			return chromedp.Run(ctx,
				chromedp.WaitVisible(s.Selector, chromedp.ByQuery),
				chromedp.Focus(s.Selector, chromedp.ByQuery),
				chromedp.KeyEvent(seq),
			)
		}
		return chromedp.Run(ctx, chromedp.KeyEvent(seq))
	case actHover:
		return hoverSelector(ctx, s.Selector)
	case actWaitFor:
		c, cancel := context.WithTimeout(ctx, stepTimeout(s.TimeoutMS))
		defer cancel()
		if err := chromedp.Run(c, chromedp.WaitVisible(s.Selector, chromedp.ByQuery)); err != nil {
			return fmt.Errorf("selector %q did not appear: %w", s.Selector, err)
		}
		return nil
	case actWait:
		return chromedp.Run(ctx, chromedp.Sleep(time.Duration(s.MS)*time.Millisecond))
	}
	return fmt.Errorf("unknown action %q", s.Action)
}

func stepTimeout(ms int) time.Duration {
	if ms <= 0 {
		return 10 * time.Second
	}
	return time.Duration(ms) * time.Millisecond
}

// clearField empties a form control before fill types into it, handling the
// element kinds SendKeys accepts: <input>/<textarea> (via the prototype's native
// value setter so React-controlled inputs register the change) and contenteditable.
// Other elements are left as-is; the input/change events still fire.
func clearField(ctx context.Context, selector string) error {
	sel, _ := json.Marshal(selector)
	js := fmt.Sprintf(`(function(){
  var el = document.querySelector(%s);
  if(!el) return false;
  if('value' in el){
    var d = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(el), 'value');
    if(d && d.set){ d.set.call(el, ''); } else { el.value = ''; }
  } else if(el.isContentEditable){
    el.textContent = '';
  }
  el.dispatchEvent(new Event('input', {bubbles:true}));
  el.dispatchEvent(new Event('change', {bubbles:true}));
  return true;
})()`, sel)
	var ok bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &ok)); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("selector %q not found", selector)
	}
	return nil
}

// hoverSelector dispatches pointer/mouse events at the element's centre so
// JavaScript-driven tooltips fire. CSS :hover-only effects are not triggered by
// synthetic events; those need a real pointer move and are out of scope here.
func hoverSelector(ctx context.Context, selector string) error {
	if err := chromedp.Run(ctx, chromedp.WaitVisible(selector, chromedp.ByQuery)); err != nil {
		return err
	}
	sel, _ := json.Marshal(selector)
	js := fmt.Sprintf(`(function(){
  var el = document.querySelector(%s);
  if(!el) return false;
  var r = el.getBoundingClientRect();
  var o = {bubbles:true, cancelable:true, clientX:r.left+r.width/2, clientY:r.top+r.height/2};
  ['pointerover','mouseover','pointerenter','mouseenter','mousemove'].forEach(function(t){
    el.dispatchEvent(new MouseEvent(t, o));
  });
  return true;
})()`, sel)
	var ok bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &ok)); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("selector %q not found", selector)
	}
	return nil
}

func observePage(ctx context.Context, obs browserObserve) (actionObservation, error) {
	var o actionObservation
	if err := chromedp.Run(ctx, chromedp.Location(&o.URL), chromedp.Title(&o.Title)); err != nil {
		return o, err
	}
	if obs.Text {
		var page browserPageContent
		if err := chromedp.Run(ctx, chromedp.Evaluate(browserPageContentExtractionJS(), &page)); err != nil {
			return o, err
		}
		o.Text = normalizeBrowserPageText(page)
	}
	if obs.ActiveElement {
		if err := chromedp.Run(ctx, chromedp.Evaluate(activeElementJS(), &o.Active)); err != nil {
			return o, err
		}
		o.HasActive = true
	}
	for _, q := range obs.Query {
		var qr queryResult
		if err := chromedp.Run(ctx, chromedp.Evaluate(queryJS(q), &qr)); err != nil {
			return o, err
		}
		o.Queries = append(o.Queries, namedQuery{Name: q.Name, Selector: q.Selector, Result: qr})
	}
	if obs.Screenshot {
		var buf []byte
		if err := chromedp.Run(ctx, chromedp.FullScreenshot(&buf, 90)); err != nil {
			return o, err
		}
		path, err := saveScreenshot(buf)
		if err != nil {
			return o, err
		}
		o.ScreenshotPath = path
	}
	return o, nil
}

func activeElementJS() string {
	return `(function(){
  var e = document.activeElement;
  if(!e || e === document.body) return {tag:"", id:"", selector:"", name:""};
  var sel = e.tagName.toLowerCase();
  if(e.id) sel += "#" + e.id;
  if(typeof e.className === "string" && e.className.trim()) sel += "." + e.className.trim().split(/\s+/).join(".");
  return {tag:e.tagName.toLowerCase(), id:e.id||"", selector:sel, name:e.getAttribute("name")||""};
})()`
}

func queryJS(q browserQuery) string {
	sel, _ := json.Marshal(q.Selector)
	attrs, _ := json.Marshal(q.Attrs)
	computed, _ := json.Marshal(q.Computed)
	return fmt.Sprintf(`(function(){
  var els = Array.prototype.slice.call(document.querySelectorAll(%s));
  var first = els[0] || null;
  var out = {exists: els.length > 0, count: els.length, text: "", attrs:{}, computed:{}};
  if(first){
    out.text = (first.innerText || first.textContent || "").trim().slice(0, 500);
    (%s || []).forEach(function(a){ out.attrs[a] = first.getAttribute(a); });
    var cs = (%s || []).length ? getComputedStyle(first) : null;
    if(cs){ (%s).forEach(function(c){ out.computed[c] = cs.getPropertyValue(c); }); }
  }
  return out;
})()`, sel, attrs, computed, computed)
}

// saveScreenshot writes the PNG under the aigem state dir rather than $TMPDIR:
// on this host /tmp is a tmpfs under a user quota, and a full-page PNG there can
// hit EDQUOT and fail the whole call. Falls back to a temp file if the state dir
// is unavailable.
func saveScreenshot(buf []byte) (string, error) {
	dir, err := screenshotDir()
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "shot-*.png")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(buf); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func screenshotDir() (string, error) {
	state, err := config.StateDir()
	if err != nil {
		return os.TempDir(), nil
	}
	dir := filepath.Join(state, "screenshots")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create screenshot dir: %w", err)
	}
	return dir, nil
}

// formatActionResult renders the observation as text for the agent: DOM-oriented
// facts (page text, focused element, per-selector results) rather than an image,
// since the model reads text.
func formatActionResult(o actionObservation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "URL: %s\n", o.URL)
	if o.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", o.Title)
	}
	if o.HasActive {
		if o.Active.Selector == "" {
			b.WriteString("Active element: (none)\n")
		} else {
			fmt.Fprintf(&b, "Active element: %s", o.Active.Selector)
			if o.Active.Name != "" {
				fmt.Fprintf(&b, " (name=%s)", o.Active.Name)
			}
			b.WriteString("\n")
		}
	}
	if len(o.Queries) > 0 {
		b.WriteString("\nQuery results:\n")
		for _, q := range o.Queries {
			fmt.Fprintf(&b, "  - %s: exists=%t count=%d", q.Name, q.Result.Exists, q.Result.Count)
			if q.Result.Text != "" {
				fmt.Fprintf(&b, " text=%q", truncate(q.Result.Text, 120))
			}
			for _, k := range sortedKeys(q.Result.Attrs) {
				fmt.Fprintf(&b, " %s=%q", k, q.Result.Attrs[k])
			}
			for _, k := range sortedKeys(q.Result.Computed) {
				fmt.Fprintf(&b, " %s=%s", k, q.Result.Computed[k])
			}
			b.WriteString("\n")
		}
	}
	if o.ScreenshotPath != "" {
		fmt.Fprintf(&b, "\nScreenshot saved: %s\n", o.ScreenshotPath)
	}
	if o.Text != "" {
		fmt.Fprintf(&b, "\nPage text:\n%s\n", o.Text)
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
