package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadKeysGeneratesOnceAndKeepsThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat", "vapid.json")
	first, err := LoadKeys(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if first.Public == "" {
		t.Fatal("no public key")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// The private half of the identity every stored subscription is bound to.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file is %o, want 600", perm)
	}
	second, err := LoadKeys(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if second.Public != first.Public {
		t.Error("a second load minted a new key, which would strand every subscription")
	}
}

func TestLoadKeysRejectsAFileThatDoesNotAgreeWithItself(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vapid.json")
	keys, err := LoadKeys(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	other, err := LoadKeys(filepath.Join(dir, "other.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var kf keyFile
	if err := json.Unmarshal(raw, &kf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	kf.Public = other.Public
	swapped, err := json.Marshal(kf)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, swapped, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadKeys(path); err == nil {
		t.Fatal("a mismatched key pair was accepted")
	}
	if keys.Public == other.Public {
		t.Fatal("two generated keys are identical")
	}
}

func TestLoadKeysRejectsNonsense(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vapid.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadKeys(path); err == nil {
		t.Fatal("a corrupt file was accepted")
	}
}

func TestAuthorizationIsAVerifiableTokenForOneService(t *testing.T) {
	keys, err := LoadKeys(filepath.Join(t.TempDir(), "vapid.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	header, err := keys.Authorization("https://fcm.googleapis.com/fcm/send/abc123", now)
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}

	scheme, rest, ok := strings.Cut(header, " ")
	if !ok || scheme != "vapid" {
		t.Fatalf("header does not use the vapid scheme: %q", header)
	}
	tok, key, ok := strings.Cut(rest, ",")
	if !ok {
		t.Fatalf("header carries no key: %q", header)
	}
	if key != "k="+keys.Public {
		t.Errorf("k= is %q, want the public key %q", key, keys.Public)
	}
	jwt := strings.TrimPrefix(tok, "t=")
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}

	var head struct{ Typ, Alg string }
	if err := json.Unmarshal(decode(t, parts[0]), &head); err != nil {
		t.Fatalf("header: %v", err)
	}
	if head.Alg != "ES256" || head.Typ != "JWT" {
		t.Errorf("token header is %+v, want a JWT signed with ES256", head)
	}

	var claims struct {
		Aud string `json:"aud"`
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(decode(t, parts[1]), &claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	// The origin and nothing else: the path names the subscription.
	if claims.Aud != "https://fcm.googleapis.com" {
		t.Errorf("aud is %q, want the push service's origin alone", claims.Aud)
	}
	if claims.Sub == "" {
		t.Error("no sub claim; services refuse a token without one")
	}
	if want := now.Add(tokenLife).Unix(); claims.Exp != want {
		t.Errorf("exp is %d, want %d", claims.Exp, want)
	}
	if tokenLife > 24*time.Hour {
		t.Error("RFC 8292 caps a token's life at 24 hours")
	}

	sig := decode(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want the two fixed-width integers JWS asks for", len(sig))
	}
	pubBytes := decode(t, keys.Public)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pubBytes)
	if err != nil {
		t.Fatalf("public key: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		t.Error("the signature does not verify against the advertised public key")
	}
}

func TestAuthorizationRefusesAnEndpointItCannotAddress(t *testing.T) {
	keys, err := LoadKeys(filepath.Join(t.TempDir(), "vapid.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := keys.Authorization("/fcm/send/abc", time.Now()); err == nil {
		t.Fatal("a relative endpoint was accepted")
	}
}

// A truncated key file is a state the daemon cannot leave on its own: it will
// not regenerate over one (that would strand every subscription), so the file
// has to arrive whole or not at all.
func TestLoadKeysLeavesNoHalfWrittenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vapid.json")
	if _, err := LoadKeys(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	// Nothing else is left behind for the next start to trip over.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "vapid.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the state directory holds %v, want only the key file", names)
	}
}

// Two daemons starting at once must not each mint a key: every subscription
// already made is bound to whichever one is on disk.
func TestLoadKeysAgreesUnderARace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vapid.json")
	keys := make([]string, 8)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := LoadKeys(path)
			if err != nil {
				t.Errorf("load: %v", err)
				return
			}
			keys[i] = k.Public
		}()
	}
	wg.Wait()
	for _, k := range keys {
		if k != keys[0] {
			t.Fatalf("two starts disagree about the key: %q and %q", k, keys[0])
		}
	}
}
