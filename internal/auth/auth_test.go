package auth

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func TestHashAndCheckPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "correct horse battery staple" {
		t.Fatal("HashPassword returned the plaintext")
	}
	if !CheckPassword("correct horse battery staple", hash) {
		t.Error("CheckPassword rejected the right password")
	}
	if CheckPassword("wrong password", hash) {
		t.Error("CheckPassword accepted the wrong password")
	}
}

func testKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestTOTPSecretRoundTrip(t *testing.T) {
	key := testKey(t)
	sealed, err := EncryptTOTPSecret(key, "JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatalf("EncryptTOTPSecret: %v", err)
	}
	if strings.Contains(sealed, "JBSWY3DPEHPK3PXP") {
		t.Fatal("ciphertext contains the secret")
	}
	plain, err := DecryptTOTPSecret(key, sealed)
	if err != nil {
		t.Fatalf("DecryptTOTPSecret: %v", err)
	}
	if plain != "JBSWY3DPEHPK3PXP" {
		t.Errorf("round trip = %q", plain)
	}
	if _, err := DecryptTOTPSecret(testKey(t), sealed); err == nil {
		t.Error("decrypting with a different key succeeded")
	}
}

func TestValidateTOTP(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP"
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !ValidateTOTP(secret, " "+code+" ") {
		t.Error("current code with surrounding spaces was rejected")
	}
	if ValidateTOTP(secret, "000000") && code != "000000" {
		t.Error("an arbitrary code was accepted")
	}
}

func TestGenerateRecoveryCodes(t *testing.T) {
	plain, hashes, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	if len(plain) != 10 || len(hashes) != 10 {
		t.Fatalf("got %d codes and %d hashes, want 10 each", len(plain), len(hashes))
	}
	seen := map[string]bool{}
	for i, code := range plain {
		if strings.Count(code, "-") != 3 {
			t.Errorf("code %q is not four dash-separated groups", code)
		}
		if seen[code] {
			t.Errorf("duplicate code %q", code)
		}
		seen[code] = true
		if hashes[i] != RecoveryCodeHash(code) {
			t.Errorf("hash %d does not match its code", i)
		}
	}
}

func TestRecoveryCodeHashNormalizes(t *testing.T) {
	want := RecoveryCodeHash("ABCD-EFGH-IJKL-MNOP")
	for _, variant := range []string{"abcd-efgh-ijkl-mnop", " ABCDEFGHIJKLMNOP ", "abcd efgh ijkl mnop"} {
		if got := RecoveryCodeHash(variant); got != want {
			t.Errorf("RecoveryCodeHash(%q) differs from the canonical form", variant)
		}
	}
}
