package federation

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

type actorPublicKeys struct {
	publicKeyPEM        string
	publicKeyID         string
	ed25519PublicKeyPEM string
	ed25519PublicKeyID  string
	byID                map[string]string
}

// PublicKeyPEMForID returns only a key the actor actually advertised under
// keyID. Empty IDs are tolerated solely for legacy main keys and local OIP
// keys created before the key-ID columns existed.
func PublicKeyPEMForID(account *models.Account, keyID string) (string, error) {
	if account == nil || keyID == "" {
		return "", fmt.Errorf("missing account or key ID")
	}
	var advertised map[string]string
	if json.Unmarshal([]byte(account.PublicKeysJSON), &advertised) == nil && advertised[keyID] != "" {
		return advertised[keyID], nil
	}
	if account.Ed25519PublicKeyID == keyID && account.Ed25519PublicKeyPem != "" {
		return account.Ed25519PublicKeyPem, nil
	}
	if account.PublicKeyID == keyID && account.PublicKeyPem != "" {
		return account.PublicKeyPem, nil
	}
	if account.Domain == nil && account.PublicKeyID == "" && account.PublicKeyPem != "" &&
		(keyID == account.ActorURI+"#main-key" || keyID == account.ActorURI+"/main-key") {
		return account.PublicKeyPem, nil
	}
	if account.Domain == nil && account.Ed25519PublicKeyID == "" && account.Ed25519PublicKeyPem != "" &&
		keyID == account.ActorURI+"#ed25519-key" {
		return account.Ed25519PublicKeyPem, nil
	}
	return "", fmt.Errorf("key %q is not advertised by actor %q", keyID, account.ActorURI)
}

func (keys *actorPublicKeys) add(keyID, publicKeyPEM string, algorithm KeyAlgorithm) {
	if keyID != "" {
		if keys.byID == nil {
			keys.byID = make(map[string]string)
		}
		keys.byID[keyID] = publicKeyPEM
	}
	if keys.publicKeyPEM == "" {
		keys.publicKeyPEM, keys.publicKeyID = publicKeyPEM, keyID
	}
	if algorithm == KeyEd25519 && keys.ed25519PublicKeyPEM == "" {
		keys.ed25519PublicKeyPEM, keys.ed25519PublicKeyID = publicKeyPEM, keyID
	}
}

func (keys actorPublicKeys) json() string {
	b, _ := json.Marshal(keys.byID)
	if string(b) == "null" {
		return "{}"
	}
	return string(b)
}

