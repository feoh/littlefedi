// Package push implements Web Push notification sending (RFC 8030, RFC 8291).
// It handles VAPID authentication, payload encryption (aes128gcm), and delivery
// to browser push services.
package push

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/netutil"
)

// VAPIDKeys holds the VAPID key pair used for identifying the application server.
type VAPIDKeys struct {
	Public  string // base64-encoded raw public key (65 bytes, no padding)
	Private string // base64-encoded raw private key (32 bytes, no padding)
}

// Subscription represents a client's push subscription.
type Subscription struct {
	Endpoint string
	P256DH   string // base64-encoded client public key
	Auth     string // base64-encoded auth secret
	// Standard selects RFC 8291 aes128gcm. Otherwise the payload uses the
	// legacy aesgcm draft encoding, which Mastodon sends to every client
	// that does not ask for the standard one.
	Standard bool
}

// NotificationPayload is the JSON payload sent in push notifications.
//
// The fields Mastodon's own payload carries are what Mastodon clients decode:
// access_token tells them which account a push is for, and notification_id
// is the API ID as a number, which they fetch next.
type NotificationPayload struct {
	Title                    string `json:"title"`
	Body                     string `json:"body"`
	Icon                     string `json:"icon,omitempty"`
	Badge                    string `json:"badge,omitempty"`
	Vibrate                  []int  `json:"vibrate,omitempty"`
	Tag                      string `json:"tag,omitempty"`
	URL                      string `json:"url,omitempty"`
	Type                     string `json:"type,omitempty"`
	AccessToken              string `json:"access_token,omitempty"`
	NotificationID           int64  `json:"notification_id"`
	NotificationType         string `json:"notification_type,omitempty"`
	PreferredLocale          string `json:"preferred_locale,omitempty"`
	AccountID                string `json:"account_id,omitempty"`
	AccountAcct              string `json:"account_acct,omitempty"`
	AccountName              string `json:"account_name,omitempty"`
	AccountAvatar            string `json:"account_avatar,omitempty"`
	AccountURL               string `json:"account_url,omitempty"`
	StatusID                 string `json:"status_id,omitempty"`
	StatusURL                string `json:"status_url,omitempty"`
	RecipientAccountID       string `json:"recipient_account_id,omitempty"`
	RecipientAccountUsername string `json:"recipient_account_username,omitempty"`
}

// Sender handles sending Web Push notifications.
type Sender struct {
	vapid   *VAPIDKeys
	subject string
	client  *http.Client
}

// NewSender creates a new push notification sender.
// vapid contains the VAPID keys; if nil, VAPID will not be used.
// subject is the "sub" claim for VAPID (typically mailto: or https://).
func NewSender(vapid *VAPIDKeys, subject string) *Sender {
	// Guarding in Control keeps the dialer's own walk over every resolved
	// address, so a push service whose first record is unreachable still gets
	// the notification.
	dialer := &net.Dialer{
		Timeout:       10 * time.Second,
		KeepAlive:     30 * time.Second,
		FallbackDelay: 300 * time.Millisecond,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("invalid push address: %w", err)
			}
			ip := net.ParseIP(host)
			if ip == nil || !isPublicIP(ip) {
				return fmt.Errorf("push endpoint resolved to a non-public address")
			}
			return nil
		},
	}
	return &Sender{
		vapid:   vapid,
		subject: subject,
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: netutil.WithIPv4Fallback(&http.Transport{}, dialer),
		},
	}
}

// NormalizeEndpoint accepts only absolute HTTPS push-service URLs. The sender
// repeats the check and resolves through an SSRF-guarded dialer before sending.
func NormalizeEndpoint(endpoint string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("push endpoint must be an absolute HTTPS URL")
	}
	if u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("push endpoint must not contain credentials or a fragment")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !isPublicIP(ip) {
		return "", fmt.Errorf("push endpoint must use a public address")
	}
	return u.String(), nil
}

func isPublicIP(ip net.IP) bool {
	return netutil.IsPublicIP(ip)
}

