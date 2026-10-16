package federation

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RFC 9421 (HTTP Message Signatures) is the standards-track successor to the
// Cavage draft. Mastodon 4.7 emits it when a draft-signed request is refused,
// so we have to verify it even though we still sign with the draft.
//
// Only verification lives here. The two formats are told apart by the
// Signature-Input header, which the draft does not use: when it is present the
// request is verified as RFC 9421 and never retried as a draft signature,
// because a signer that can be talked down to the older format is a signer
// that can be talked down at all.

// maxSignatureHeaderLen bounds both signature headers. The draft parser uses
// the same limit inline.
const maxSignatureHeaderLen = 8192

// signatureInput is one labelled entry of a Signature-Input header.
type signatureInput struct {
	label      string
	components []string
	// rawParams is the parameter section exactly as it arrived, minus the
	// leading semicolon. It goes into the signature base verbatim: rebuilding
	// it from the parsed values would reorder or renormalise parameters and
	// produce a base the signer never saw.
	rawParams  string
	keyID      string
	algorithm  string
	created    int64
	hasCreated bool
	expires    int64
	hasExpires bool
}

// parseSignatureInputs parses a Signature-Input header into its labelled
// entries. Components carrying their own parameters (@query-param, ;sf, ;bs,
// ;req and friends) are rejected rather than ignored: silently dropping a
// parameter changes what the base means.
func parseSignatureInputs(header string) ([]signatureInput, error) {
	if header == "" || len(header) > maxSignatureHeaderLen {
		return nil, fmt.Errorf("signature-input header missing or too long")
	}

	var inputs []signatureInput
	pos := 0
	for pos < len(header) {
		for pos < len(header) && (header[pos] == ' ' || header[pos] == '\t' || header[pos] == ',') {
			pos++
		}
		if pos >= len(header) {
			break
		}

		eq := strings.IndexByte(header[pos:], '=')
		if eq < 0 {
			return nil, fmt.Errorf("malformed signature-input entry")
		}
		label := strings.TrimSpace(header[pos : pos+eq])
		if label == "" || strings.ContainsFunc(label, func(r rune) bool { return r < 32 }) {
			return nil, fmt.Errorf("malformed signature-input label")
		}
		pos += eq + 1

		if pos >= len(header) || header[pos] != '(' {
			return nil, fmt.Errorf("signature-input %q is not an inner list", label)
		}
		pos++

		input := signatureInput{label: label}
		for pos < len(header) && header[pos] != ')' {
			if header[pos] == ' ' || header[pos] == '\t' {
				pos++
				continue
			}
			if header[pos] != '"' {
				return nil, fmt.Errorf("signature-input %q has an unquoted component", label)
			}
			pos++
			end := strings.IndexByte(header[pos:], '"')
			if end < 0 {
				return nil, fmt.Errorf("signature-input %q has an unterminated component", label)
			}
			component := strings.ToLower(header[pos : pos+end])
			pos += end + 1
			if pos < len(header) && header[pos] == ';' {
				return nil, fmt.Errorf("component %q carries unsupported parameters", component)
			}
			if component == "" {
				return nil, fmt.Errorf("signature-input %q has an empty component", label)
			}
			input.components = append(input.components, component)
		}
		if pos >= len(header) {
			return nil, fmt.Errorf("signature-input %q has an unterminated component list", label)
		}
		pos++ // closing parenthesis

		// Parameters run to the next top-level comma. Commas inside quoted
		// values belong to the value, not to the dictionary.
		start := pos
		inQuotes := false
		for pos < len(header) {
			c := header[pos]
			if c == '"' {
				inQuotes = !inQuotes
			} else if c == ',' && !inQuotes {
				break
			}
			pos++
		}
		if inQuotes {
			return nil, fmt.Errorf("signature-input %q has an unterminated parameter", label)
		}
		params := strings.TrimSpace(header[start:pos])
		params = strings.TrimPrefix(params, ";")
		input.rawParams = params

		if err := input.applyParams(params); err != nil {
			return nil, err
		}
		inputs = append(inputs, input)
	}

	if len(inputs) == 0 {
		return nil, fmt.Errorf("no usable signature-input entry")
	}
	return inputs, nil
}

