package federation

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	_ "crypto/sha512" // register SHA-512 for crypto.SHA512.New() in signature verification
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/version"
)

// Signer signs outgoing HTTP requests with HTTP Signatures (Cavage draft).
// It is key-agnostic: RSA (PKCS#1 v1.5) or Ed25519, selected by algorithm.
type Signer struct {
	keyID       string
	privateKey  crypto.Signer
	algorithm   KeyAlgorithm
	userAgent   string
	instanceKey bool
}

// NewSigner creates a Signer from a PEM-encoded private key and key ID. The
// key may be RSA (PKCS1 or PKCS8) or Ed25519 (PKCS8).
func NewSigner(keyID, privateKeyPEM string) (*Signer, error) {
	priv, algorithm, err := ParsePrivateKeyPEM(privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("signer: %w", err)
	}

	return &Signer{
		keyID:      keyID,
		privateKey: priv,
		algorithm:  algorithm,
		userAgent:  "littleFedi/" + version.Version,
	}, nil
}

// SetUserAgent configures the User-Agent applied to signed HTTP requests.
func (s *Signer) SetUserAgent(userAgent string) {
	if userAgent != "" {
		s.userAgent = userAgent
	}
}

// SignRequest signs an outgoing HTTP request.
// For POST requests, signs (request-target), host, date, digest.
// For GET requests, signs (request-target), host, date.
// If the request carries an Accept header (as all ActivityPub fetches do),
// it is also signed so that servers requiring it (e.g. GoToSocial with
// authorized fetch) accept the signature.
func (s *Signer) SignRequest(req *http.Request, body []byte) error {
	// Ensure Date header is set
	if req.Header.Get("Date") == "" {
		req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	}

	// Set User-Agent
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", s.userAgent)
	}

	// Explicitly promote Host to the header map for signature construction.
	// Outbound client requests usually leave req.Host empty and use URL.Host.
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	req.Header.Set("Host", host)

	// For POST requests, compute Digest header.
	// For GET requests that carry an Accept header (ActivityPub fetches),
	// include it so servers like GoToSocial that require it for authorized
	// fetch will accept the signature.
	var signedHeaders []string
	if len(body) > 0 {
		digest := sha256.Sum256(body)
		digestB64 := base64.StdEncoding.EncodeToString(digest[:])
		req.Header.Set("Digest", "SHA-256="+digestB64)
		signedHeaders = []string{"(request-target)", "host", "date", "digest"}
	} else {
		signedHeaders = []string{"(request-target)", "host", "date"}
		if req.Header.Get("Accept") != "" {
			signedHeaders = append(signedHeaders, "accept")
		}
	}

	// Build signing string
	signingString := buildSigningString(req, signedHeaders)

	// Sign. RSA signs the SHA-256 digest of the signing string with PKCS#1
	// v1.5; Ed25519 signs the signing string bytes directly (Ed25519 hashes
	// internally) and uses the hs2019 label, where the real algorithm is
	// derived from the key by the verifier.
	var sig []byte
	var err error
	algorithmLabel := "rsa-sha256"
	switch s.algorithm {
	case KeyEd25519:
		sig = ed25519.Sign(s.privateKey.(ed25519.PrivateKey), []byte(signingString))
		algorithmLabel = "hs2019"
	default:
		hash := sha256.Sum256([]byte(signingString))
		sig, err = rsa.SignPKCS1v15(rand.Reader, s.privateKey.(*rsa.PrivateKey), crypto.SHA256, hash[:])
		if err != nil {
			return fmt.Errorf("sign: %w", err)
		}
	}

	sigB64 := base64.StdEncoding.EncodeToString(sig)

	// Build Signature header
	signatureHeader := fmt.Sprintf(
		`keyId="%s",algorithm="%s",headers="%s",signature="%s"`,
		s.keyID,
		algorithmLabel,
		strings.Join(signedHeaders, " "),
		sigB64,
	)

	req.Header.Set("Signature", signatureHeader)

	if debugSig {
		slog.Warn("DEBUG sign request",
			"keyID", s.keyID,
			"method", req.Method,
			"url", req.URL.String(),
			"signed_headers", strings.Join(signedHeaders, " "),
			"signing_string", strconv.Quote(signingString),
			"signature_header", signatureHeader,
		)
	}
	return nil
}

