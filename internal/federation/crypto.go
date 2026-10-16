// Package federation handles ActivityPub server-to-server communication,
// including HTTP Signatures, actor discovery, and activity delivery.
package federation

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
)

// GenerateKeyPair generates an RSA key pair for ActivityPub signing.
// Returns PEM-encoded private and public keys.
func GenerateKeyPair() (privateKeyPEM, publicKeyPEM string, err error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", fmt.Errorf("generate key: %w", err)
	}

	// Marshal private key
	privBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: privBytes,
	})

	// Marshal public key
	pubBytes, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return "", "", fmt.Errorf("marshal public key: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubBytes,
	})

	return string(privPEM), string(pubPEM), nil
}

// GenerateEd25519KeyPair generates an Ed25519 key pair for Object Integrity
// Proofs (FEP-8b32) and ed25519 HTTP Signatures. Returns PKCS8-encoded
// private and PKIX-encoded public PEM.
func GenerateEd25519KeyPair() (privateKeyPEM, publicKeyPEM string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate ed25519 key: %w", err)
	}

	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", "", fmt.Errorf("marshal ed25519 private key: %w", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: privBytes,
	})

	pubBytes, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", "", fmt.Errorf("marshal ed25519 public key: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubBytes,
	})

	return string(privPEM), string(pubPEM), nil
}

// GenerateDualKeyPair generates both an RSA and an Ed25519 key pair for a new
// local account. The RSA pair remains the primary HTTP Signature key; the
// Ed25519 pair backs Object Integrity Proofs and ed25519 HTTP Signatures.
func GenerateDualKeyPair() (rsaPrivatePEM, rsaPublicPEM, ed25519PrivatePEM, ed25519PublicPEM string, err error) {
	rsaPrivatePEM, rsaPublicPEM, err = GenerateKeyPair()
	if err != nil {
		return "", "", "", "", fmt.Errorf("generate rsa keys: %w", err)
	}
	ed25519PrivatePEM, ed25519PublicPEM, err = GenerateEd25519KeyPair()
	if err != nil {
		return "", "", "", "", fmt.Errorf("generate ed25519 keys: %w", err)
	}
	return
}

// Ed25519PublicKeyPEMFromPrivate derives the PKIX-encoded public key PEM that
// corresponds to a PKCS8-encoded Ed25519 private key PEM.
func Ed25519PublicKeyPEMFromPrivate(privateKeyPEM string) (string, error) {
	signer, algorithm, err := ParsePrivateKeyPEM(privateKeyPEM)
	if err != nil {
		return "", err
	}
	if algorithm != KeyEd25519 {
		return "", fmt.Errorf("private key is not Ed25519")
	}
	der, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return "", fmt.Errorf("marshal ed25519 public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// PublicKeyPEMFromPrivate derives the PKIX-encoded public key PEM that
// corresponds to a PEM-encoded RSA private key (PKCS1 or PKCS8). The output
// format matches GenerateKeyPair exactly, so it can be compared byte-for-byte
// against a stored PublicKeyPem to detect a mismatched key pair.
func PublicKeyPEMFromPrivate(privateKeyPEM string) (string, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return "", fmt.Errorf("failed to decode private key PEM")
	}

	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		key, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return "", fmt.Errorf("parse private key: %v / %v", err, err2)
		}
		var ok bool
		priv, ok = key.(*rsa.PrivateKey)
		if !ok {
			return "", fmt.Errorf("private key is not RSA")
		}
	}

	pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return "", fmt.Errorf("marshal public key: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubBytes,
	})
	return string(pubPEM), nil
}

// Verify verifies a base64-encoded signature against data using the given PEM public key.
func Verify(publicKeyPEM string, data []byte, signatureBase64 string) error {
	block, _ := pem.Decode([]byte(publicKeyPEM))
	if block == nil {
		return fmt.Errorf("failed to decode public key PEM")
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse public key: %w", err)
	}

	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("public key is not RSA")
	}

	sig, err := base64.StdEncoding.DecodeString(signatureBase64)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}

	hash := sha256.Sum256(data)
	return rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, hash[:], sig)
}
