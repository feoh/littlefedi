package federation

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/piprate/json-gold/ld"
)

// Linked Data Signature ("RsaSignature2017") verification.
//
// Mastodon (and compatible software) forwards activities on behalf of a
// third party in a few legitimate cases - most commonly: you follow someone
// who boosted a remote post, and the original author later edits or deletes
// it. The booster's server relays that Update/Delete to your inbox signed
// with its own HTTP Signature key (it is the one actually POSTing to you),
// but the activity body carries a separate signature block proving the true
// author produced it. Rejecting every message where the HTTP signer differs
// from the activity's "actor", which is what plain HTTP Signature
// verification does, silently drops these, so boosted-then-deleted/edited
// content never gets cleaned up locally.
//
// This implements the same algorithm as Mastodon's
// app/lib/activitypub/linked_data_signature.rb and the gating logic in
// app/services/activitypub/process_collection_service.rb#actor_from_verified_ld_signature:
// canonicalize (JSON-LD/URDNA2015) the signature options and the document
// separately, hex-hash each with SHA-256, concatenate the hex strings, and
// RSA-PKCS1v15/SHA-256 verify that against the creator's public key. The
// caller must then still confirm the verified creator matches the
// activity's declared actor (see VerifyLinkedDataSignature below) - a valid
// signature only proves who signed, not that they are who they claim.
//
// Object Integrity Proofs (FEP-8b32) are implemented separately in oip.go and
// take precedence over this legacy suite when present (see handler.go
// tryObjectIntegrityProofFallback). Real-world traffic still overwhelmingly
// uses RsaSignature2017, so this fallback remains for compatibility.

//go:embed ldcontexts/*.json
var ldContextFS embed.FS

// embeddedLDContexts maps the small, fixed set of remote JSON-LD context
// IRIs this verification needs to their bundled copies. Any other IRI fails
// closed (verification is rejected) rather than being fetched over the
// network: a per-message HTTP fetch to an attacker-influenced @context URL
// would be slow on low-power hardware and lets a forged document make us
// reach out to arbitrary hosts.
var embeddedLDContexts = map[string]string{
	"https://www.w3.org/ns/activitystreams": "ldcontexts/activitystreams.json",
	"http://www.w3.org/ns/activitystreams":  "ldcontexts/activitystreams.json",
	"https://w3id.org/security/v1":          "ldcontexts/security-v1.json",
	"http://w3id.org/security/v1":           "ldcontexts/security-v1.json",
	"https://w3id.org/identity/v1":          "ldcontexts/identity-v1.json",
	"http://w3id.org/identity/v1":           "ldcontexts/identity-v1.json",
	// Controlled Identifiers v1, which Mastodon puts in an actor's @context
	// alongside security/v1. Without it every forwarded activity that has to
	// fall back to an LD-Signature fails to canonicalise, and the boosts,
	// deletions and edits arriving through a relay are rejected as an actor
	// mismatch.
	"https://www.w3.org/ns/cid/v1":                 "ldcontexts/cid-v1.json",
	"http://www.w3.org/ns/cid/v1":                  "ldcontexts/cid-v1.json",
	"https://purl.archive.org/socialweb/webfinger": "ldcontexts/webfinger.json",
	"http://purl.archive.org/socialweb/webfinger":  "ldcontexts/webfinger.json",
}

// identityV1Context is the context Mastodon compacts a signature block
// against before hashing it (ActivityPub::LinkedDataSignature::CONTEXT).
const identityV1Context = "https://w3id.org/identity/v1"

// parsedLDContexts holds the embedded contexts pre-parsed once at startup;
// read-only thereafter so it's safe for concurrent use without locking.
var parsedLDContexts = mustParseLDContexts()

func mustParseLDContexts() map[string]*ld.RemoteDocument {
	out := make(map[string]*ld.RemoteDocument, len(embeddedLDContexts))
	seen := make(map[string]*ld.RemoteDocument, 4)
	for url, path := range embeddedLDContexts {
		if doc, ok := seen[path]; ok {
			out[url] = doc
			continue
		}
		raw, err := ldContextFS.ReadFile(path)
		if err != nil {
			panic("federation: embedded LD context missing: " + path + ": " + err.Error())
		}
		var parsed interface{}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			panic("federation: embedded LD context invalid JSON: " + path + ": " + err.Error())
		}
		doc := &ld.RemoteDocument{DocumentURL: url, Document: parsed}
		out[url] = doc
		seen[path] = doc
	}
	return out
}

// embeddedContextLoader implements ld.DocumentLoader using only the
// pre-parsed embedded contexts above.
type embeddedContextLoader struct{}

func (embeddedContextLoader) LoadDocument(u string) (*ld.RemoteDocument, error) {
	if doc, ok := parsedLDContexts[u]; ok {
		return doc, nil
	}
	return nil, fmt.Errorf("ld context %q is not bundled, refusing network fetch", u)
}

func newJSONLDOptions() *ld.JsonLdOptions {
	opts := ld.NewJsonLdOptions("")
	opts.DocumentLoader = embeddedContextLoader{}
	return opts
}

// canonicalHexHash reproduces Mastodon's JsonLdHelper#hash: URDNA2015-
// canonicalize obj (using its own "@context") to N-Quads, then hex-encode
// its SHA-256 digest.
func canonicalHexHash(obj map[string]interface{}) (string, error) {
	opts := newJSONLDOptions()
	opts.Format = "application/n-quads"
	opts.Algorithm = ld.AlgorithmURDNA2015

	normalized, err := ld.NewJsonLdProcessor().Normalize(obj, opts)
	if err != nil {
		return "", err
	}
	nquads, ok := normalized.(string)
	if !ok {
		return "", fmt.Errorf("unexpected normalize result type %T", normalized)
	}
	sum := sha256.Sum256([]byte(nquads))
	return hex.EncodeToString(sum[:]), nil
}