// GenerateVAPIDKeys generates a new VAPID key pair (ECDSA P-256).
// Returns the keys as base64-encoded raw strings (no padding).
func GenerateVAPIDKeys() (*VAPIDKeys, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate vapid key: %w", err)
	}

	// Public key: uncompressed point (65 bytes: 04 || x || y)
	pub := elliptic.Marshal(elliptic.P256(), key.PublicKey.X, key.PublicKey.Y)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	// Private key: 32 bytes
	privRaw := key.D.Bytes()
	if len(privRaw) < 32 {
		padded := make([]byte, 32)
		copy(padded[32-len(privRaw):], privRaw)
		privRaw = padded
	}
	privB64 := base64.RawURLEncoding.EncodeToString(privRaw)

	return &VAPIDKeys{
		Public:  pubB64,
		Private: privB64,
	}, nil
}

// ParseVAPIDKeys parses base64-encoded VAPID keys into usable form.
func ParseVAPIDKeys(publicB64, privateB64 string) (*VAPIDKeys, error) {
	if publicB64 == "" || privateB64 == "" {
		return nil, fmt.Errorf("both vapid public and private keys are required")
	}
	pubRaw, normalizedPub, err := decodeBase64URL(publicB64)
	if err != nil {
		return nil, fmt.Errorf("invalid vapid public key: %w", err)
	}
	if len(pubRaw) != 65 || pubRaw[0] != 0x04 {
		return nil, fmt.Errorf("invalid vapid public key: expected 65-byte uncompressed P-256 point")
	}
	privRaw, normalizedPriv, err := decodeBase64URL(privateB64)
	if err != nil {
		return nil, fmt.Errorf("invalid vapid private key: %w", err)
	}
	if len(privRaw) != 32 {
		return nil, fmt.Errorf("invalid vapid private key: expected 32 bytes")
	}
	return &VAPIDKeys{
		Public:  normalizedPub,
		Private: normalizedPriv,
	}, nil
}

// NormalizeClientKeys validates and normalizes Web Push client keys to
// unpadded base64url strings suitable for storage and encryption.
func NormalizeClientKeys(p256dh, auth string) (string, string, error) {
	clientPub, normalizedP256DH, err := decodeBase64URL(p256dh)
	if err != nil {
		return "", "", fmt.Errorf("decode client public key: %w", err)
	}
	if len(clientPub) != 65 || clientPub[0] != 0x04 {
		return "", "", fmt.Errorf("invalid client public key: expected 65-byte uncompressed P-256 point")
	}
	if x, y := elliptic.Unmarshal(elliptic.P256(), clientPub); x == nil || y == nil {
		return "", "", fmt.Errorf("invalid client public key")
	}

	authSecret, normalizedAuth, err := decodeBase64URL(auth)
	if err != nil {
		return "", "", fmt.Errorf("decode auth secret: %w", err)
	}
	if len(authSecret) < 16 {
		return "", "", fmt.Errorf("invalid auth secret: expected at least 16 bytes")
	}

	return normalizedP256DH, normalizedAuth, nil
}

func decodeBase64URL(value string) ([]byte, string, error) {
	cleaned := strings.TrimSpace(value)
	if cleaned == "" {
		return nil, "", fmt.Errorf("empty value")
	}

	encodings := []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	}
	for _, enc := range encodings {
		if decoded, err := enc.DecodeString(cleaned); err == nil {
			return decoded, base64.RawURLEncoding.EncodeToString(decoded), nil
		}
	}

	if missing := len(cleaned) % 4; missing != 0 {
		padded := cleaned + strings.Repeat("=", 4-missing)
		for _, enc := range []*base64.Encoding{base64.URLEncoding, base64.StdEncoding} {
			if decoded, err := enc.DecodeString(padded); err == nil {
				return decoded, base64.RawURLEncoding.EncodeToString(decoded), nil
			}
		}
	}

	return nil, "", fmt.Errorf("illegal base64 data")
}

