// Package pushtest reads a Web Push message the way a browser does.
//
// It is the receiving half, and it is deliberately a second implementation
// rather than a call into internal/push: a sender checked against its own
// derivation is self-consistent and unreadable to every browser. This one is
// pinned by the RFC 8291 vector - internal/push's round-trip test decrypts
// through here - so a test that reads a payload back is reading it the way the
// RFC says, not the way the sender happens to write it.
//
// Nothing in the daemon uses it. Two test suites do: the encoding's own, and
// the notifier's, which is the only place the payload a person will see is
// assembled.
package pushtest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// Header layout, RFC 8188: salt, record size, key length, key.
const (
	saltLen   = 16
	keyLen    = 65
	headerLen = saltLen + 4 + 1 + keyLen
	// delimiter marks the last record of a body, RFC 8188 section 2.
	delimiter = 0x02
)

// Decrypt reads a body addressed to this subscription. priv is the
// subscription's private key and auth its shared secret.
func Decrypt(priv *ecdh.PrivateKey, auth, body []byte) ([]byte, error) {
	if len(body) < headerLen {
		return nil, fmt.Errorf("pushtest: body is %d bytes, shorter than a header", len(body))
	}
	salt := body[:saltLen]
	if rs := binary.BigEndian.Uint32(body[saltLen : saltLen+4]); rs < uint32(len(body)) {
		return nil, fmt.Errorf("pushtest: body is longer than the record size it declares (%d)", rs)
	}
	if n := int(body[saltLen+4]); n != keyLen {
		return nil, fmt.Errorf("pushtest: key length byte is %d, want %d", n, keyLen)
	}
	asPub := body[saltLen+5 : headerLen]
	as, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		return nil, fmt.Errorf("pushtest: sender's key: %w", err)
	}
	shared, err := priv.ECDH(as)
	if err != nil {
		return nil, fmt.Errorf("pushtest: ecdh: %w", err)
	}

	uaPub := priv.PublicKey().Bytes()
	info := make([]byte, 0, len("WebPush: info")+1+len(uaPub)+len(asPub))
	info = append(info, "WebPush: info"...)
	info = append(info, 0)
	info = append(info, uaPub...)
	info = append(info, asPub...)

	ikm, err := hkdf.Key(sha256.New, shared, auth, string(info), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	key, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
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
	record, err := gcm.Open(nil, nonce, body[headerLen:], nil)
	if err != nil {
		return nil, fmt.Errorf("pushtest: %w", err)
	}
	if len(record) == 0 || record[len(record)-1] != delimiter {
		return nil, errors.New("pushtest: the record does not end in the last-record delimiter")
	}
	return record[:len(record)-1], nil
}
