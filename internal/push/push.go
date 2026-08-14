// Package push delivers Web Push messages to a browser that is not open.
//
// It is the only new cryptography in the fleet's UI, and it is written against
// the RFCs rather than pulled in as a dependency: the whole of it is one ECDH,
// four HKDF expansions and an AES-GCM seal (RFC 8291), plus a signed JWT
// (RFC 8292). A dependency for that is more supply chain than code.
//
// Nothing here decides when to notify. That belongs to the caller.
package push

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// b64 is the encoding every value in these RFCs is written in: URL-safe and
// unpadded, on the wire and in the browser's subscription object alike.
var b64 = base64.RawURLEncoding

// Subscription is what a browser hands back from pushManager.subscribe: where
// to deliver, and the two keys the payload is encrypted to. The daemon stores
// it verbatim and never inspects the endpoint's host - which service the
// browser chose is the browser's business.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	// P256dh is the subscription's public key, uncompressed P-256, base64url.
	P256dh string `json:"p256dh"`
	// Auth is the 16-byte shared authentication secret, base64url.
	Auth string `json:"auth"`
}

// ErrInvalidSubscription reports a subscription the daemon cannot deliver to.
// It is a refusal to store, not a delivery failure: a subscription whose keys
// do not parse would fail identically on every push forever.
var ErrInvalidSubscription = errors.New("push: invalid subscription")

// authLen is fixed by RFC 8291: the browser generates 16 bytes.
const authLen = 16

// Validate reports whether this subscription is deliverable, so a bad one is
// refused at the API boundary rather than discovered by a push that never
// arrives.
func (s Subscription) Validate() error {
	// Whitespace around an endpoint means something upstream is mangling it.
	// Trimming it here while storing what was sent produced a subscription that
	// validated once and then failed on every delivery; accepting a trailing
	// space produced one that was escaped into %20 and delivered to nobody.
	if strings.TrimSpace(s.Endpoint) != s.Endpoint {
		return fmt.Errorf("%w: endpoint is padded with whitespace", ErrInvalidSubscription)
	}
	u, err := url.Parse(s.Endpoint)
	switch {
	case err != nil:
		// Without the parse error: it is a *url.Error carrying the whole URL,
		// and this error is logged when a subscription reaches delivery without
		// having come through the store.
		return fmt.Errorf("%w: endpoint does not parse as a URL", ErrInvalidSubscription)
	case u.Scheme != "https":
		// Not a purity check: the payload is encrypted, but the endpoint is a
		// bearer capability to notify this browser, and http would hand it to
		// anyone on the path.
		return fmt.Errorf("%w: endpoint must be https", ErrInvalidSubscription)
	case u.Host == "":
		return fmt.Errorf("%w: endpoint names no host", ErrInvalidSubscription)
	}
	key, err := b64.DecodeString(s.P256dh)
	if err != nil {
		return fmt.Errorf("%w: p256dh: %w", ErrInvalidSubscription, err)
	}
	if _, err := parsePublic(key); err != nil {
		return fmt.Errorf("%w: p256dh: %w", ErrInvalidSubscription, err)
	}
	secret, err := b64.DecodeString(s.Auth)
	if err != nil {
		return fmt.Errorf("%w: auth: %w", ErrInvalidSubscription, err)
	}
	if len(secret) != authLen {
		return fmt.Errorf("%w: auth is %d bytes, want %d", ErrInvalidSubscription, len(secret), authLen)
	}
	return nil
}
