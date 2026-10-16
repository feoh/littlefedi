package keycrypt

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func newKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestSealOpenWithGlobalKey(t *testing.T) {
	t.Cleanup(func() { _ = SetKey("") })
	if err := SetKey(newKey(t)); err != nil {
		t.Fatalf("SetKey: %v", err)
	}
	if !Enabled() {
		t.Fatal("Enabled() = false after SetKey")
	}
	const secret = "-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----"
	sealed, err := Seal(secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !IsSealed(sealed) || strings.Contains(sealed, "PRIVATE KEY") {
		t.Fatalf("Seal did not produce a sealed value: %q", sealed)
	}
	again, err := Seal(sealed)
	if err != nil || again != sealed {
		t.Error("sealing an already sealed value should be a no-op")
	}
	plain, err := Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if plain != secret {
		t.Errorf("Open = %q, want %q", plain, secret)
	}
}

func TestSealWithoutKeyIsPassthrough(t *testing.T) {
	_ = SetKey("")
	if Enabled() {
		t.Fatal("Enabled() = true with no key")
	}
	got, err := Seal("plain")
	if err != nil || got != "plain" {
		t.Errorf("Seal without a key = %q, %v; want passthrough", got, err)
	}
	got, err = Open("plain")
	if err != nil || got != "plain" {
		t.Errorf("Open of an unsealed value = %q, %v; want passthrough", got, err)
	}
}

func TestSealWithExplicitAEAD(t *testing.T) {
	aead, err := NewAEAD(newKey(t))
	if err != nil {
		t.Fatalf("NewAEAD: %v", err)
	}
	sealed, err := SealWith(aead, "value")
	if err != nil {
		t.Fatalf("SealWith: %v", err)
	}
	plain, err := OpenWith(aead, sealed)
	if err != nil || plain != "value" {
		t.Fatalf("OpenWith = %q, %v", plain, err)
	}
	other, _ := NewAEAD(newKey(t))
	if _, err := OpenWith(other, sealed); err == nil {
		t.Error("OpenWith succeeded with the wrong key")
	}
}

func TestNewAEADRejectsShortKey(t *testing.T) {
	if _, err := NewAEAD(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Error("NewAEAD accepted a 5-byte key")
	}
}
