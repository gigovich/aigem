package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// The aes128gcm content encoding, RFC 8188, keyed the way RFC 8291 says.
//
// A body is one record: header || AES-GCM(plaintext || 0x02). Records exist so
// a stream can be encrypted in pieces, and a push message is never a stream -
// it is one payload a push service will not carry past 4KiB anyway. Sending it
// as several records would only add ways to be wrong.
const (
	// saltLen and keyLen are the header's fixed-width fields.
	saltLen = 16
	keyLen  = 65
	// headerLen is salt || record size || key length || key.
	headerLen = saltLen + 4 + 1 + keyLen
	// recordSize is what the header declares. The last record may be shorter
	// than it declares, which is why nothing is padded to reach it.
	recordSize = 4096
	// MaxPayload is the longest plaintext that fits one record inside the 4KiB
	// a push service is required to accept: the record holds the plaintext, the
	// 0x02 delimiter and the GCM tag, and the header sits in front of it.
	MaxPayload = recordSize - headerLen - 1 - 16
	// delimiter marks the last record. 0x01 would mean "more follow".
	delimiter = 0x02
)

// parsePublic decodes an uncompressed P-256 point, rejecting anything that is
// not one - including a point off the curve, which is how an invalid-curve
// attack starts.
func parsePublic(b []byte) (*ecdh.PublicKey, error) {
	if len(b) != keyLen {
		return nil, fmt.Errorf("public key is %d bytes, want %d", len(b), keyLen)
	}
	return ecdh.P256().NewPublicKey(b)
}

// Encrypt seals a payload for one subscription, with a fresh key pair and a
// fresh salt per message - which RFC 8291 requires, since the pair is what
// makes the derived key unique for a message.
func Encrypt(sub Subscription, plaintext []byte) ([]byte, error) {
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return encrypt(sub, plaintext, salt, as)
}

// encrypt is Encrypt with the two random inputs supplied, so the RFC's test
// vector can be reproduced exactly.
func encrypt(sub Subscription, plaintext, salt []byte, as *ecdh.PrivateKey) ([]byte, error) {
	if len(plaintext) > MaxPayload {
		return nil, fmt.Errorf("push: payload is %d bytes, at most %d fit one message",
			len(plaintext), MaxPayload)
	}
	if len(salt) != saltLen {
		return nil, fmt.Errorf("push: salt is %d bytes, want %d", len(salt), saltLen)
	}
	uaBytes, err := b64.DecodeString(sub.P256dh)
	if err != nil {
		return nil, fmt.Errorf("%w: p256dh: %w", ErrInvalidSubscription, err)
	}
	ua, err := parsePublic(uaBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: p256dh: %w", ErrInvalidSubscription, err)
	}
	auth, err := b64.DecodeString(sub.Auth)
	if err != nil {
		return nil, fmt.Errorf("%w: auth: %w", ErrInvalidSubscription, err)
	}
	if len(auth) != authLen {
		return nil, fmt.Errorf("%w: auth is %d bytes, want %d", ErrInvalidSubscription, len(auth), authLen)
	}

	shared, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	key, nonce, err := derive(shared, ua.Bytes(), as.PublicKey().Bytes(), auth, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	asPub := as.PublicKey().Bytes()
	body := make([]byte, 0, headerLen+len(plaintext)+1+gcm.Overhead())
	body = append(body, salt...)
	body = binary.BigEndian.AppendUint32(body, recordSize)
	body = append(body, byte(len(asPub)))
	body = append(body, asPub...)

	record := make([]byte, 0, len(plaintext)+1)
	record = append(record, plaintext...)
	record = append(record, delimiter)
	return gcm.Seal(body, nonce, record, nil), nil
}

// derive turns the ECDH shared secret, both public keys, the subscription's
// auth secret and the message salt into this message's content encryption key
// and nonce. It takes the shared secret rather than the keys that produced it
// so both ends of the exchange derive through the same code.
//
// The two-stage shape is the whole point of RFC 8291 and worth stating: the
// first extraction mixes in auth, a secret the push service never sees, so a
// service that learns both public keys still cannot derive the key. The second
// is plain RFC 8188 keyed by that result.
func derive(shared, uaPub, asPub, auth, salt []byte) (key, nonce []byte, err error) {
	keyInfo := make([]byte, 0, len("WebPush: info")+1+len(uaPub)+len(asPub))
	keyInfo = append(keyInfo, "WebPush: info"...)
	keyInfo = append(keyInfo, 0)
	keyInfo = append(keyInfo, uaPub...)
	keyInfo = append(keyInfo, asPub...)

	ikm, err := hkdf.Key(sha256.New, shared, auth, string(keyInfo), 32)
	if err != nil {
		return nil, nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, err
	}
	key, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, nil, err
	}
	nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, nil, err
	}
	return key, nonce, nil
}
