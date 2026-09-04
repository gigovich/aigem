package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// getMeta reads /api/meta with the daemon's token.
func getMeta(t *testing.T, srv *Server) (*http.Response, metaResponse) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.Base()+"api/meta", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var body metaResponse
	if res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return res, body
}

func TestMetaReportsTheVersionTheModelAndTheFeatures(t *testing.T) {
	want := Meta{Version: "1.2.3", DefaultModel: "openai/gpt-5.6-sol"}
	srv := newTestServer(t, Config{Backend: &fakeBackend{meta: want}})
	res, got := getMeta(t, srv)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if got.Meta != want {
		t.Errorf("meta = %+v, want %+v", got.Meta, want)
	}
	if !got.Features["controlSocket"] {
		t.Errorf("features = %v, want the control socket this build serves", got.Features)
	}
}

// The answer describes this server, not the binary: a caller can build a daemon
// with no assets out of a binary that carries them, and the page it serves is
// then a 501. This is the check /healthz used to carry, and it moved here whole
// when the capability map did.
func TestMetaReportsThisServersUIState(t *testing.T) {
	if _, got := getMeta(t, newTestServer(t, Config{})); got.UI {
		t.Error("a daemon built with no assets reported ui = true")
	}
	withUI := Config{Assets: spaHandler(testDist())}
	if _, got := getMeta(t, newTestServer(t, withUI)); !got.UI {
		t.Error("a daemon built with assets reported ui = false")
	}
}

// The whole capability map is behind Guard. It describes the machine the daemon
// runs on - its version, the operator's model, what this build can do - and
// there is no caller entitled to that before signing in.
func TestMetaNeedsACredential(t *testing.T) {
	srv := newTestServer(t, Config{Backend: &fakeBackend{meta: Meta{Version: "1.2.3"}}})
	res, err := http.Get(srv.Base() + "api/meta")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "1.2.3") {
		t.Errorf("the refusal leaked the version:\n%s", body)
	}
}

func TestMetaSaysSoWhenTheBackendCannot(t *testing.T) {
	srv := newTestServer(t, Config{Backend: &fakeBackend{err: errors.New("no model store")}})
	res, _ := getMeta(t, srv)
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", res.StatusCode)
	}
}

func TestOtherMethodsOnMetaAre405(t *testing.T) {
	srv := newTestServer(t, Config{})
	res, err := http.Post(srv.Base()+"api/meta", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/meta = %d, want 405", res.StatusCode)
	}
}

// The map names only what this build actually serves. A flag listed as false
// for a route that does not exist commits the wire to a name three stages
// before the code, with nothing to catch it going stale or getting spelled
// differently when it lands.
func TestTheFeatureMapNamesOnlyWhatIsServed(t *testing.T) {
	srv := newTestServer(t, Config{})
	_, got := getMeta(t, srv)
	for name, on := range got.Features {
		if !on {
			t.Errorf("features names %q as unsupported; leave it out instead", name)
		}
	}
	if _, ok := got.Features["controlSocket"]; !ok {
		t.Error("the control socket is served and not named")
	}
}

// The snapshot says which revision it is current as of, and reads it before it
// is taken: a page that noticed a gap refetches here, and a base labelled newer
// than it is would leave that page silently stale with no second gap to catch
// it.
func TestMetaIsStampedWithARevisionItCannotPredate(t *testing.T) {
	// The backend mutates while it is being asked, which is what pins the order
	// of the two reads. Against a quiet one, taking the revision after the
	// snapshot would look identical.
	b := &mutatingBackend{}
	srv := newTestServer(t, Config{Backend: b})
	b.hub = srv.hub

	_, got := getMeta(t, srv)
	at := b.at.Load()
	if at == 0 {
		t.Fatal("the backend never published; this test checked nothing")
	}
	if got.Rev >= at {
		t.Fatalf("the snapshot claims rev %d, but it was read before the mutation at rev %d: "+
			"a page would mark itself current at a revision this answer predates", got.Rev, at)
	}
}