// Send sends a push notification to the given subscription.
// payload is the notification data to send.
// urgency is one of "very-low", "low", "normal", "high" (RFC 8030 §5.3).
func (s *Sender) Send(sub *Subscription, payload *NotificationPayload, urgency string) error {
	endpoint, err := NormalizeEndpoint(sub.Endpoint)
	if err != nil {
		return err
	}
	sub.Endpoint = endpoint
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	// The encoding and its headers follow Mastodon's push worker exactly,
	// since that is what every Mastodon client and push relay is built
	// against. Standard subscriptions get RFC 8291 aes128gcm with RFC 8292
	// VAPID; the rest get the aesgcm draft, whose salt and server key travel
	// in the Encryption and Crypto-Key headers. A Crypto-Key header on a
	// standard push would make clients such as Mastodon for Android take it
	// for the legacy encoding.
	var body []byte
	var salt, serverPub []byte
	if sub.Standard {
		body, _, _, err = encryptPayload(payloadBytes, sub.P256DH, sub.Auth)
	} else {
		body, salt, serverPub, err = encryptLegacyPayload(payloadBytes, sub.P256DH, sub.Auth)
	}
	if err != nil {
		return fmt.Errorf("encrypt payload: %w", err)
	}

	req, err := http.NewRequest("POST", sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", "86400") // 24 hours
	if urgency != "" {
		req.Header.Set("Urgency", urgency)
	}

	var cryptoKey []string
	if sub.Standard {
		req.Header.Set("Content-Encoding", "aes128gcm")
	} else {
		req.Header.Set("Content-Encoding", "aesgcm")
		req.Header.Set("Encryption", "salt="+base64.RawURLEncoding.EncodeToString(salt))
		cryptoKey = append(cryptoKey, "dh="+base64.RawURLEncoding.EncodeToString(serverPub))
	}
	if s.vapid != nil {
		jwt, err := s.vapidJWT(sub.Endpoint)
		if err != nil {
			return fmt.Errorf("vapid: %w", err)
		}
		if sub.Standard {
			req.Header.Set("Authorization", "vapid t="+jwt+", k="+s.vapid.Public)
		} else {
			req.Header.Set("Authorization", "WebPush "+jwt)
			cryptoKey = append(cryptoKey, "p256ecdsa="+s.vapid.Public)
		}
	}
	if len(cryptoKey) > 0 {
		req.Header.Set("Crypto-Key", strings.Join(cryptoKey, ";"))
	}

	slog.Debug("push: sending notification",
		"endpoint", truncateEndpoint(sub.Endpoint),
		"payload_size", len(payloadBytes),
	)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("push request: %w", err)
	}
	defer resp.Body.Close()

	// Read response body for error details
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	// Handle 404/410 (subscription is invalid/unsubscribed)
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return fmt.Errorf("subscription expired (%d): %w", resp.StatusCode, ErrSubscriptionExpired)
	}

	// Handle 429 Too Many Requests (rate limited)
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("push rate limited (429): %w", ErrRateLimited)
	}

	return fmt.Errorf("push failed (status %d): %s", resp.StatusCode, string(respBody))
}

// vapidJWT signs the VAPID token (RFC 8292) for the endpoint's push service.
func (s *Sender) vapidJWT(endpoint string) (string, error) {
	// Decode private key
	privRaw, err := base64.RawURLEncoding.DecodeString(s.vapid.Private)
	if err != nil {
		return "", fmt.Errorf("decode vapid private key: %w", err)
	}

	// Parse private key
	priv := new(ecdsa.PrivateKey)
	priv.PublicKey.Curve = elliptic.P256()
	priv.D = new(big.Int).SetBytes(privRaw)
	priv.PublicKey.X, priv.PublicKey.Y = priv.PublicKey.Curve.ScalarBaseMult(privRaw)

	// Build JWT header
	header := map[string]string{
		"typ": "JWT",
		"alg": "ES256",
	}
	headerJSON, _ := json.Marshal(header)
	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)

	endpointURL, err := url.Parse(endpoint)
	if err != nil || endpointURL.Scheme == "" || endpointURL.Host == "" {
		return "", fmt.Errorf("invalid vapid audience endpoint")
	}
	audience := endpointURL.Scheme + "://" + endpointURL.Host

	// Build JWT payload. RFC 8292 requires the push service's origin, not the
	// full subscription URL, as the audience.
	now := time.Now().Unix()
	payload := map[string]interface{}{
		"aud": audience,
		"exp": now + 86400, // 24 hours
		"sub": s.subject,
	}
	payloadJSON, _ := json.Marshal(payload)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)

	// Sign
	signingInput := headerB64 + "." + payloadB64
	hash := sha256.Sum256([]byte(signingInput))
	r, ss, err := ecdsa.Sign(rand.Reader, priv, hash[:])
	if err != nil {
		return "", fmt.Errorf("sign vapid jwt: %w", err)
	}

	// Encode signature as fixed-width raw R||S (64 bytes). Big.Int.Bytes drops
	// leading zeroes, so concatenating it directly produces intermittent 63-byte
	// signatures that push services reject.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	ss.FillBytes(sig[32:])
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)

	return headerB64 + "." + payloadB64 + "." + sigB64, nil
}