// debugSig enables verbose HTTP-signature debug logging when LITTLEFEDI_DEBUG_SIG
// is set in the environment. Used to diagnose federation signature rejections.
var debugSig = os.Getenv("LITTLEFEDI_DEBUG_SIG") != ""

// verifyRSASignature verifies an RSA PKCS#1 v1.5 signature over data, choosing
// the hash from the (possibly hidden) algorithm label. "rsa-sha256" forces
// SHA-256, "rsa-sha512" forces SHA-512, and "" / "hs2019" (where the real
// algorithm is derived from the key) try SHA-256 first then SHA-512.
func verifyRSASignature(pub *rsa.PublicKey, data, sig []byte, algorithm string) error {
	tryHashes := []crypto.Hash{}
	switch algorithm {
	case "rsa-sha512":
		tryHashes = append(tryHashes, crypto.SHA512)
	case "rsa-sha256":
		tryHashes = append(tryHashes, crypto.SHA256)
	case "", "hs2019":
		tryHashes = append(tryHashes, crypto.SHA256, crypto.SHA512)
	default:
		return fmt.Errorf("algorithm %q does not match an RSA key", algorithm)
	}

	var lastErr error
	for _, h := range tryHashes {
		hasher := h.New()
		hasher.Write(data)
		if err := rsa.VerifyPKCS1v15(pub, h, hasher.Sum(nil), sig); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

// verifySignature verifies data against sig with the given public key,
// dispatching on the key's algorithm. The algorithm label controls only the
// hash selection for RSA; with "hs2019" the digest algorithm is hidden and
// derived from the key (RSA tries SHA-256 then SHA-512, Ed25519 is used
// directly).
func verifySignature(pub *PublicKey, data, sig []byte, algorithm string) error {
	switch pub.Algorithm {
	case KeyEd25519:
		if algorithm != "" && algorithm != "hs2019" && algorithm != "ed25519" {
			return fmt.Errorf("algorithm %q does not match an Ed25519 key", algorithm)
		}
		if !ed25519.Verify(pub.Ed25519, data, sig) {
			return fmt.Errorf("ed25519 signature verification failed")
		}
		return nil
	case KeyRSA:
		return verifyRSASignature(pub.RSA, data, sig, algorithm)
	default:
		return fmt.Errorf("unsupported key algorithm %v", pub.Algorithm)
	}
}

// buildSigningString builds the signing string per the Cavage HTTP Signatures draft.
func buildSigningString(req *http.Request, headers []string) string {
	var parts []string
	for _, h := range headers {
		if h == "(request-target)" {
			parts = append(parts, "(request-target): "+strings.ToLower(req.Method)+" "+req.URL.Path)
			if req.URL.RawQuery != "" {
				parts[len(parts)-1] += "?" + req.URL.RawQuery
			}
		} else {
			val := req.Header.Get(h)
			// Go's HTTP server strips the Host header from req.Header on
			// incoming requests and promotes it to req.Host. Use req.Host
			// as a fallback so signature verification works.
			if val == "" && strings.EqualFold(h, "host") {
				val = req.Host
			}
			parts = append(parts, h+": "+val)
		}
	}
	return strings.Join(parts, "\n")
}

// Verification

// Verifier verifies incoming HTTP Signatures.
type Verifier struct {
	fetchKey   func(keyID string) (string, error) // fetches PEM public key by keyID
	replays    replayCache
	parsedKeys parsedKeyCache
}

const (
	replayWindow  = 5 * time.Minute
	maxReplayKeys = 4096
)

type replayCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
	order   []replayEntry
	head    int
}

type replayEntry struct {
	key  string
	seen time.Time
}

// NewVerifier creates a Verifier with a key-fetching function.
func NewVerifier(fetchKey func(keyID string) (string, error)) *Verifier {
	return &Verifier{
		fetchKey: fetchKey,
		replays:  replayCache{entries: make(map[string]time.Time)},
	}
}

const maxParsedKeys = 512

// parsedKeyCache memoises PEM decode + PKIX parse per keyID: both run on
// every inbound signed request otherwise. fetchKey is still consulted every
// request (it is a RAM entity-cache hit when caching is on), and the parsed
// key is reused only when the fetched PEM is byte-identical to the memoised
// one - so key rotation via the actor refresher takes effect the moment the
// new PEM is served, with no TTL and no invalidation hook.
type parsedKeyCache struct {
	mu      sync.RWMutex
	entries map[string]*parsedKeyEntry
}

type parsedKeyEntry struct {
	pem string
	key *PublicKey
}

// parsePublicKey returns the public key for publicKeyPEM, reusing the
// memoised parse for keyID when the PEM has not changed.
func (v *Verifier) parsePublicKey(keyID, publicKeyPEM string) (*PublicKey, error) {
	c := &v.parsedKeys
	c.mu.RLock()
	entry := c.entries[keyID]
	c.mu.RUnlock()
	if entry != nil && entry.pem == publicKeyPEM {
		return entry.key, nil
	}

	pub, err := ParsePublicKeyPEM(publicKeyPEM)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]*parsedKeyEntry)
	}
	if len(c.entries) >= maxParsedKeys {
		// Simple random eviction of a handful of entries keeps the map
		// bounded without LRU bookkeeping on the verify hot path.
		evicted := 0
		for k := range c.entries {
			delete(c.entries, k)
			if evicted++; evicted >= maxParsedKeys/16 {
				break
			}
		}
	}
	c.entries[keyID] = &parsedKeyEntry{pem: publicKeyPEM, key: pub}
	c.mu.Unlock()
	return pub, nil
}