// extractActorPublicKeys returns normalized PKIX PEM keys and their exact IDs
// from an actor document. Valid keys survive malformed optional key entries.
//
// Every wire format in use across the fediverse is handled:
//
//   - publicKey.publicKeyPem (RSA or Ed25519 PEM) - Mastodon, GoToSocial, littleFedi
//   - publicKey.keyType "Ed25519" + publicKeyBase64 (Mastodon 4.3+)
//   - publicKey.publicKeyMultibase (GoToSocial)
//   - assertionMethod[].publicKeyMultibase (Mastodon/GTS OIP key, type Multikey)
//
// Either PEM may be empty when the actor does not advertise that key. The PEMs
// are the canonical stored forms, so DB schema and the Verifier never see the
// wire-specific encodings.
func extractActorPublicKeys(doc map[string]interface{}) (keys actorPublicKeys, err error) {
	actorID, _ := doc["id"].(string)
	var errs []error
	for _, pk := range objectMaps(doc["publicKey"]) {
		keyID, _ := pk["id"].(string)
		if owner, _ := pk["owner"].(string); owner != "" && actorID != "" && owner != actorID {
			errs = append(errs, fmt.Errorf("publicKey %q owner %q does not match actor %q", keyID, owner, actorID))
			continue
		}
		if pemStr, ok := pk["publicKeyPem"].(string); ok && pemStr != "" {
			parsed, parseErr := ParsePublicKeyPEM(pemStr)
			if parseErr != nil {
				errs = append(errs, fmt.Errorf("parse publicKeyPem: %w", parseErr))
				continue
			}
			keys.add(keyID, pemStr, parsed.Algorithm)
		}
		if keyType, _ := pk["keyType"].(string); strings.EqualFold(keyType, "Ed25519") {
			if b64, ok := pk["publicKeyBase64"].(string); ok && b64 != "" {
				raw, decodeErr := base64.StdEncoding.DecodeString(b64)
				if decodeErr != nil {
					errs = append(errs, fmt.Errorf("decode publicKeyBase64: %w", decodeErr))
					continue
				}
				ed, keyErr := ed25519RawToPEM(raw)
				if keyErr != nil {
					errs = append(errs, keyErr)
					continue
				}
				keys.add(keyID, ed, KeyEd25519)
			}
		}
		if mb, ok := pk["publicKeyMultibase"].(string); ok && mb != "" {
			ed, keyErr := ed25519MultikeyToPEM(mb)
			if keyErr != nil {
				errs = append(errs, fmt.Errorf("decode publicKeyMultibase: %w", keyErr))
				continue
			}
			keys.add(keyID, ed, KeyEd25519)
		}
	}

	// OIP key advertised in assertionMethod (Mastodon 4.4+ / GoToSocial).
	for _, entry := range objectMaps(doc["assertionMethod"]) {
		if typ, _ := entry["type"].(string); typ != "Multikey" {
			continue
		}
		keyID, _ := entry["id"].(string)
		controller, _ := entry["controller"].(string)
		if keyID == "" || controller == "" || actorID != "" && controller != actorID {
			errs = append(errs, fmt.Errorf("invalid assertionMethod id/controller"))
			continue
		}
		mb, ok := entry["publicKeyMultibase"].(string)
		if !ok || mb == "" {
			continue
		}
		ed, keyErr := ed25519MultikeyToPEM(mb)
		if keyErr != nil {
			errs = append(errs, fmt.Errorf("decode assertionMethod publicKeyMultibase: %w", keyErr))
			continue
		}
		keys.add(keyID, ed, KeyEd25519)
	}

	return keys, errors.Join(errs...)
}

func objectMaps(v interface{}) []map[string]interface{} {
	switch value := v.(type) {
	case map[string]interface{}:
		return []map[string]interface{}{value}
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(value))
		for _, item := range value {
			if object, ok := item.(map[string]interface{}); ok {
				out = append(out, object)
			}
		}
		return out
	default:
		return nil
	}
}

