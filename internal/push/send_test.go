package push

import (
	"context"
	"crypto/ecdh"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/push/pushtest"
)

// testClient talks to srv, whose certificate no root store has heard of. A
// stand-in has to serve TLS at all because a subscription endpoint is https or
// it is refused.
func testClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	keys, err := LoadKeys(filepath.Join(t.TempDir(), "vapid.json"))
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if srv == nil {
		return NewClient(keys)
	}
	return NewClient(keys, WithTransport(srv.Client().Transport))
}

func TestSendPostsAnEncryptedMessageTheBrowserCanRead(t *testing.T) {
	uaPriv, err := ecdh.P256().NewPrivateKey(decode(t, vecUAPriv))
	if err != nil {
		t.Fatalf("subscription key: %v", err)
	}
	var got *http.Request
	var body []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := testClient(t, srv)
	sub := Subscription{Endpoint: srv.URL + "/send/abc", P256dh: vecUAPub, Auth: vecAuth}
	err = c.Send(t.Context(), sub, Message{Payload: []byte(vecPlaintext), Topic: "t_abc"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	if got.Method != http.MethodPost {
		t.Errorf("method is %s", got.Method)
	}
	if enc := got.Header.Get("Content-Encoding"); enc != "aes128gcm" {
		t.Errorf("Content-Encoding is %q", enc)
	}
	if ct := got.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type is %q", ct)
	}
	if got.Header.Get("Topic") != "t_abc" {
		t.Errorf("Topic is %q", got.Header.Get("Topic"))
	}
	// A literal, not the constant: comparing a value against the thing that
	// produced it asserts nothing about what goes on the wire. Twelve hours is
	// how long a service holds this for a phone that is off.
	if n, err := strconv.Atoi(got.Header.Get("TTL")); err != nil || n != 43200 {
		t.Errorf("TTL is %q, want 43200 seconds", got.Header.Get("TTL"))
	}
	// Urgency is what makes a phone wake rather than batch the message, which
	// is the whole point of pushing this particular event.
	if u := got.Header.Get("Urgency"); u != "high" {
		t.Errorf("Urgency is %q, want high", u)
	}
	if a := got.Header.Get("Authorization"); !strings.HasPrefix(a, "vapid t=") ||
		!strings.Contains(a, ",k="+c.PublicKey()) {
		t.Errorf("Authorization is %q", a)
	}
	plain, err := pushtest.Decrypt(uaPriv, decode(t, vecAuth), body)
	if err != nil {
		t.Fatalf("the subscription could not read what was posted to it: %v", err)
	}
	if string(plain) != vecPlaintext {
		t.Errorf("the subscription reads %q, want %q", plain, vecPlaintext)
	}
}

func TestSendReportsAVanishedSubscription(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()
			sub := Subscription{Endpoint: srv.URL + "/send/abc", P256dh: vecUAPub, Auth: vecAuth}
			err := testClient(t, srv).Send(t.Context(), sub, Message{Payload: []byte("x")})
			if !errors.Is(err, ErrGone) {
				t.Fatalf("send: %v, want ErrGone", err)
			}
		})
	}
}

func TestSendReportsARefusalWithoutLeakingTheEndpoint(t *testing.T) {
	// What a WAF in front of a push service answers with: the request URL,
	// which is the capability to notify that browser.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "The requested URL "+r.URL.String()+" was rejected",
			http.StatusRequestEntityTooLarge)
	}))
	defer srv.Close()
	sub := Subscription{Endpoint: srv.URL + "/send/secret-capability", P256dh: vecUAPub, Auth: vecAuth}

	err := testClient(t, srv).Send(t.Context(), sub, Message{Payload: []byte("x")})
	if err == nil {
		t.Fatal("a 413 was reported as success")
	}
	if errors.Is(err, ErrGone) {
		t.Fatal("a 413 was reported as a vanished subscription")
	}
	if strings.Contains(err.Error(), "secret-capability") {
		t.Errorf("the error names the subscription path: %v", err)
	}
	// The status is what a reader can act on. The body is not repeated,
	// because a service that echoes the request URL would put the endpoint
	// straight back into the log.
	if !strings.Contains(err.Error(), "413") {
		t.Errorf("the error drops the status: %v", err)
	}
}

