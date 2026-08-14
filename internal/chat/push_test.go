package chat

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/push"
)

// A subscription as a browser produces one. The keys are the RFC 8291 example's,
// which are the only P-256 point and 16-byte secret in the tree that are known
// to be well formed.
func testSub(endpoint string) push.Subscription {
	return push.Subscription{
		Endpoint: endpoint,
		P256dh: "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpk" +
			"NtoIAiw4",
		Auth: "BTBZMqHH6r4Tts7J_aSIgg",
	}
}

func TestPushSubsRoundTrip(t *testing.T) {
	s := newStore(t)
	sub := testSub("https://push.example.net/send/one")
	if err := s.AddPushSub(t.Context(), sub); err != nil {
		t.Fatalf("add: %v", err)
	}
	got, err := s.PushSubs(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0] != sub {
		t.Fatalf("stored %+v, want %+v", got, sub)
	}

	if err := s.DeletePushSub(t.Context(), sub.Endpoint); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Twice, because a service answering 410 and a browser unsubscribing can
	// both arrive for the same endpoint.
	if err := s.DeletePushSub(t.Context(), sub.Endpoint); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if got, err = s.PushSubs(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("after delete: %+v, %v", got, err)
	}
}

// Re-subscribing is what a page does on every load. It must update the row it
// already has rather than accumulate.
func TestAddPushSubReplacesTheSameEndpoint(t *testing.T) {
	s := newStore(t)
	sub := testSub("https://push.example.net/send/one")
	if err := s.AddPushSub(t.Context(), sub); err != nil {
		t.Fatalf("add: %v", err)
	}
	rotated := sub
	rotated.Auth = "gaSCatx3sT8oKn6rmshMHg"
	if err := s.AddPushSub(t.Context(), rotated); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	got, err := s.PushSubs(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("%d rows for one endpoint", len(got))
	}
	if got[0].Auth != rotated.Auth {
		t.Errorf("auth is %q, want the one the browser last sent", got[0].Auth)
	}
}