// hasUnsupportedJSONLDFeatures reports whether obj uses @graph, @included or
// @reverse anywhere, recursively. These JSON-LD keywords can change how a
// document expands to RDF in ways that have historically been used to
// construct documents whose signed and processed forms diverge (signature
// confusion); Mastodon rejects them outright before attempting to verify,
// and we do the same.
func hasUnsupportedJSONLDFeatures(v interface{}) bool {
	switch val := v.(type) {
	case map[string]interface{}:
		for k, vv := range val {
			if k == "@graph" || k == "@included" || k == "@reverse" {
				return true
			}
			if hasUnsupportedJSONLDFeatures(vv) {
				return true
			}
		}
	case []interface{}:
		for _, vv := range val {
			if hasUnsupportedJSONLDFeatures(vv) {
				return true
			}
		}
	}
	return false
}

// documentDeclaresActivityStreams reports whether doc's @context includes
// the ActivityStreams IRI, a cheap sanity gate applied before the expensive
// canonicalization work (mirrors Mastodon's supported_context?).
func documentDeclaresActivityStreams(doc map[string]interface{}) bool {
	const as2 = "https://www.w3.org/ns/activitystreams"
	switch ctx := doc["@context"].(type) {
	case string:
		return ctx == as2
	case []interface{}:
		for _, v := range ctx {
			if s, ok := v.(string); ok && s == as2 {
				return true
			}
		}
	}
	return false
}

// VerifyLinkedDataSignature checks the "signature" block of an ActivityPub
// activity per the RsaSignature2017 suite and confirms the verified signer
// matches the activity's declared "actor". On success the activity is
// authenticated as if the actor itself had sent it directly, even though it
// arrived signed (at the HTTP layer) by someone else.
//
// Reuses v.fetchKey and v.parsePublicKey, so LD-Signature verification
// shares the same actor-fetch/cache path (and memoised parsed keys) as HTTP
// Signature verification.
func (v *Verifier) VerifyLinkedDataSignature(doc map[string]interface{}) error {
	if !documentDeclaresActivityStreams(doc) {
		return fmt.Errorf("document does not declare the ActivityStreams context")
	}
	if hasUnsupportedJSONLDFeatures(doc) {
		return fmt.Errorf("document uses unsupported JSON-LD keywords (@graph/@included/@reverse)")
	}

	rawSig, _ := doc["signature"].(map[string]interface{})
	if rawSig == nil {
		return fmt.Errorf("no signature block")
	}

	sigInput := make(map[string]interface{}, len(rawSig)+1)
	for k, v := range rawSig {
		sigInput[k] = v
	}
	sigInput["@context"] = identityV1Context

	sigOptions, err := ld.NewJsonLdProcessor().Compact(sigInput, identityV1Context, newJSONLDOptions())
	if err != nil {
		return fmt.Errorf("compact signature options: %w", err)
	}

	sigType, _ := sigOptions["type"].(string)
	if sigType != "RsaSignature2017" {
		return fmt.Errorf("unsupported signature type %q", sigType)
	}
	creator, _ := sigOptions["creator"].(string)
	if creator == "" {
		return fmt.Errorf("signature has no creator")
	}
	signatureB64, _ := sigOptions["signatureValue"].(string)
	if signatureB64 == "" {
		return fmt.Errorf("signature has no signatureValue")
	}
	if expires, ok := sigOptions["expires"].(string); ok && expires != "" {
		if t, err := time.Parse(time.RFC3339, expires); err == nil && time.Now().After(t) {
			return fmt.Errorf("signature expired at %s", expires)
		}
	}

	delete(sigOptions, "type")
	delete(sigOptions, "id")
	delete(sigOptions, "signatureValue")
	optionsHash, err := canonicalHexHash(sigOptions)
	if err != nil {
		return fmt.Errorf("canonicalize signature options: %w", err)
	}

	docWithoutSignature := make(map[string]interface{}, len(doc))
	for k, v := range doc {
		if k == "signature" {
			continue
		}
		docWithoutSignature[k] = v
	}
	documentHash, err := canonicalHexHash(docWithoutSignature)
	if err != nil {
		return fmt.Errorf("canonicalize document: %w", err)
	}

	toVerify := sha256.Sum256([]byte(optionsHash + documentHash))

	sigBytes, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return fmt.Errorf("decode signatureValue: %w", err)
	}

	publicKeyPEM, err := v.fetchKey(creator)
	if err != nil {
		return fmt.Errorf("fetch ld-signature creator key: %w", err)
	}
	pub, err := v.parsePublicKey(creator, publicKeyPEM)
	if err != nil {
		return err
	}
	if pub.Algorithm != KeyRSA || pub.RSA == nil {
		return fmt.Errorf("ld-signature requires an RSA key (got %s)", pub.Algorithm)
	}

	if err := rsa.VerifyPKCS1v15(pub.RSA, crypto.SHA256, toVerify[:], sigBytes); err != nil {
		return fmt.Errorf("ld-signature verification failed: %w", err)
	}

	// A valid signature only proves the creator signed this exact document;
	// it says nothing about whether the creator is who the activity claims
	// to be. Require them to match, exactly as Mastodon's different_actor?
	// re-check does after swapping in the LD-verified account.
	declaredActor, _ := doc["actor"].(string)
	if ExtractKeyOwner(creator) != declaredActor {
		return fmt.Errorf("ld-signature creator %q does not match activity actor %q", ExtractKeyOwner(creator), declaredActor)
	}

	return nil
}
