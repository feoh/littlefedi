// Package keycrypt seals secrets that have to survive in the database but
// must not be readable from a stolen dump: today the ActivityPub private keys
// of local accounts.
//
// The key lives in the environment, never in the config file, so a leaked
// littlefedi.toml is not enough to unseal a backup. Losing it is fatal to the
// sealed values, which is why unsealing never falls back to anything: an
// account whose signing key cannot be read must stop signing, not quietly
// federate under a freshly generated identity that no remote server has ever
// seen.
package keycrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
)

// prefix marks a sealed value. Anything else is stored as it was written,
// which keeps plaintext databases readable after the feature is turned on and
// lets an operator turn it back off.
const prefix = "lfk1:"

var (
	mu         sync.RWMutex
	cipherAEAD cipher.AEAD
)

// SetKey installs the sealing key, given as base64 of 32 raw bytes. An empty
// key disables sealing without touching values already stored.
func SetKey(encodedKey string) error {
	if strings.TrimSpace(encodedKey) == "" {
		mu.Lock()
		cipherAEAD = nil
		mu.Unlock()
		return nil
	}

	aead, err := newAEAD(encodedKey)
	if err != nil {
		return err
	}
	mu.Lock()
	cipherAEAD = aead
	mu.Unlock()
	return nil
}

// Enabled reports whether a key is installed.
func Enabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	return cipherAEAD != nil
}

// NewAEAD validates a base64 key and returns the cipher it produces, without
// installing it. Used by config validation so a bad key is caught at startup
// rather than on the first signature.
func NewAEAD(encodedKey string) (cipher.AEAD, error) {
	return newAEAD(encodedKey)
}

func newAEAD(encodedKey string) (cipher.AEAD, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encodedKey))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("keycrypt: key must decode to 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal returns the sealed form of value. With no key installed, or on a value
// that is already sealed, it returns the input unchanged so callers can seal
// unconditionally.
func Seal(value string) (string, error) {
	if value == "" || IsSealed(value) {
		return value, nil
	}
	mu.RLock()
	aead := cipherAEAD
	mu.RUnlock()
	if aead == nil {
		return value, nil
	}
	return SealWith(aead, value)
}

// SealWith seals value with an explicit cipher, for the admin commands that
// have to work with a key the running process has not installed.
func SealWith(aead cipher.AEAD, value string) (string, error) {
	if value == "" || IsSealed(value) {
		return value, nil
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("keycrypt: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, []byte(value), nil)
	return prefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Open returns the plaintext behind value. Values that were never sealed pass
// through untouched, so a database written before sealing was turned on keeps
// working. A sealed value with no key installed is an error: carrying on would
// mean signing with something that is not the account's key.
func Open(value string) (string, error) {
	if !IsSealed(value) {
		return value, nil
	}
	mu.RLock()
	aead := cipherAEAD
	mu.RUnlock()
	if aead == nil {
		return "", fmt.Errorf("keycrypt: value is sealed but no key is configured (set LITTLEFEDI_FEDERATION_KEY_ENCRYPTION_KEY)")
	}
	return OpenWith(aead, value)
}

// OpenWith unseals value with an explicit cipher.
func OpenWith(aead cipher.AEAD, value string) (string, error) {
	if !IsSealed(value) {
		return value, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", fmt.Errorf("keycrypt: sealed value is not valid base64")
	}
	if len(raw) < aead.NonceSize() {
		return "", fmt.Errorf("keycrypt: sealed value is truncated")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("keycrypt: cannot unseal value, wrong key or tampered data")
	}
	return string(plain), nil
}

// IsSealed reports whether value carries the sealed marker.
func IsSealed(value string) bool {
	return strings.HasPrefix(value, prefix)
}