// recordReplay atomically records a verified signature. It returns false when
// the same key/signature pair was accepted within the replay window. The cache
// is process-local and bounded: durable activity idempotency
// remains the correctness layer across restarts and replicas.
func (v *Verifier) recordReplay(key string, now time.Time) bool {
	c := &v.replays
	c.mu.Lock()
	defer c.mu.Unlock()

	cutoff := now.Add(-replayWindow)
	for c.head < len(c.order) {
		old := c.order[c.head]
		if old.seen.After(cutoff) {
			break
		}
		if seen, ok := c.entries[old.key]; ok && seen.Equal(old.seen) {
			delete(c.entries, old.key)
		}
		c.head++
	}
	if seen, ok := c.entries[key]; ok && seen.After(cutoff) {
		return false
	}
	for len(c.entries) >= maxReplayKeys && c.head < len(c.order) {
		old := c.order[c.head]
		c.head++
		if seen, ok := c.entries[old.key]; ok && seen.Equal(old.seen) {
			delete(c.entries, old.key)
		}
	}
	c.entries[key] = now
	c.order = append(c.order, replayEntry{key: key, seen: now})
	if c.head >= 1024 && c.head*2 >= len(c.order) {
		copy(c.order, c.order[c.head:])
		c.order = c.order[:len(c.order)-c.head]
		c.head = 0
	}
	return true
}

// VerifyRequest verifies the HTTP Signature on an incoming request.
// Returns the key owner's actor URI on success. Callers that have already
// read the request body should use VerifyRequestWithBody to avoid a second
// read and copy.
func (v *Verifier) VerifyRequest(req *http.Request) (keyOwner string, err error) {
	var body []byte
	if req.Header.Get("Digest") != "" || req.Header.Get("Content-Digest") != "" {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return "", fmt.Errorf("read body for digest check: %w", err)
		}
		body = b
		req.Body = io.NopCloser(bytes.NewReader(b))
	}
	return v.VerifyRequestWithBody(req, body)
}

