package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// VAPID, RFC 8292: the daemon signs a short-lived JWT for each push service it
// talks to, and the browser is told the public key when it subscribes. It is
// not authentication in any useful sense - the subscription endpoint is the
// capability - but a service will refuse a push without it, and it is what ties
// a subscription to one application server.

// contact is the "sub" claim: who to reach about a misbehaving sender. RFC 8292
// wants a mailto: or https: URI, and a push service operator with a complaint
// has nowhere else to go. It names the project rather than the operator,
// because the operator's address is not the daemon's to publish.
const contact = "https://github.com/gigovich/aigem"

// tokenLife is how long a signed token stays valid. RFC 8292 caps it at 24
// hours; half a day leaves room for a clock that disagrees with the service's
// without minting a credential that outlives the day it was made for.
const tokenLife = 12 * time.Hour

// Keys is the application server's identity. The private half never leaves the
// process; the public half is handed to every browser that subscribes, and a
// subscription made against one key is worthless to any other - which is why
// the file is generated once and kept rather than regenerated per start.
type Keys struct {
	priv *ecdsa.PrivateKey
	// Public is the uncompressed P-256 point, base64url: exactly what
	// pushManager.subscribe wants as applicationServerKey.
	Public string
}

// keyFile is the on-disk form. Both halves are stored, so a file that is
// readable is checkable: a public key that does not match its private half is
// a corrupt file rather than a mystery at the push service.
type keyFile struct {
	Private string `json:"private"`
	Public  string `json:"public"`
}

// LoadKeys reads the application server's keys, generating them the first time.
//
// The file is 0600 and stays where the store lives, because it is the identity
// every existing subscription is bound to: lose it and every browser has to
// subscribe again, silently, since nothing tells a phone its subscription has
// become undeliverable.
func LoadKeys(path string) (*Keys, error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		return parseKeys(raw, path)
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("push: read %s: %w", path, err)
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	pub, err := priv.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	d, err := priv.Bytes()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(keyFile{Private: b64.EncodeToString(d), Public: b64.EncodeToString(pub)})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("push: %w", err)
	}
	// Written whole into a temporary file and then linked into place. Both
	// halves of that matter, and for different reasons: the link is exclusive,
	// so two daemons racing to first-run cannot each write a key and leave
	// subscriptions bound to whichever lost - and it is atomic, so a crash
	// between the create and the write cannot leave a truncated key file, which
	// nothing would regenerate (that would strand every subscription) and which
	// would therefore turn notifications off until someone deleted it by hand.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vapid-*")
	if err != nil {
		return nil, fmt.Errorf("push: create %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("push: write %s: %w", path, err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("push: write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("push: write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("push: write %s: %w", path, err)
	}
	if err := linkAndSync(tmp.Name(), path); err != nil {
		if os.IsExist(err) {
			// Someone else got there first. Theirs is the key every subscription
			// made from now on will be bound to.
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil, fmt.Errorf("push: read %s: %w", path, rerr)
			}
			return parseKeys(raw, path)
		}
		return nil, fmt.Errorf("push: create %s: %w", path, err)
	}
	return &Keys{priv: priv, Public: b64.EncodeToString(pub)}, nil
}

// linkAndSync puts the file in place and makes the directory entry durable.
//
// The link alone is atomic against a reader, not against a power cut: the entry
// can still be in the kernel's cache when browsers have already subscribed to
// the key it names, and losing it after that is exactly what this whole dance
// is here to prevent. A directory that cannot be synced is not an error - some
// filesystems refuse it - and the file is there either way.
func linkAndSync(from, to string) error {
	if err := os.Link(from, to); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(to))
	if err != nil {
		return nil
	}
	defer func() { _ = dir.Close() }()
	_ = dir.Sync()
	return nil
}

func parseKeys(raw []byte, path string) (*Keys, error) {
	var kf keyFile
	if err := json.Unmarshal(raw, &kf); err != nil {
		return nil, fmt.Errorf("push: %s is not a key file: %w", path, err)
	}
	d, err := b64.DecodeString(kf.Private)
	if err != nil {
		return nil, fmt.Errorf("push: %s: private key: %w", path, err)
	}
	priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d)
	if err != nil {
		return nil, fmt.Errorf("push: %s: private key: %w", path, err)
	}
	pub, err := priv.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	if got := b64.EncodeToString(pub); got != kf.Public {
		return nil, fmt.Errorf("push: %s: the public key does not belong to the private one", path)
	}
	return &Keys{priv: priv, Public: kf.Public}, nil
}

// Authorization builds the header for one endpoint. The token is bound to the
// push service's origin and to nothing else, so it cannot be replayed at a
// different service by one that receives it.
func (k *Keys) Authorization(endpoint string, now time.Time) (string, error) {
	aud, err := originOf(endpoint)
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"aud": aud,
		"exp": now.Add(tokenLife).Unix(),
		"sub": contact,
	})
	if err != nil {
		return "", err
	}
	// The header is constant, so it is written rather than encoded: {"typ":
	// "JWT","alg":"ES256"}.
	signing := "eyJ0eXAiOiJKV1QiLCJhbGciOiJFUzI1NiJ9." + b64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, digest[:])
	if err != nil {
		return "", err
	}
	// JWS wants the two integers fixed-width and concatenated. ASN.1, which
	// ecdsa.SignASN1 produces, is the same signature in an encoding no push
	// service accepts.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	return "vapid t=" + signing + "." + b64.EncodeToString(sig) + ",k=" + k.Public, nil
}

// originOf is the "aud" claim: scheme and host, nothing else. A push endpoint
// carries the subscription id in its path, and putting that in a signed token
// would hand every service a name for it.
func originOf(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		// Without the endpoint in it: this error is logged, and the endpoint is
		// the capability to notify that browser.
		return "", fmt.Errorf("%w: endpoint does not parse as a URL", ErrInvalidSubscription)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("%w: endpoint is not an absolute URL", ErrInvalidSubscription)
	}
	return u.Scheme + "://" + u.Host, nil
}