// ed25519RawToPEM wraps a raw 32-byte Ed25519 public key in a PKIX PEM.
func ed25519RawToPEM(raw []byte) (string, error) {
	if len(raw) != ed25519.PublicKeySize {
		return "", fmt.Errorf("ed25519 key size mismatch: got %d want %d", len(raw), ed25519.PublicKeySize)
	}
	der, err := x509.MarshalPKIXPublicKey(ed25519.PublicKey(raw))
	if err != nil {
		return "", fmt.Errorf("marshal ed25519 public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// base58btcAlphabet is the Bitcoin-style base58 alphabet used by multibase
// base58btc, which GoToSocial uses for publicKeyMultibase.
const base58btcAlphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var base58btcIndex = func() [256]int {
	var idx [256]int
	for i := range idx {
		idx[i] = -1
	}
	for i := 0; i < len(base58btcAlphabet); i++ {
		idx[base58btcAlphabet[i]] = i
	}
	return idx
}()

var base58btcRadix = big.NewInt(58)

// base58btcEncode encodes bytes in base58btc. A leading 'z' (the multibase
// prefix for base58btc) is prepended, matching the encoding GoToSocial and
// Mastodon use for publicKeyMultibase.
func base58btcEncode(input []byte) string {
	zeros := 0
	for zeros < len(input) && input[zeros] == 0 {
		zeros++
	}

	n := new(big.Int).SetBytes(input[zeros:])
	mod := new(big.Int)
	out := make([]byte, 0, len(input)*2)
	for n.Sign() > 0 {
		n.DivMod(n, base58btcRadix, mod)
		out = append(out, base58btcAlphabet[mod.Int64()])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}

	result := make([]byte, 0, zeros+len(out)+1)
	result = append(result, 'z')
	for i := 0; i < zeros; i++ {
		result = append(result, '1')
	}
	return string(append(result, out...))
}

// base58btcDecode decodes a base58btc string. A leading 'z' (the multibase
// prefix for base58btc) is stripped. Leading '1' characters decode to leading
// zero bytes, matching the standard base58 encoding rules.
func base58btcDecode(input string) ([]byte, error) {
	// Ed25519 signatures are the largest values decoded here: 64 bytes encode
	// to at most 88 base58 digits. Bound before big.Int work so hostile inbox
	// input cannot turn a 1 MiB request into quadratic CPU time.
	if len(input) > 89 {
		return nil, fmt.Errorf("base58btc value is too long")
	}
	if !strings.HasPrefix(input, "z") {
		return nil, fmt.Errorf("base58btc value must start with z")
	}
	input = input[1:]
	if input == "" {
		return nil, fmt.Errorf("empty base58 input")
	}

	zeros := 0
	for zeros < len(input) && input[zeros] == '1' {
		zeros++
	}

	n := new(big.Int)
	for _, c := range input[zeros:] {
		if c >= 256 {
			return nil, fmt.Errorf("invalid base58 character %q", c)
		}
		digit := base58btcIndex[c]
		if digit < 0 {
			return nil, fmt.Errorf("invalid base58 character %q", c)
		}
		n.Mul(n, base58btcRadix)
		n.Add(n, big.NewInt(int64(digit)))
	}

	out := n.Bytes()
	if zeros > 0 {
		z := make([]byte, zeros)
		out = append(z, out...)
	}
	return out, nil
}

func ed25519MultikeyToPEM(multibase string) (string, error) {
	raw, err := base58btcDecode(multibase)
	if err != nil {
		return "", err
	}
	if len(raw) != 2+ed25519.PublicKeySize || raw[0] != 0xed || raw[1] != 0x01 {
		return "", fmt.Errorf("invalid Ed25519 Multikey prefix or length")
	}
	return ed25519RawToPEM(raw[2:])
}

// ed25519PEMToRaw extracts the raw 32-byte Ed25519 public key from a PKIX PEM.
// Returns nil when the PEM is not an Ed25519 public key.
func ed25519PEMToRaw(publicKeyPEM string) []byte {
	pub, err := ParsePublicKeyPEM(publicKeyPEM)
	if err != nil || pub.Algorithm != KeyEd25519 {
		return nil
	}
	return pub.Ed25519
}

// actorPublicKeyBlock builds the unambiguous legacy RSA publicKey block.
// Ed25519 is advertised separately through assertionMethod.
func actorPublicKeyBlock(actorURI, rsaPublicKeyPEM string) map[string]interface{} {
	return map[string]interface{}{
		"id":           actorURI + "#main-key",
		"type":         "Key",
		"owner":        actorURI,
		"publicKeyPem": rsaPublicKeyPEM,
	}
}

// actorAssertionMethod builds the FEP-8b32 assertionMethod array advertising
// the actor's Ed25519 key as a Multikey with publicKeyMultibase, the form
// GoToSocial and Mastodon 4.4+ read to verify Object Integrity Proofs. Returns
// nil when the actor has no Ed25519 pair.
func actorAssertionMethod(actorURI, ed25519PublicKeyPEM string) []interface{} {
	raw := ed25519PEMToRaw(ed25519PublicKeyPEM)
	if raw == nil {
		return nil
	}
	return []interface{}{
		map[string]interface{}{
			"id":                 actorURI + "#ed25519-key",
			"type":               "Multikey",
			"controller":         actorURI,
			"publicKeyMultibase": base58btcEncode(append([]byte{0xed, 0x01}, raw...)),
		},
	}
}
