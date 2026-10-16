package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/gowebpki/jcs"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/keycrypt"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// Object Integrity Proofs (FEP-8b32) with the eddsa-jcs-2022 cryptosuite.
//
// The signing input is the concatenation of two SHA-256 digests, signed with
// Ed25519:
//
//	createHash(canonicalized proof configuration) || createHash(canonicalized document)
//
// where both pieces are canonicalized with JCS (RFC 8785) and the document has
// its `proof` (and, when present, legacy `signature`) removed. The proofValue
// is the base58btc multibase encoding of the 64-byte signature. The algorithm
// is validated against the official FEP-8b32 test vector
// (fep/8b32/fep-8b32.feature).

const dataIntegrityContext = "https://w3id.org/security/data-integrity/v2"

// oipCanonicalHash returns the SHA-256 of the JCS canonical form of v.
func oipCanonicalHash(v interface{}) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("oip: marshal: %w", err)
	}
	canon, err := jcs.Transform(b)
	if err != nil {
		return nil, fmt.Errorf("oip: canonicalize: %w", err)
	}
	sum := sha256.Sum256(canon)
	return sum[:], nil
}

// oipSigningInput builds the FEP-8b32 signing input for a proof configuration
// (proof with proofValue removed) and a document (with proof and signature
// removed). The proof configuration hash comes first, then the document hash.
func oipSigningInput(proofConfig, doc interface{}) ([]byte, error) {
	proofHash, err := oipCanonicalHash(proofConfig)
	if err != nil {
		return nil, err
	}
	docHash, err := oipCanonicalHash(doc)
	if err != nil {
		return nil, err
	}
	return append(proofHash, docHash...), nil
}