// encryptPayload encrypts the payload according to RFC 8291 (aes128gcm).
// Returns the full encrypted blob (header + ciphertext + tag), the salt, and the server public key.
func encryptPayload(plaintext []byte, clientPubB64, authB64 string) (encrypted []byte, salt []byte, serverPub []byte, err error) {
	// Decode client keys
	clientPub, _, err := decodeBase64URL(clientPubB64)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decode client public key: %w", err)
	}
	authSecret, _, err := decodeBase64URL(authB64)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decode auth secret: %w", err)
	}

	// Parse client public key (uncompressed point: 04 || x || y, 65 bytes)
	x, y := elliptic.Unmarshal(elliptic.P256(), clientPub)
	if x == nil || y == nil {
		return nil, nil, nil, fmt.Errorf("invalid client public key")
	}

	// Generate ephemeral ECDH key pair
	ephemeral, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate ephemeral key: %w", err)
	}

	// Compute ECDH shared secret
	sharedX, sharedY := elliptic.P256().ScalarMult(x, y, ephemeral.D.Bytes())
	if sharedX == nil || sharedY == nil {
		return nil, nil, nil, fmt.Errorf("ecdh: point at infinity")
	}
	sharedSecret := sharedX.Bytes()
	// Pad to 32 bytes
	if len(sharedSecret) < 32 {
		padded := make([]byte, 32)
		copy(padded[32-len(sharedSecret):], sharedSecret)
		sharedSecret = padded
	}

	// Generate random salt (16 bytes)
	salt = make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, nil, fmt.Errorf("generate salt: %w", err)
	}

	// Encode server ephemeral public key (uncompressed)
	serverPub = elliptic.Marshal(elliptic.P256(), ephemeral.PublicKey.X, ephemeral.PublicKey.Y)

	contentEncryptionKey, nonce, err := deriveAES128GCMKeys(sharedSecret, authSecret, salt, clientPub, serverPub)
	if err != nil {
		return nil, nil, nil, err
	}

	// Encrypt with AES-128-GCM
	block, err := aes.NewCipher(contentEncryptionKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create gcm: %w", err)
	}

	// Construct one final RFC 8188 record. The advertised record size includes
	// the authentication tag; 4096 is the interoperable Web Push default.
	const recordSize = 4096
	if len(plaintext)+1+gcm.Overhead() > recordSize {
		return nil, nil, nil, fmt.Errorf("push payload too large")
	}
	record := make([]byte, len(plaintext)+1)
	copy(record[:len(plaintext)], plaintext)
	// 0x02 marks the final record; no padding follows it.
	record[len(plaintext)] = 0x02

	// Encrypt with GCM: nonce is 12 bytes
	ciphertext := gcm.Seal(nil, nonce, record, nil)

	// Build the aes128gcm header (21 bytes)
	// Header format: salt(16) || record_size(4) || server_key_length(1) || server_key(variable)
	header := make([]byte, 0, 21+len(serverPub))
	header = append(header, salt...)
	// Record size as 32-bit big-endian
	rs := make([]byte, 4)
	binary.BigEndian.PutUint32(rs, recordSize)
	header = append(header, rs...)
	// Server key length
	header = append(header, byte(len(serverPub)))
	header = append(header, serverPub...)

	// Final output: header || ciphertext (includes GCM tag)
	encrypted = append(header, ciphertext...)

	return encrypted, salt, serverPub, nil
}