// VerifyRequestWithBody verifies the HTTP Signature on an incoming request
// whose body has already been read by the caller. req.Body is never touched.
func (v *Verifier) VerifyRequestWithBody(req *http.Request, body []byte) (keyOwner string, err error) {
	sigHeader := req.Header.Get("Signature")
	if sigHeader == "" {
		return "", fmt.Errorf("missing Signature header")
	}

	// Signature-Input marks an RFC 9421 signature, which the Cavage draft has
	// no equivalent of. Once it is there the request is verified as RFC 9421
	// and a failure stays a failure: retrying it as a draft signature would
	// let a peer pick the weaker of the two formats.
	if req.Header.Get("Signature-Input") != "" {
		return v.verifyRFC9421(req, body)
	}

	// Parse the Signature header
	params := parseSignatureHeader(sigHeader)
	if params == nil {
		return "", fmt.Errorf("invalid Signature header format")
	}

	keyID := params["keyId"]
	headers := params["headers"]
	sigB64 := params["signature"]
	algorithm := params["algorithm"]

	if keyID == "" || sigB64 == "" {
		return "", fmt.Errorf("incomplete Signature parameters")
	}

	// Validate the algorithm parameter. Per the HTTP Signatures "cavage" draft
	// (and as implemented by GoToSocial, Mastodon, etc.) the modern label is
	// "hs2019", where the real algorithm is derived from the key rather than
	// stated explicitly; verifiers are expected to IGNORE the algorithm field
	// in that case. The final dispatch happens in verifySignature, which
	// selects by key type; this gate only rejects labels that can never be
	// valid regardless of the key.
	algorithm = strings.ToLower(algorithm)
	switch algorithm {
	case "", "hs2019", "ed25519", "rsa-sha256", "rsa-sha512":
		// supported / derive-from-key
	default:
		return "", fmt.Errorf("unsupported signature algorithm: %s", algorithm)
	}

	// Default headers if not specified
	if headers == "" {
		headers = "(request-target) host date"
	}

	// Validate minimum required headers to prevent replay attacks
	signedHeaders := strings.Fields(headers)
	hasRequestTarget := false
	hasHost := false
	hasDate := false
	hasDigest := false
	for _, h := range signedHeaders {
		switch strings.ToLower(h) {
		case "(request-target)":
			hasRequestTarget = true
		case "host":
			hasHost = true
		case "date":
			hasDate = true
		case "digest":
			hasDigest = true
		}
	}
	if !hasRequestTarget || !hasHost || !hasDate {
		return "", fmt.Errorf("insufficient signed headers (need at least (request-target) host date)")
	}
	if req.Method == http.MethodPost && !hasDigest {
		return "", fmt.Errorf("digest must be signed on POST")
	}

	// Resolve the key ID to an actor URI and fetch the public key
	publicKeyPEM, err := v.fetchKey(keyID)
	if err != nil {
		return "", fmt.Errorf("fetch key: %w", err)
	}

	signingString := buildSigningString(req, signedHeaders)

	// Debug: log the full verification context to diagnose signing-string mismatches.
	// Guarded so the 13 attrs aren't boxed per inbound request when debug is off.
	if slog.Default().Enabled(req.Context(), slog.LevelDebug) {
		slog.Debug("verifying HTTP signature",
			"keyId", keyID,
			"algorithm", algorithm,
			"signed_headers", signedHeaders,
			"method", req.Method,
			"url_path", req.URL.Path,
			"url_rawquery", req.URL.RawQuery,
			"host_header", req.Header.Get("host"),
			"date_header", req.Header.Get("date"),
			"digest_header", req.Header.Get("digest"),
			"content_type_header", req.Header.Get("content-type"),
			"signing_string", signingString,
		)
	}

	// Decode signature
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return "", fmt.Errorf("decode signature: %w", err)
	}

	// Parse public key (memoised per keyID while the PEM is unchanged)
	pub, err := v.parsePublicKey(keyID, publicKeyPEM)
	if err != nil {
		return "", err
	}

	// Verify. With "hs2019" the digest algorithm is hidden, so the dispatch
	// picks it from the key: RSA tries SHA-256 (what RSA signers like
	// GoToSocial and Mastodon use by default) then SHA-512; Ed25519 is used
	// directly. For explicit rsa-sha512, only SHA-512 is attempted.
	if err := verifySignature(pub, []byte(signingString), sig, algorithm); err != nil {
		return "", fmt.Errorf("signature verification failed: %w", err)
	}

	// Verify Date header is within acceptable skew (5 minutes)
	date, err := time.Parse(http.TimeFormat, req.Header.Get("Date"))
	if err != nil {
		return "", fmt.Errorf("invalid Date header: %w", err)
	}
	now := time.Now()
	skew := now.Sub(date)
	if skew < -replayWindow || skew > replayWindow {
		return "", fmt.Errorf("date header skew too large: %v", skew)
	}

	// Verify Digest - required for POST requests with body to prevent
	// body manipulation and replay-style attacks.
	if req.Method == http.MethodPost {
		if digestHeader := req.Header.Get("Digest"); digestHeader == "" {
			return "", fmt.Errorf("missing Digest header on POST")
		}
	}

	// Verify Digest if present
	if digestHeader := req.Header.Get("Digest"); digestHeader != "" {
		if !strings.HasPrefix(digestHeader, "SHA-256=") {
			return "", fmt.Errorf("unsupported digest algorithm: %s", digestHeader)
		}
		expectedDigest := strings.TrimPrefix(digestHeader, "SHA-256=")
		actualHash := sha256.Sum256(body)
		actualDigest := base64.StdEncoding.EncodeToString(actualHash[:])
		if actualDigest != expectedDigest {
			return "", fmt.Errorf("digest mismatch")
		}
	}

	if req.Method == http.MethodPost && !v.recordReplay(keyID+"\x00"+sigB64, now) {
		return "", fmt.Errorf("replayed signature")
	}

	return keyID, nil
}