// oipCloneMap deep-copies a JSON object via a marshal/unmarshal round trip.
func oipCloneMap(m map[string]interface{}) (map[string]interface{}, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// signOutboundOIP gates attaching an Object Integrity Proof to outbound
// activities. It defaults to off; main.go enables it from
// federation.sign_with_oip.
var signOutboundOIP atomic.Bool

// SetSignOutboundOIP enables or disables attaching FEP-8b32 proofs to
// outbound activities.
func SetSignOutboundOIP(enabled bool) {
	signOutboundOIP.Store(enabled)
}

// SignOutboundOIPEnabled reports whether outbound OIP signing is enabled.
func SignOutboundOIPEnabled() bool {
	return signOutboundOIP.Load()
}

// maybeSignWithOIP attaches an FEP-8b32 Object Integrity Proof to an outbound
// activity when enabled and the activity's local actor has an Ed25519 pair.
// It returns the (possibly new) document; on any missing piece it falls back
// to the HTTP Signature alone, which stays the baseline authentication.
func maybeSignWithOIP(ctx context.Context, store storage.Storage, activity map[string]interface{}) (map[string]interface{}, error) {
	if !signOutboundOIP.Load() {
		return activity, nil
	}
	actorURI, _ := activity["actor"].(string)
	if actorURI == "" {
		return activity, nil
	}
	account, err := store.GetAccountByURI(ctx, actorURI)
	if err != nil || account == nil || account.Domain != nil {
		return activity, nil
	}
	if account.Ed25519PrivateKeyPem == "" {
		return activity, nil
	}
	keyID := account.Ed25519PublicKeyID
	if keyID == "" {
		keyID = account.ActorURI + "#ed25519-key"
	}
	// The activity comes back untouched on failure: one caller assigns the
	// first return value even when the error is non-nil, and an unsigned
	// activity still has its HTTP Signature.
	privateKeyPEM, err := keycrypt.Open(account.Ed25519PrivateKeyPem)
	if err != nil {
		return activity, err
	}
	return SignWithObjectIntegrityProof(activity, privateKeyPEM, keyID)
}

// VerifyObjectIntegrityProof verifies the FEP-8b32 `proof` of an activity or
// object document against the owner's Ed25519 public key (PKIX PEM). It
// returns nil on success and an error otherwise, so callers can distinguish "no
// proof / unsupported suite" from "proof present but invalid".
func VerifyObjectIntegrityProof(doc map[string]interface{}, ed25519PublicKeyPEM string) error {
	rawProof, ok := doc["proof"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("oip: no proof block")
	}

	proofValue, _ := rawProof["proofValue"].(string)
	if proofValue == "" {
		return fmt.Errorf("oip: missing proofValue")
	}

	proof, err := oipCloneMap(rawProof)
	if err != nil {
		return fmt.Errorf("oip: copy proof: %w", err)
	}
	delete(proof, "proofValue")

	proofType, _ := proof["type"].(string)
	if proofType != "DataIntegrityProof" {
		return fmt.Errorf("oip: unsupported proof type %q", proofType)
	}
	suite, _ := proof["cryptosuite"].(string)
	if suite != "eddsa-jcs-2022" {
		return fmt.Errorf("oip: unsupported cryptosuite %q", suite)
	}
	purpose, _ := proof["proofPurpose"].(string)
	if purpose != "assertionMethod" {
		return fmt.Errorf("oip: unsupported proofPurpose %q", purpose)
	}
	verificationMethod, _ := proof["verificationMethod"].(string)
	if parsed, err := url.Parse(verificationMethod); verificationMethod == "" || err != nil || !parsed.IsAbs() {
		return fmt.Errorf("oip: invalid verificationMethod %q", verificationMethod)
	}
	if created, exists := proof["created"]; exists {
		value, ok := created.(string)
		if !ok || value == "" {
			return fmt.Errorf("oip: invalid created timestamp")
		}
		if _, err := time.Parse(time.RFC3339, value); err != nil {
			return fmt.Errorf("oip: invalid created timestamp: %w", err)
		}
	}
	if expires, exists := proof["expires"]; exists {
		value, ok := expires.(string)
		if !ok || value == "" {
			return fmt.Errorf("oip: invalid expires timestamp")
		}
		t, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return fmt.Errorf("oip: invalid expires timestamp: %w", err)
		}
		if time.Now().After(t) {
			return fmt.Errorf("oip: proof expired at %s", value)
		}
	}

	// FEP backward compatibility: remove the legacy linked data signature
	// before verifying the integrity proof.
	docCopy, err := oipCloneMap(doc)
	if err != nil {
		return fmt.Errorf("oip: copy document: %w", err)
	}
	delete(docCopy, "proof")
	delete(docCopy, "signature")
	if proofContext, ok := proof["@context"]; ok {
		if !contextStartsWith(docCopy["@context"], proofContext) {
			return fmt.Errorf("oip: proof context is not a prefix of document context")
		}
		docCopy["@context"] = proofContext
	}

	signingInput, err := oipSigningInput(proof, docCopy)
	if err != nil {
		return err
	}

	sig, err := base58btcDecode(proofValue)
	if err != nil {
		return fmt.Errorf("oip: decode proofValue: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("oip: invalid signature length %d", len(sig))
	}

	pub, err := ParsePublicKeyPEM(ed25519PublicKeyPEM)
	if err != nil {
		return fmt.Errorf("oip: parse key: %w", err)
	}
	if pub.Algorithm != KeyEd25519 {
		return fmt.Errorf("oip: proof key is not Ed25519")
	}
	if !ed25519.Verify(pub.Ed25519, signingInput, sig) {
		return fmt.Errorf("oip: proof verification failed")
	}
	return nil
}

func contextStartsWith(document, proof interface{}) bool {
	docValues := contextValues(document)
	proofValues := contextValues(proof)
	if len(proofValues) == 0 || len(docValues) < len(proofValues) {
		return false
	}
	for i := range proofValues {
		if !reflect.DeepEqual(docValues[i], proofValues[i]) {
			return false
		}
	}
	return true
}

func contextValues(value interface{}) []interface{} {
	if values, ok := value.([]interface{}); ok {
		return values
	}
	if value == nil {
		return nil
	}
	return []interface{}{value}
}

func ensureDataIntegrityContext(doc map[string]interface{}) {
	values := contextValues(doc["@context"])
	for _, value := range values {
		if value == dataIntegrityContext {
			return
		}
	}
	doc["@context"] = append(values, dataIntegrityContext)
}

// SignWithObjectIntegrityProof attaches an FEP-8b32 Object Integrity Proof to
// a document and returns the new document (the input is not mutated). The
// Ed25519 private key must belong to the local actor identified by
// verificationMethod, whose Ed25519 public key is advertised under
// `assertionMethod` so peers can resolve it.
func SignWithObjectIntegrityProof(doc map[string]interface{}, ed25519PrivateKeyPEM, verificationMethod string) (map[string]interface{}, error) {
	return signWithObjectIntegrityProofAt(doc, ed25519PrivateKeyPEM, verificationMethod, time.Now())
}

// signWithObjectIntegrityProofAt is SignWithObjectIntegrityProof with a caller
// controlled proof creation time, so known-answer tests can reproduce the
// official FEP-8b32 vector.
func signWithObjectIntegrityProofAt(doc map[string]interface{}, ed25519PrivateKeyPEM, verificationMethod string, created time.Time) (map[string]interface{}, error) {
	if parsed, err := url.Parse(verificationMethod); verificationMethod == "" || err != nil || !parsed.IsAbs() {
		return nil, fmt.Errorf("oip: invalid verificationMethod %q", verificationMethod)
	}
	priv, alg, err := ParsePrivateKeyPEM(ed25519PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("oip: parse private key: %w", err)
	}
	if alg != KeyEd25519 {
		return nil, fmt.Errorf("oip: signing key is not Ed25519")
	}

	out, err := oipCloneMap(doc)
	if err != nil {
		return nil, fmt.Errorf("oip: copy document: %w", err)
	}
	// FEP: the integrity proof replaces any legacy linked data signature.
	delete(out, "proof")
	delete(out, "signature")
	ensureDataIntegrityContext(out)

	proof := map[string]interface{}{
		"type":               "DataIntegrityProof",
		"cryptosuite":        "eddsa-jcs-2022",
		"verificationMethod": verificationMethod,
		"proofPurpose":       "assertionMethod",
		"created":            created.UTC().Format(time.RFC3339),
	}
	proof["@context"] = out["@context"]

	signingInput, err := oipSigningInput(proof, out)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(priv.(ed25519.PrivateKey), signingInput)
	proof["proofValue"] = base58btcEncode(sig)
	out["proof"] = proof
	return out, nil
}