func (in *signatureInput) applyParams(params string) error {
	for _, raw := range splitParams(params) {
		name, value, found := strings.Cut(raw, "=")
		if !found {
			continue
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		value = strings.TrimSuffix(strings.TrimPrefix(value, `"`), `"`)
		switch name {
		case "keyid":
			in.keyID = value
		case "alg":
			in.algorithm = strings.ToLower(value)
		case "created":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid created parameter")
			}
			in.created, in.hasCreated = n, true
		case "expires":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid expires parameter")
			}
			in.expires, in.hasExpires = n, true
		}
	}
	return nil
}

// splitParams splits a parameter section on semicolons that sit outside quotes.
func splitParams(params string) []string {
	var out []string
	start := 0
	inQuotes := false
	for i := 0; i < len(params); i++ {
		switch params[i] {
		case '"':
			inQuotes = !inQuotes
		case ';':
			if !inQuotes {
				out = append(out, params[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, params[start:])
	return out
}

// parseSignatureValues parses an RFC 9421 Signature header, a dictionary of
// byte sequences delimited by colons.
func parseSignatureValues(header string) (map[string][]byte, error) {
	if header == "" || len(header) > maxSignatureHeaderLen {
		return nil, fmt.Errorf("signature header missing or too long")
	}

	values := make(map[string][]byte)
	pos := 0
	for pos < len(header) {
		for pos < len(header) && (header[pos] == ' ' || header[pos] == '\t' || header[pos] == ',') {
			pos++
		}
		if pos >= len(header) {
			break
		}

		eq := strings.IndexByte(header[pos:], '=')
		if eq < 0 {
			return nil, fmt.Errorf("malformed signature entry")
		}
		label := strings.TrimSpace(header[pos : pos+eq])
		pos += eq + 1

		if pos >= len(header) || header[pos] != ':' {
			return nil, fmt.Errorf("signature %q is not a byte sequence", label)
		}
		pos++
		end := strings.IndexByte(header[pos:], ':')
		if end < 0 {
			return nil, fmt.Errorf("signature %q is unterminated", label)
		}
		raw, err := base64.StdEncoding.DecodeString(header[pos : pos+end])
		if err != nil {
			return nil, fmt.Errorf("decode signature %q: %w", label, err)
		}
		pos += end + 1
		values[label] = raw
	}

	if len(values) == 0 {
		return nil, fmt.Errorf("no usable signature entry")
	}
	return values, nil
}

// buildSignatureBase assembles the string that was signed, per RFC 9421
// section 2.5: one line per covered component, then the @signature-params
// line holding the component list and the parameters as received.
func buildSignatureBase(req *http.Request, in signatureInput) (string, error) {
	var b strings.Builder
	for _, component := range in.components {
		value, err := componentValue(req, component)
		if err != nil {
			return "", err
		}
		b.WriteString(`"` + component + `": ` + value + "\n")
	}

	b.WriteString(`"@signature-params": (`)
	for i, component := range in.components {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(`"` + component + `"`)
	}
	b.WriteByte(')')
	if in.rawParams != "" {
		b.WriteString(";" + in.rawParams)
	}
	return b.String(), nil
}

// componentValue returns the canonical value of one covered component.
func componentValue(req *http.Request, component string) (string, error) {
	if !strings.HasPrefix(component, "@") {
		// Go's server hoists Host out of the header map, and a field may
		// legitimately appear more than once.
		values := req.Header.Values(component)
		if len(values) == 0 && strings.EqualFold(component, "host") && req.Host != "" {
			values = []string{req.Host}
		}
		if len(values) == 0 {
			return "", fmt.Errorf("covered component %q is missing", component)
		}
		canonical := make([]string, len(values))
		for i, v := range values {
			canonical[i] = strings.TrimSpace(v)
		}
		return strings.Join(canonical, ", "), nil
	}

	switch component {
	case "@method":
		return strings.ToUpper(req.Method), nil
	case "@authority":
		return normalizedAuthority(req)
	case "@path":
		path := req.URL.EscapedPath()
		if path == "" {
			path = "/"
		}
		return path, nil
	case "@query":
		// A request without a query string still signs "?", per section 2.2.7.
		return "?" + req.URL.RawQuery, nil
	case "@request-target":
		return req.URL.RequestURI(), nil
	case "@target-uri":
		authority, err := normalizedAuthority(req)
		if err != nil {
			return "", err
		}
		return "https://" + authority + req.URL.RequestURI(), nil
	default:
		return "", fmt.Errorf("unsupported derived component %q", component)
	}
}

// requestAuthority returns the host the client addressed. Inbound requests
// carry it in req.Host; the scheme is not recoverable from the request at all
// once a reverse proxy has terminated TLS, so @target-uri is always built as
// https, which is the only scheme federation runs over.
func requestAuthority(req *http.Request) string {
	if req.Host != "" {
		return req.Host
	}
	if host := req.Header.Get("Host"); host != "" {
		return host
	}
	return req.URL.Host
}

func normalizedAuthority(req *http.Request) (string, error) {
	raw := strings.TrimSpace(requestAuthority(req))
	parsed, err := url.Parse("https://" + raw)
	if err != nil || parsed.User != nil || parsed.Hostname() == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid request authority %q", raw)
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if port == "" || port == "443" {
		if strings.Contains(host, ":") {
			return "[" + host + "]", nil
		}
		return host, nil
	}
	return net.JoinHostPort(host, port), nil
}

// verifyRFC9421 verifies an RFC 9421 signature on an incoming request whose
// body the caller has already read. It returns the key ID that verified.
func (v *Verifier) verifyRFC9421(req *http.Request, body []byte) (string, error) {
	inputs, err := parseSignatureInputs(strings.Join(req.Header.Values("Signature-Input"), ", "))
	if err != nil {
		return "", err
	}
	signatures, err := parseSignatureValues(strings.Join(req.Header.Values("Signature"), ", "))
	if err != nil {
		return "", err
	}

	now := time.Now()
	var lastErr error
	for _, in := range inputs {
		sig, ok := signatures[in.label]
		if !ok {
			lastErr = fmt.Errorf("signature-input %q has no matching signature", in.label)
			continue
		}
		keyID, err := v.verifyRFC9421Input(req, body, in, sig, now)
		if err == nil {
			return keyID, nil
		}
		lastErr = err
	}
	return "", lastErr
}

func (v *Verifier) verifyRFC9421Input(req *http.Request, body []byte, in signatureInput, sig []byte, now time.Time) (string, error) {
	if in.keyID == "" {
		return "", fmt.Errorf("signature %q has no keyid", in.label)
	}

	covered := make(map[string]bool, len(in.components))
	for _, component := range in.components {
		covered[component] = true
	}
	if !covered["@method"] {
		return "", fmt.Errorf("signature %q does not cover @method", in.label)
	}
	targetCovered := covered["@target-uri"] || covered["@authority"] && covered["@request-target"]
	if covered["@authority"] && covered["@path"] && (req.URL.RawQuery == "" || covered["@query"]) {
		targetCovered = true
	}
	if !targetCovered {
		return "", fmt.Errorf("signature %q does not cover the request target", in.label)
	}
	// A body that is not bound to the signature can be swapped for another.
	if req.Method == http.MethodPost && !covered["content-digest"] && !covered["digest"] {
		return "", fmt.Errorf("signature %q does not cover the body digest", in.label)
	}
	// Without created there is nothing to age the signature against, so the
	// Date header has to be covered instead.
	if !in.hasCreated && !covered["date"] {
		return "", fmt.Errorf("signature %q has neither created nor a covered date", in.label)
	}

	if in.hasCreated {
		skew := now.Sub(time.Unix(in.created, 0))
		if skew < -replayWindow || skew > replayWindow {
			return "", fmt.Errorf("signature %q created skew too large: %v", in.label, skew)
		}
	}
	if in.hasExpires && now.After(time.Unix(in.expires, 0)) {
		return "", fmt.Errorf("signature %q has expired", in.label)
	}

	base, err := buildSignatureBase(req, in)
	if err != nil {
		return "", err
	}

	publicKeyPEM, err := v.fetchKey(in.keyID)
	if err != nil {
		return "", fmt.Errorf("fetch key: %w", err)
	}
	pub, err := v.parsePublicKey(in.keyID, publicKeyPEM)
	if err != nil {
		return "", err
	}
	if err := verifyRFC9421Signature(pub, []byte(base), sig, in.algorithm); err != nil {
		return "", fmt.Errorf("signature verification failed: %w", err)
	}

	if covered["date"] {
		date, err := time.Parse(http.TimeFormat, req.Header.Get("Date"))
		if err != nil {
			return "", fmt.Errorf("invalid Date header: %w", err)
		}
		skew := now.Sub(date)
		if skew < -replayWindow || skew > replayWindow {
			return "", fmt.Errorf("date header skew too large: %v", skew)
		}
	}

	if covered["content-digest"] {
		if err := verifyContentDigest(strings.Join(req.Header.Values("Content-Digest"), ", "), body); err != nil {
			return "", err
		}
	}
	if covered["digest"] {
		if err := verifyLegacyDigest(req.Header.Get("Digest"), body); err != nil {
			return "", err
		}
	}

	if req.Method == http.MethodPost {
		key := in.keyID + "\x00" + base64.StdEncoding.EncodeToString(sig)
		if !v.recordReplay(key, now) {
			return "", fmt.Errorf("replayed signature")
		}
	}

	return in.keyID, nil
}

// verifyRFC9421Signature dispatches on the key, using alg only to rule out
// combinations that can never be valid. Mastodon signs Ed25519 with "ed25519"
// and RSA with "rsa-v1_5-sha256"; alg is optional, in which case the key type
// decides.
func verifyRFC9421Signature(pub *PublicKey, base, sig []byte, algorithm string) error {
	switch pub.Algorithm {
	case KeyEd25519:
		if algorithm != "" && algorithm != "ed25519" {
			return fmt.Errorf("algorithm %q does not match an Ed25519 key", algorithm)
		}
		return verifySignature(pub, base, sig, "ed25519")
	case KeyRSA:
		switch algorithm {
		case "", "rsa-v1_5-sha256":
			hash := sha256.Sum256(base)
			return rsa.VerifyPKCS1v15(pub.RSA, crypto.SHA256, hash[:], sig)
		case "rsa-v1_5-sha512":
			hash := sha512.Sum512(base)
			return rsa.VerifyPKCS1v15(pub.RSA, crypto.SHA512, hash[:], sig)
		case "rsa-pss-sha512":
			hash := sha512.Sum512(base)
			return rsa.VerifyPSS(pub.RSA, crypto.SHA512, hash[:], sig, &rsa.PSSOptions{
				SaltLength: 64,
				Hash:       crypto.SHA512,
			})
		default:
			return fmt.Errorf("algorithm %q does not match an RSA key", algorithm)
		}
	default:
		return fmt.Errorf("unsupported key algorithm %v", pub.Algorithm)
	}
}

// verifyContentDigest checks a Content-Digest header (RFC 9530) against the
// body. The field may list several algorithms; every one we recognise has to
// match, and at least one has to be recognised.
func verifyContentDigest(header string, body []byte) error {
	if header == "" {
		return fmt.Errorf("missing Content-Digest header")
	}
	if len(header) > maxSignatureHeaderLen {
		return fmt.Errorf("Content-Digest header too long")
	}

	checked := 0
	for _, entry := range splitDigestEntries(header) {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		value = strings.TrimSuffix(strings.TrimPrefix(value, ":"), ":")

		var want string
		switch name {
		case "sha-256":
			sum := sha256.Sum256(body)
			want = base64.StdEncoding.EncodeToString(sum[:])
		case "sha-512":
			sum := sha512.Sum512(body)
			want = base64.StdEncoding.EncodeToString(sum[:])
		default:
			continue
		}
		if value != want {
			return fmt.Errorf("content-digest mismatch")
		}
		checked++
	}

	if checked == 0 {
		return fmt.Errorf("no supported content-digest algorithm")
	}
	return nil
}

// verifyLegacyDigest checks the Cavage-era Digest header, which some servers
// keep sending alongside an RFC 9421 signature.
func verifyLegacyDigest(header string, body []byte) error {
	if !strings.HasPrefix(header, "SHA-256=") {
		return fmt.Errorf("unsupported digest algorithm: %s", header)
	}
	sum := sha256.Sum256(body)
	if strings.TrimPrefix(header, "SHA-256=") != base64.StdEncoding.EncodeToString(sum[:]) {
		return fmt.Errorf("digest mismatch")
	}
	return nil
}

// splitDigestEntries splits a Content-Digest field on commas outside the
// colon-delimited byte sequences, whose base64 payload can itself hold commas
// only when the field is malformed but must not be split on regardless.
func splitDigestEntries(header string) []string {
	var out []string
	start := 0
	inBytes := false
	for i := 0; i < len(header); i++ {
		switch header[i] {
		case ':':
			inBytes = !inBytes
		case ',':
			if !inBytes {
				out = append(out, header[start:i])
				start = i + 1
			}
		}
	}
	return append(out, header[start:])
}