func TestAddPushSubKeepsTheNewestUnderTheCap(t *testing.T) {
	s := newStore(t)
	// One millisecond apart, because the cap orders by age and a store whose
	// clock does not move cannot tell these apart.
	base := s.now()
	for i := range maxPushSubs + 5 {
		s.now = func() time.Time { return base.Add(time.Duration(i) * time.Millisecond) }
		if err := s.AddPushSub(t.Context(), testSub(fmt.Sprintf("https://push.example.net/send/%d", i))); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	got, err := s.PushSubs(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// The literal, not the constant it came from: raising the cap should break
	// a test that says what the cap is.
	if len(got) != 20 {
		t.Fatalf("kept %d subscriptions, want the cap of 20", len(got))
	}
	// The five oldest are the ones dropped, and every one of them.
	for i := range 5 {
		want := fmt.Sprintf("https://push.example.net/send/%d", i)
		for _, sub := range got {
			if sub.Endpoint == want {
				t.Errorf("kept %s, which is past the cap", sub.Endpoint)
			}
		}
	}
	if !strings.HasSuffix(got[len(got)-1].Endpoint, fmt.Sprint(maxPushSubs+4)) {
		t.Errorf("newest is %s", got[len(got)-1].Endpoint)
	}
}

func TestAddPushSubRefusesOneItCouldNeverDeliverTo(t *testing.T) {
	s := newStore(t)
	bad := testSub("http://push.example.net/send/one")
	err := s.AddPushSub(t.Context(), bad)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("add: %v, want ErrInvalid", err)
	}
	got, err := s.PushSubs(t.Context())
	if err != nil || len(got) != 0 {
		t.Fatalf("a refused subscription was stored: %+v, %v", got, err)
	}
}

// ---- the routes ----

// testPushAPI stands the routes up with a key configured, which is the daemon a
// browser can actually subscribe to.
func testPushAPI(t *testing.T, key string) (*Store, *httptest.Server) {
	t.Helper()
	s := newStore(t)
	hub := NewHub()
	_ = s.AddPublisher("hub", hub.Publish)
	api := NewAPI(s, hub)
	api.SetPushKey(key)
	mux := http.NewServeMux()
	api.Mount(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

func TestPushKeyRouteSaysWhetherThereIsOne(t *testing.T) {
	_, srv := testPushAPI(t, "BPublicKey")
	got := decode[PushAvailability](t, do(t, srv, http.MethodGet, "/api/chat/push", nil))
	if !got.Available || got.Key != "BPublicKey" {
		t.Fatalf("key route returned %+v", got)
	}

	_, none := testPushAPI(t, "")
	got = decode[PushAvailability](t, do(t, none, http.MethodGet, "/api/chat/push", nil))
	if got.Available || got.Key != "" {
		t.Fatalf("a daemon with no keys offered %+v", got)
	}
}

func TestSubscribeRouteStoresAndForgets(t *testing.T) {
	store, srv := testPushAPI(t, "BPublicKey")
	sub := testSub("https://push.example.net/send/phone")

	if res := do(t, srv, http.MethodPost, "/api/chat/push/subs", sub); res.StatusCode != http.StatusNoContent {
		t.Fatalf("subscribe answered %s", res.Status)
	}
	subs, err := store.PushSubs(t.Context())
	if err != nil || len(subs) != 1 {
		t.Fatalf("stored %+v, %v", subs, err)
	}

	body := map[string]string{"endpoint": sub.Endpoint}
	if res := do(t, srv, http.MethodDelete, "/api/chat/push/subs", body); res.StatusCode != http.StatusNoContent {
		t.Fatalf("unsubscribe answered %s", res.Status)
	}
	if subs, err = store.PushSubs(t.Context()); err != nil || len(subs) != 0 {
		t.Fatalf("after unsubscribing: %+v, %v", subs, err)
	}
}

func TestSubscribeRouteRefusesWhatItCannotUse(t *testing.T) {
	_, srv := testPushAPI(t, "BPublicKey")
	res := do(t, srv, http.MethodPost, "/api/chat/push/subs", testSub("http://push.example.net/send/phone"))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("an http endpoint answered %s", res.Status)
	}
	res = do(t, srv, http.MethodDelete, "/api/chat/push/subs", map[string]string{"endpoint": " "})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsubscribing from nothing answered %s", res.Status)
	}
}

// Subscribing to a daemon with no keys would store a row nothing could ever
// push to, and the page would show notifications as on.
func TestSubscribeRouteRefusesWhenThereIsNoKey(t *testing.T) {
	store, srv := testPushAPI(t, "")
	res := do(t, srv, http.MethodPost, "/api/chat/push/subs", testSub("https://push.example.net/send/phone"))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("subscribe answered %s on a daemon with no keys", res.Status)
	}
	if subs, err := store.PushSubs(t.Context()); err != nil || len(subs) != 0 {
		t.Fatalf("stored %+v, %v", subs, err)
	}
}

// The cap keeps the newest, and re-subscribing is what a page does on every
// load - so a browser that is still there has to count as new. Without that,
// the operator's phone is the first row evicted by a browser that mints a fresh
// endpoint every time it loads.
func TestAddPushSubKeepsABrowserThatKeepsComingBack(t *testing.T) {
	s := newStore(t)
	base := s.now()
	at := func(i int) { s.now = func() time.Time { return base.Add(time.Duration(i) * time.Millisecond) } }

	phone := testSub("https://push.example.net/send/phone")
	at(0)
	if err := s.AddPushSub(t.Context(), phone); err != nil {
		t.Fatal(err)
	}
	// A browser that mints a new endpoint on every load, up to the cap.
	for i := range maxPushSubs - 1 {
		at(i + 1)
		if err := s.AddPushSub(t.Context(), testSub(fmt.Sprintf("https://push.example.net/send/%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	// The phone loads the page again, which re-sends the subscription it has.
	at(maxPushSubs)
	if err := s.AddPushSub(t.Context(), phone); err != nil {
		t.Fatal(err)
	}
	// And the churning browser takes the table past the cap.
	at(maxPushSubs + 1)
	if err := s.AddPushSub(t.Context(), testSub("https://push.example.net/send/last")); err != nil {
		t.Fatal(err)
	}

	subs, err := s.PushSubs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != maxPushSubs {
		t.Fatalf("kept %d subscriptions, want the cap of %d", len(subs), maxPushSubs)
	}
	for _, sub := range subs {
		if sub.Endpoint == phone.Endpoint {
			return
		}
	}
	t.Errorf("the browser that keeps coming back was evicted: %+v", subs)
}
