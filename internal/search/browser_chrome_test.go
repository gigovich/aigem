package search

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const fixturePage = `<!doctype html><html><head><title>Fixture</title></head><body>
<form id="f" onsubmit="event.preventDefault();
  document.getElementById('out').textContent = 'submitted:' + document.getElementById('name').value">
<input id="name" name="name" value="old">
<button id="go" type="button"
  onclick="document.body.insertAdjacentHTML('beforeend', '<i id=clicked>ok</i>')">Go</button>
</form>
<div id="ed" contenteditable="true">x</div>
<p id="out">idle</p>
</body></html>`

const fixtureSecond = `<!doctype html><html><head><title>Second</title></head><body>
<p>second page body</p></body></html>`

func TestManagedBrowserDrivesAFixturePage(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a real browser")
	}
	if _, err := detectChromeExecutable(); err != nil {
		t.Skip(err)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/second" {
			_, _ = io.WriteString(w, fixtureSecond)
			return
		}
		_, _ = io.WriteString(w, fixturePage)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// Not t.TempDir: Chrome may still write to its profile after the context ends.
	profile, err := os.MkdirTemp("", "aigem-chrome-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profile) })
	bctx, stop, err := startChrome(ctx, BrowserConfig{ProfileDir: profile})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	steps := []browserStep{
		{Action: actSetViewport, Width: 800, Height: 600},
		{Action: actNavigate, URL: srv.URL + "/"},
		{Action: actFill, Selector: "#name", Text: "alice"},
		{Action: actClick, Selector: "#go"},
		{Action: actWaitFor, Selector: "#clicked", TimeoutMS: 5000},
		{Action: actPressKey, Selector: "#name", Key: "enter"},
		{Action: actHover, Selector: "#go"},
		{Action: actWait, MS: 10},
		{Action: actFill, Selector: "#ed", Text: "hello"},
	}
	for i, s := range steps {
		if err := execStep(bctx, s); err != nil {
			t.Fatalf("step %d (%s): %v", i+1, s.Action, err)
		}
	}

	o, err := observePage(bctx, browserObserve{
		Text:          true,
		ActiveElement: true,
		Query:         []browserQuery{{Name: "out", Selector: "#out"}, {Name: "ed", Selector: "#ed"}},
		Screenshot:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.URL != srv.URL+"/" || o.Title != "Fixture" {
		t.Errorf("location = %q, title = %q", o.URL, o.Title)
	}
	if !strings.Contains(o.Text, "submitted:alice") {
		t.Errorf("page text %q lacks the submitted value", o.Text)
	}
	if !o.HasActive || o.Active.Tag != "div" || o.Active.ID != "ed" {
		t.Errorf("active element = %+v", o.Active)
	}
	if got := o.Queries[0].Result; !got.Exists || got.Text != "submitted:alice" {
		t.Errorf("#out = %+v", got)
	}
	if got := o.Queries[1].Result.Text; got != "hello" {
		t.Errorf("#ed text = %q", got)
	}
	if info, err := os.Stat(o.ScreenshotPath); err != nil || info.Size() == 0 {
		t.Errorf("screenshot %q: %v", o.ScreenshotPath, err)
	}

	if present, err := browserCaptchaPresent(bctx); err != nil || present {
		t.Errorf("captcha present = %v, %v", present, err)
	}
	if err := showCaptchaOverlay(bctx); err != nil {
		t.Fatal(err)
	}
	if confirmed, err := browserCaptchaConfirmed(bctx); err != nil || confirmed {
		t.Errorf("captcha confirmed = %v, %v", confirmed, err)
	}

	res, err := readBrowserPage(bctx, srv.URL+"/second", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != srv.URL+"/second" || res.Title != "Second" || !strings.Contains(res.Description, "second page body") {
		t.Errorf("read page = %+v", res)
	}
}