// parseSignatureHeader parses a basic HTTP Signature header.
// Handles the Cavage draft format: keyId="...",algorithm="...",headers="...",signature="..."
//
// This is a minimal parser; it does not handle edge cases like
// quoted-pair escapes inside values. A malicious server sending a deliberately
// malformed Signature header can at worst cause us to reject the request, never
// to accept a forged one.
func parseSignatureHeader(header string) map[string]string {
	if len(header) == 0 || len(header) > 8192 {
		return nil
	}

	params := make(map[string]string)
	pos := 0
	for pos < len(header) {
		// Skip whitespace and commas
		for pos < len(header) && (header[pos] == ' ' || header[pos] == '\t' || header[pos] == ',') {
			pos++
		}
		if pos >= len(header) {
			break
		}

		// Find key
		eqPos := strings.IndexByte(header[pos:], '=')
		if eqPos < 0 {
			break
		}
		key := strings.TrimSpace(header[pos : pos+eqPos])

		// Reject control characters in key names
		if strings.ContainsFunc(key, func(r rune) bool { return r < 32 }) {
			return nil
		}

		pos += eqPos + 1

		if pos >= len(header) {
			break
		}

		// Parse value (quoted or unquoted)
		var value string
		if header[pos] == '"' {
			pos++ // skip opening quote
			// Scan for closing quote (does not handle backslash-escaped quotes)
			rem := header[pos:]
			endPos := strings.IndexByte(rem, '"')
			if endPos < 0 {
				break
			}
			// Limit value length to prevent head-of-line blocking
			if endPos > 4096 {
				return nil
			}
			value = rem[:endPos]
			pos += endPos + 1
		} else {
			endPos := strings.IndexAny(header[pos:], " ,\t")
			if endPos < 0 {
				value = strings.TrimSpace(header[pos:])
				pos = len(header)
			} else {
				value = strings.TrimSpace(header[pos : pos+endPos])
				pos += endPos
			}
		}

		params[key] = value
	}

	return params
}

// ExtractKeyOwner extracts the actor URI that owns a public key, given the
// key's ID. Two conventions are in use across the fediverse:
//
//   - Fragment style (Mastodon, Pleroma, littlefedi):
//     https://example.com/users/user#main-key  -> https://example.com/users/user
//   - Path style (GoToSocial):
//     https://example.com/users/user/main-key  -> https://example.com/users/user
//
// Handling both is required for the inbox anti-impersonation check (matching the
// signing key's owner against the activity actor); otherwise signed POSTs from
// GoToSocial are rejected with a spurious "actor mismatch" 403.
func ExtractKeyOwner(keyID string) string {
	if idx := strings.LastIndex(keyID, "#"); idx >= 0 {
		return keyID[:idx]
	}
	if owner := strings.TrimSuffix(keyID, "/main-key"); owner != keyID {
		return owner
	}
	return keyID
}