func TestSendRefusesATopicNoServiceWouldAccept(t *testing.T) {
	// A real service, so a test that stopped checking the topic would fail by
	// delivering rather than pass on a DNS lookup that went nowhere.
	var delivered int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered++
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	sub := Subscription{Endpoint: srv.URL + "/send/abc", P256dh: vecUAPub, Auth: vecAuth}

	for name, topic := range map[string]string{
		"too long":    strings.Repeat("a", 33),
		"not base64":  "thread/1",
		"has a space": "t abc",
	} {
		t.Run(name, func(t *testing.T) {
			err := testClient(t, srv).Send(t.Context(), sub, Message{Payload: []byte("x"), Topic: topic})
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), "URL-safe base64") {
				t.Errorf("the refusal does not name the topic rule: %v", err)
			}
		})
	}
	if delivered != 0 {
		t.Errorf("%d messages with an unusable topic were delivered", delivered)
	}
}

func TestSendFailsBeforeReachingAServiceItCannotAddress(t *testing.T) {
	sub := Subscription{Endpoint: "https://push.example.net/x", P256dh: "!!", Auth: vecAuth}
	if err := testClient(t, nil).Send(context.Background(), sub, Message{Payload: []byte("x")}); err == nil {
		t.Fatal("a subscription with an unusable key was sent to")
	}
}

// The endpoint is the capability to notify that browser. net/http puts the
// whole request URL in a *url.Error, so an ordinary network hiccup was writing
// it into the daemon's log on every alert.
func TestSendKeepsTheEndpointOutOfATransportError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := testClient(t, srv)
	endpoint := srv.URL + "/send/secret-capability"
	srv.Close() // nothing is listening now, so the POST fails in the transport

	sub := Subscription{Endpoint: endpoint, P256dh: vecUAPub, Auth: vecAuth}
	err := client.Send(t.Context(), sub, Message{Payload: []byte("x")})
	if err == nil {
		t.Fatal("sending to a closed service succeeded")
	}
	if strings.Contains(err.Error(), "secret-capability") {
		t.Errorf("the error carries the endpoint: %v", err)
	}
}

// Following a redirect would let a push service aim the daemon at any host it
// likes, and a chain ending in 410 would delete the operator's subscription.
func TestSendDoesNotFollowARedirect(t *testing.T) {
	var elsewhere int
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere++
		w.WriteHeader(http.StatusGone)
	}))
	defer other.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, other.URL+"/send/abc", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	sub := Subscription{Endpoint: srv.URL + "/send/abc", P256dh: vecUAPub, Auth: vecAuth}
	err := testClient(t, srv).Send(t.Context(), sub, Message{Payload: []byte("x")})
	if elsewhere != 0 {
		t.Error("the daemon followed a push service's redirect")
	}
	if errors.Is(err, ErrGone) {
		t.Error("a redirect to a 410 was read as a vanished subscription")
	}
}

func TestValidTopic(t *testing.T) {
	for topic, want := range map[string]bool{
		"t_db0b588a96bd081b":    true,
		"":                      false,
		strings.Repeat("a", 32): true,
		strings.Repeat("a", 33): false,
		"thread/1":              false,
		"t abc":                 false,
		"A-Za-z0-9_":            true,
	} {
		if got := ValidTopic(topic); got != want {
			t.Errorf("ValidTopic(%q) = %v, want %v", topic, got, want)
		}
	}
}

// The store validates what it stores, but the store is not the only way a
// subscription can reach delivery - a row written by an older version, or a
// caller that skipped it, would otherwise be POSTed to over plain http with the
// endpoint in clear.
func TestSendRefusesAnEndpointItWouldNotHaveStored(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	sub := Subscription{Endpoint: srv.URL + "/send/abc", P256dh: vecUAPub, Auth: vecAuth}
	err := testClient(t, nil).Send(t.Context(), sub, Message{Payload: []byte("x")})
	if !errors.Is(err, ErrInvalidSubscription) {
		t.Fatalf("send to an http endpoint: %v, want a refusal", err)
	}
	if reached {
		t.Error("the message was delivered over plain http")
	}
}
