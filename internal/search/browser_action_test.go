package search

import (
	"strings"
	"testing"
)

func TestValidateActionRequest(t *testing.T) {
	cases := []struct {
		name    string
		req     browserActionRequest
		wantErr bool
	}{
		{"empty steps", browserActionRequest{}, true},
		{"unknown action", browserActionRequest{Steps: []browserStep{{Action: "teleport"}}}, true},
		{"navigate needs url", browserActionRequest{Steps: []browserStep{{Action: actNavigate}}}, true},
		{"fill needs selector", browserActionRequest{Steps: []browserStep{{Action: actFill, Text: "x"}}}, true},
		{"viewport needs positive dims", browserActionRequest{Steps: []browserStep{{Action: actSetViewport, Width: 0, Height: 800}}}, true},
		{"press_key needs known key", browserActionRequest{Steps: []browserStep{{Action: actPressKey, Key: "F13"}}}, true},
		{"wait needs positive ms", browserActionRequest{Steps: []browserStep{{Action: actWait, MS: 0}}}, true},
		{
			"valid sequence",
			browserActionRequest{Steps: []browserStep{
				{Action: actSetViewport, Width: 375, Height: 812},
				{Action: actNavigate, URL: "https://example.com"},
				{Action: actFill, Selector: "#u", Text: "someuser"},
				{Action: actClick, Selector: "button"},
				{Action: actPressKey, Key: "Tab"},
			}},
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateActionRequest(&c.req)
			if (err != nil) != c.wantErr {
				t.Fatalf("validateActionRequest err=%v, wantErr=%v", err, c.wantErr)
			}
		})
	}
}

func TestValidateActionRequestQueryNames(t *testing.T) {
	req := browserActionRequest{
		Steps: []browserStep{{Action: actNavigate, URL: "https://example.com"}},
		Observe: browserObserve{Query: []browserQuery{
			{Name: "a", Selector: ".x"},
			{Name: "a", Selector: ".y"},
		}},
	}
	if err := validateActionRequest(&req); err == nil {
		t.Fatal("duplicate query name must be rejected")
	}

	req.Observe.Query[1].Name = "b"
	req.Observe.Query[1].Selector = ""
	if err := validateActionRequest(&req); err == nil {
		t.Fatal("empty query selector must be rejected")
	}
}

func TestActionURLReason(t *testing.T) {
	cfg := BrowserConfig{TestHosts: []string{"laban.internal"}}
	cases := []struct {
		name    string
		url     string
		allowed bool
	}{
		{"public host allowed", "https://example.com/x", true},
		{"non-http refused", "ftp://example.com", false},
		{"internal not listed refused", "https://other.internal/x", false},
		{"internal listed allowed", "https://laban.internal/events", true},
		{"internal listed case-insensitive", "https://LABAN.internal/events", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason := actionURLReason(c.url, cfg)
			if (reason == "") != c.allowed {
				t.Fatalf("actionURLReason(%q)=%q, allowed=%v", c.url, reason, c.allowed)
			}
		})
	}
}

func TestLandingHostReason(t *testing.T) {
	cfg := BrowserConfig{TestHosts: []string{"laban.internal"}}
	cases := []struct {
		name    string
		url     string
		allowed bool
	}{
		{"about:blank passes", "about:blank", true},
		{"data uri passes", "data:text/html,x", true},
		{"public landing passes", "https://example.com/", true},
		{"allowed internal passes", "https://laban.internal/events", true},
		{"unlisted internal blocked", "https://169.254.169.254/latest/meta-data/", false},
		{"other internal blocked", "https://other.internal/x", false},
		{"file scheme blocked", "file:///etc/passwd", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if (landingHostReason(c.url, cfg) == "") != c.allowed {
				t.Fatalf("landingHostReason(%q) allowed=%v", c.url, c.allowed)
			}
		})
	}
}

func TestKeySequence(t *testing.T) {
	for _, name := range []string{"Tab", "enter", "ESC", "ArrowDown", "space"} {
		if _, ok := keySequence(name); !ok {
			t.Errorf("key %q should be known", name)
		}
	}
	if _, ok := keySequence("PageUp"); ok {
		t.Error("PageUp should be unknown")
	}
}

func TestFormatActionResult(t *testing.T) {
	o := actionObservation{
		URL:       "https://laban.internal/events",
		Title:     "Events",
		HasActive: true,
		Active:    activeElementInfo{Selector: "input#search", Name: "q"},
		Queries: []namedQuery{
			{Name: "install_badge", Result: queryResult{Exists: false, Count: 0}},
			{Name: "tooltip", Result: queryResult{
				Exists: true, Count: 1, Text: "Top paths",
				Attrs:    map[string]string{"title": "Top paths help"},
				Computed: map[string]string{"visibility": "visible"},
			}},
		},
		ScreenshotPath: "/tmp/aigem-shot-1.png",
	}
	out := formatActionResult(o)
	for _, want := range []string{
		"URL: https://laban.internal/events",
		"Active element: input#search (name=q)",
		"install_badge: exists=false count=0",
		`tooltip: exists=true count=1 text="Top paths" title="Top paths help" visibility=visible`,
		"Screenshot saved: /tmp/aigem-shot-1.png",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("result missing %q\n---\n%s", want, out)
		}
	}
}

func TestQueryJSEmbedsSelectorSafely(t *testing.T) {
	js := queryJS(browserQuery{Selector: `a[href="x"]`, Attrs: []string{"title"}, Computed: []string{"visibility"}})
	if !strings.Contains(js, `querySelectorAll("a[href=\"x\"]")`) {
		t.Fatalf("selector not JSON-escaped into JS:\n%s", js)
	}
}
