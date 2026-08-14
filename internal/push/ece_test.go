package push

import (
	"bytes"
	"crypto/ecdh"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/push/pushtest"
)

// The example in RFC 8291 section 5, which fixes every input this encoding has
// - both key pairs, the auth secret and the salt - and states the body byte for
// byte. It is the only way to know the derivation is right rather than merely
// self-consistent: a wrong info string encrypts and decrypts perfectly against
// itself and is unreadable to every browser.
const (
	vecPlaintext = "When I grow up, I want to be a watermelon"
	vecUAPub     = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	vecUAPriv    = "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"
	vecASPub     = "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"
	vecASPriv    = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
	vecAuth      = "BTBZMqHH6r4Tts7J_aSIgg"
	vecSalt      = "DGv6ra1nlYgDCS1FRnbzlw"
	vecBody      = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3v" +
		"CYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyou" +
		"BWLVWGNWQexSgSxsj_Qulcy4a-fN"
)

func decode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := b64.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

func TestEncryptMatchesRFC8291Vector(t *testing.T) {
	as, err := ecdh.P256().NewPrivateKey(decode(t, vecASPriv))
	if err != nil {
		t.Fatalf("application server key: %v", err)
	}
	sub := Subscription{Endpoint: "https://push.example.net/x", P256dh: vecUAPub, Auth: vecAuth}

	body, err := encrypt(sub, []byte(vecPlaintext), decode(t, vecSalt), as)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if got := b64.EncodeToString(body); got != vecBody {
		t.Errorf("body does not match RFC 8291 section 5\n got %s\nwant %s", got, vecBody)
	}
}

func TestEncryptIsReadableBySubscription(t *testing.T) {
	uaPriv, err := ecdh.P256().NewPrivateKey(decode(t, vecUAPriv))
	if err != nil {
		t.Fatalf("subscription key: %v", err)
	}
	sub := Subscription{Endpoint: "https://push.example.net/x", P256dh: vecUAPub, Auth: vecAuth}

	body, err := Encrypt(sub, []byte(vecPlaintext))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// Through pushtest, which is a second implementation of the receiving half:
	// a sender checked against its own derivation is self-consistent and
	// unreadable to every browser. This is also what pins pushtest itself, and
	// the notifier's tests read their payloads back through it.
	got, err := pushtest.Decrypt(uaPriv, decode(t, vecAuth), body)
	if err != nil {
		t.Fatalf("a subscription could not read what was sent to it: %v", err)
	}
	if string(got) != vecPlaintext {
		t.Errorf("round trip read %q, want %q", got, vecPlaintext)
	}
}

// Two messages to one subscription must not share a key stream. They would if
// either the ephemeral pair or the salt were reused, and the body is the only
// place that is visible from outside.
func TestEncryptUsesFreshKeyAndSaltEachTime(t *testing.T) {
	sub := Subscription{Endpoint: "https://push.example.net/x", P256dh: vecUAPub, Auth: vecAuth}
	first, err := Encrypt(sub, []byte(vecPlaintext))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	second, err := Encrypt(sub, []byte(vecPlaintext))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Equal(first[:saltLen], second[:saltLen]) {
		t.Error("the same salt was used twice")
	}
	if bytes.Equal(first[saltLen+5:headerLen], second[saltLen+5:headerLen]) {
		t.Error("the same ephemeral key was used twice")
	}
}

func TestEncryptRefusesAnOversizePayload(t *testing.T) {
	sub := Subscription{Endpoint: "https://push.example.net/x", P256dh: vecUAPub, Auth: vecAuth}
	if _, err := Encrypt(sub, bytes.Repeat([]byte("a"), MaxPayload+1)); err == nil {
		t.Fatal("a payload past the record size was accepted")
	}
	if _, err := Encrypt(sub, bytes.Repeat([]byte("a"), MaxPayload)); err != nil {
		t.Fatalf("the largest payload that fits was refused: %v", err)
	}
}

func TestEncryptRefusesKeysItCannotUse(t *testing.T) {
	for name, sub := range map[string]Subscription{
		"key is not base64": {P256dh: "not base64!", Auth: vecAuth},
		"key is off the curve": {
			P256dh: b64.EncodeToString(bytes.Repeat([]byte{4}, keyLen)), Auth: vecAuth,
		},
		"auth is the wrong length": {P256dh: vecUAPub, Auth: b64.EncodeToString([]byte("short"))},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Encrypt(sub, []byte("x")); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestValidate(t *testing.T) {
	ok := Subscription{Endpoint: "https://push.example.net/x", P256dh: vecUAPub, Auth: vecAuth}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a good subscription was refused: %v", err)
	}
	for name, bad := range map[string]Subscription{
		"http endpoint": {Endpoint: "http://push.example.net/x", P256dh: vecUAPub, Auth: vecAuth},
		// A point that decodes and is not on the curve. Storing one means every
		// push to it fails, and an invalid-curve point is how a key exchange is
		// attacked where it is not checked at all.
		"key off the curve": {
			Endpoint: "https://p.example/x",
			P256dh:   b64.EncodeToString(bytes.Repeat([]byte{4}, keyLen)),
			Auth:     vecAuth,
		},
		"no host":        {Endpoint: "https:///x", P256dh: vecUAPub, Auth: vecAuth},
		"empty endpoint": {P256dh: vecUAPub, Auth: vecAuth},
		"short auth":     {Endpoint: "https://p.example/x", P256dh: vecUAPub, Auth: b64.EncodeToString([]byte("nope"))},
		"bad key":        {Endpoint: "https://p.example/x", P256dh: "!!", Auth: vecAuth},
	} {
		t.Run(name, func(t *testing.T) {
			err := bad.Validate()
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), "push: invalid subscription") {
				t.Errorf("error does not name the refusal: %v", err)
			}
		})
	}
}

// A padded endpoint is one something upstream mangled. Trimming it and storing
// what was sent produced a subscription that validated once and failed on every
// delivery; keeping the trailing space produced one escaped into %20.
func TestValidateRefusesAPaddedEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"https://push.example.net/send/abc ",
		" https://push.example.net/send/abc",
		"https://push.example.net/send/abc\n",
	} {
		sub := Subscription{Endpoint: endpoint, P256dh: vecUAPub, Auth: vecAuth}
		if err := sub.Validate(); err == nil {
			t.Errorf("accepted %q", endpoint)
		}
	}
}

// Validate's own refusals are logged when a subscription reaches delivery
// without having come through the store, and url.Parse fails with a *url.Error
// carrying the whole endpoint.
func TestValidateKeepsTheEndpointOutOfItsError(t *testing.T) {
	sub := Subscription{
		Endpoint: "https://push.example.net/send/secret-capability\x7f",
		P256dh:   vecUAPub, Auth: vecAuth,
	}
	err := sub.Validate()
	if err == nil {
		t.Fatal("an unparseable endpoint was accepted")
	}
	if strings.Contains(err.Error(), "secret-capability") {
		t.Errorf("the refusal carries the endpoint: %v", err)
	}
}