// encryptLegacyPayload encrypts the payload with the aesgcm draft encoding
// (draft-ietf-webpush-encryption-04), the default Mastodon uses for clients
// that have not opted in to RFC 8291. The body is the bare ciphertext; the
// salt and server public key go out in the Encryption and Crypto-Key headers.
func encryptLegacyPayload(plaintext []byte, clientPubB64, authB64 string) (ciphertext, salt, serverPub []byte, err error) {
	clientPub, _, err := decodeBase64URL(clientPubB64)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decode client public key: %w", err)
	}
	authSecret, _, err := decodeBase64URL(authB64)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decode auth secret: %w", err)
	}
	clientKey, err := ecdh.P256().NewPublicKey(clientPub)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid client public key: %w", err)
	}
	ephemeral, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate ephemeral key: %w", err)
	}
	sharedSecret, err := ephemeral.ECDH(clientKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ecdh: %w", err)
	}
	serverPub = ephemeral.PublicKey().Bytes()

	salt = make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, nil, fmt.Errorf("generate salt: %w", err)
	}

	ikm, err := hkdfExpand(hkdfExtract(authSecret, sharedSecret), []byte("Content-Encoding: auth\x00"), 32)
	if err != nil {
		return nil, nil, nil, err
	}
	// context = "P-256" 0x00 len(client) client len(server) server, with
	// 16-bit big-endian lengths.
	context := []byte("P-256\x00")
	context = binary.BigEndian.AppendUint16(context, uint16(len(clientPub)))
	context = append(context, clientPub...)
	context = binary.BigEndian.AppendUint16(context, uint16(len(serverPub)))
	context = append(context, serverPub...)

	prk := hkdfExtract(salt, ikm)
	cek, err := hkdfExpand(prk, append([]byte("Content-Encoding: aesgcm\x00"), context...), 16)
	if err != nil {
		return nil, nil, nil, err
	}
	nonce, err := hkdfExpand(prk, append([]byte("Content-Encoding: nonce\x00"), context...), 12)
	if err != nil {
		return nil, nil, nil, err
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create gcm: %w", err)
	}
	// One record: a two-byte padding length (zero) and the payload. The
	// draft's default record size, 4096, includes the tag.
	if len(plaintext)+2+gcm.Overhead() > 4096 {
		return nil, nil, nil, fmt.Errorf("push payload too large")
	}
	record := append([]byte{0, 0}, plaintext...)
	return gcm.Seal(nil, nonce, record, nil), salt, serverPub, nil
}

func deriveAES128GCMKeys(sharedSecret, authSecret, salt, clientPub, serverPub []byte) (contentEncryptionKey, nonce []byte, err error) {
	prk := hkdfExtract(authSecret, sharedSecret)

	info := []byte("WebPush: info\x00")
	info = append(info, clientPub...)
	info = append(info, serverPub...)
	ikm, err := hkdfExpand(prk, info, 32)
	if err != nil {
		return nil, nil, fmt.Errorf("hkdf webpush info: %w", err)
	}

	prk2 := hkdfExtract(salt, ikm)
	contentEncryptionKey, err = hkdfExpand(prk2, []byte("Content-Encoding: aes128gcm\x00"), 16)
	if err != nil {
		return nil, nil, fmt.Errorf("hkdf cek: %w", err)
	}
	nonce, err = hkdfExpand(prk2, []byte("Content-Encoding: nonce\x00"), 12)
	if err != nil {
		return nil, nil, fmt.Errorf("hkdf nonce: %w", err)
	}
	return contentEncryptionKey, nonce, nil
}

func hkdfExtract(salt, ikm []byte) []byte {
	mac := hmac.New(sha256.New, salt)
	mac.Write(ikm)
	return mac.Sum(nil)
}

func hkdfExpand(prk, info []byte, length int) ([]byte, error) {
	var result []byte
	var previous []byte
	counter := byte(1)
	for len(result) < length {
		mac := hmac.New(sha256.New, prk)
		mac.Write(previous)
		mac.Write(info)
		mac.Write([]byte{counter})
		previous = mac.Sum(nil)
		result = append(result, previous...)
		counter++
	}
	return result[:length], nil
}

// truncateEndpoint truncates a push endpoint URL for logging.
func truncateEndpoint(endpoint string) string {
	if len(endpoint) > 60 {
		return endpoint[:60] + "..."
	}
	return endpoint
}

// Sentinel errors for known push failure modes.
var (
	ErrSubscriptionExpired = fmt.Errorf("subscription expired")
	ErrRateLimited         = fmt.Errorf("rate limited")
)
