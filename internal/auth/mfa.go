package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/pquerna/otp/totp"
)

func EncryptTOTPSecret(encodedKey, secret string) (string, error) {
	gcm, err := mfaGCM(encodedKey)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(secret), nil)), nil
}

func DecryptTOTPSecret(encodedKey, ciphertext string) (string, error) {
	gcm, err := mfaGCM(encodedKey)
	if err != nil {
		return "", err
	}
	sealed, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil || len(sealed) < gcm.NonceSize() {
		return "", fmt.Errorf("invalid encrypted TOTP secret")
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt TOTP secret: %w", err)
	}
	return string(plain), nil
}

func mfaGCM(encodedKey string) (cipher.AEAD, error) {
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("MFA encryption key must decode to 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func ValidateTOTP(secret, code string) bool {
	return totp.Validate(strings.TrimSpace(code), secret)
}

func GenerateRecoveryCodes() ([]string, []string, error) {
	plain := make([]string, 10)
	hashes := make([]string, 10)
	for i := range plain {
		raw := make([]byte, 10)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, err
		}
		compact := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
		plain[i] = compact[:4] + "-" + compact[4:8] + "-" + compact[8:12] + "-" + compact[12:]
		hashes[i] = RecoveryCodeHash(plain[i])
	}
	return plain, hashes, nil
}

func RecoveryCodeHash(code string) string {
	normalized := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}
