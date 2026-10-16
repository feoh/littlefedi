package federation

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// KeyAlgorithm identifies the asymmetric algorithm backing a federation key.
type KeyAlgorithm int

const (
	// KeyRSA is RSA PKCS#1 v1.5, the historical ActivityPub HTTP Signature and
	// RsaSignature2017 algorithm.
	KeyRSA KeyAlgorithm = iota
	// KeyEd25519 is Ed25519, used for Object Integrity Proofs (FEP-8b32) and
	// ed25519 HTTP Signatures.
	KeyEd25519
)

func (k KeyAlgorithm) String() string {
	switch k {
	case KeyRSA:
		return "rsa"
	case KeyEd25519:
		return "ed25519"
	default:
		return fmt.Sprintf("key(%d)", int(k))
	}
}

// PublicKey is a key-agnostic public key: exactly one of RSA or Ed25519 is
// set, selected by Algorithm.
type PublicKey struct {
	Algorithm KeyAlgorithm
	RSA       *rsa.PublicKey
	Ed25519   ed25519.PublicKey
}

// ParsePublicKeyPEM parses a PEM public key that is either an RSA PKIX key or
// an Ed25519 PKIX key. This is the single normalization point for public keys
// in every wire format: PEM, Mastodon publicKeyBase64, or GoToSocial
// publicKeyMultibase.
func ParsePublicKeyPEM(publicKeyPEM string) (*PublicKey, error) {
	block, _ := pem.Decode([]byte(publicKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("failed to decode public key PEM")
	}
	return ParsePublicKeyPKIX(block.Bytes)
}

// ParsePublicKeyPKIX parses a DER-encoded PKIX SubjectPublicKeyInfo that is
// either RSA or Ed25519.
func ParsePublicKeyPKIX(der []byte) (*PublicKey, error) {
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	switch p := pub.(type) {
	case *rsa.PublicKey:
		return &PublicKey{Algorithm: KeyRSA, RSA: p}, nil
	case ed25519.PublicKey:
		return &PublicKey{Algorithm: KeyEd25519, Ed25519: p}, nil
	default:
		return nil, fmt.Errorf("unsupported public key type %T", pub)
	}
}

// ParsePrivateKeyPEM parses a PEM private key: PKCS1 or PKCS8 RSA, or PKCS8
// Ed25519. It returns the key as a crypto.Signer plus its algorithm, so the
// caller never has to branch on encoding.
func ParsePrivateKeyPEM(privateKeyPEM string) (crypto.Signer, KeyAlgorithm, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, KeyRSA, fmt.Errorf("failed to decode private key PEM")
	}

	if priv, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return priv, KeyRSA, nil
	}

	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, KeyRSA, fmt.Errorf("parse private key: %w", err)
	}
	switch priv := key.(type) {
	case *rsa.PrivateKey:
		return priv, KeyRSA, nil
	case ed25519.PrivateKey:
		return priv, KeyEd25519, nil
	default:
		return nil, KeyRSA, fmt.Errorf("unsupported private key type %T", key)
	}
}
