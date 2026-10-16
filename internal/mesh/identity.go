// Package mesh implements littleMesh, a transport that lets littleFedi
// instances federate without a public IP address, a DNS name, or an inbound
// port.
//
// A node is identified by an Ed25519 key pair. Its ActivityPub domain is
// "<node-id>.mesh.<lighthouse-host>", where node-id is the base32 encoding of
// the public key, so the name is both self-certifying and a routing hint: any
// peer holding the domain knows which lighthouse to ask for a circuit.
//
// Nothing above the socket changes. A mesh circuit is a net.Conn and a mesh
// node exposes a net.Listener, so ActivityPub, WebFinger, HTTP Signatures and
// the delivery queue run over it unmodified.
package mesh

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

const (
	// ALPN is the protocol name negotiated on every end-to-end mesh session.
	ALPN = "littlemesh/1"

	// NodeIDLen is the length of a base32-encoded Ed25519 public key. 32 bytes
	// encode to 52 unpadded characters, which fits a 63-octet DNS label.
	NodeIDLen = 52

	// DefaultLighthousePort is the control/relay port used when a lighthouse
	// address carries no port of its own.
	DefaultLighthousePort = "7842"
)

// nodeEncoding is unpadded base32 in the lowercase alphabet used by DNS
// labels. The standard alphabet is uppercase, so encoded values are lowered on
// the way out and raised on the way back in.
var nodeEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Identity is a node's long-term key pair together with the self-signed
// certificate that carries it into a TLS handshake.
type Identity struct {
	NodeID string

	priv ed25519.PrivateKey
	cert tls.Certificate
}

// NewIdentity generates a fresh node identity.
func NewIdentity() (*Identity, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mesh: generate identity: %w", err)
	}
	return identityFromKey(priv)
}

// IdentityFromPEM restores an identity from its stored PKCS#8 private key.
func IdentityFromPEM(pemText string) (*Identity, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, fmt.Errorf("mesh: identity is not PEM encoded")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("mesh: parse identity: %w", err)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("mesh: identity must be an Ed25519 key, got %T", parsed)
	}
	return identityFromKey(priv)
}

func identityFromKey(priv ed25519.PrivateKey) (*Identity, error) {
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("mesh: identity public key is not Ed25519")
	}
	nodeID := NodeIDFromPublicKey(pub)

	// The certificate is a carrier for the public key, not a claim anyone
	// verifies against a CA: peers compare the key itself against the node ID
	// they meant to reach. A century of validity therefore removes a renewal
	// path that would add moving parts without adding a security property.
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: nodeID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(100, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("mesh: create certificate: %w", err)
	}

	return &Identity{
		NodeID: nodeID,
		priv:   priv,
		cert: tls.Certificate{
			Certificate: [][]byte{der},
			PrivateKey:  priv,
		},
	}, nil
}

// PrivateKeyPEM returns the PKCS#8 PEM encoding for persistence.
func (id *Identity) PrivateKeyPEM() (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(id.priv)
	if err != nil {
		return "", fmt.Errorf("mesh: marshal identity: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// NodeIDFromPublicKey renders a public key as a DNS-safe node ID.
func NodeIDFromPublicKey(pub ed25519.PublicKey) string {
	return strings.ToLower(nodeEncoding.EncodeToString(pub))
}

// PublicKeyFromNodeID reverses NodeIDFromPublicKey.
func PublicKeyFromNodeID(nodeID string) (ed25519.PublicKey, error) {
	if len(nodeID) != NodeIDLen {
		return nil, fmt.Errorf("mesh: node id must be %d characters, got %d", NodeIDLen, len(nodeID))
	}
	raw, err := nodeEncoding.DecodeString(strings.ToUpper(nodeID))
	if err != nil {
		return nil, fmt.Errorf("mesh: node id is not base32: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("mesh: node id decodes to %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// ValidNodeID reports whether s is a syntactically valid node ID.
func ValidNodeID(s string) bool {
	_, err := PublicKeyFromNodeID(s)
	return err == nil
}

// ClientConfig returns the TLS configuration for dialing a peer. When expect
// is non-empty the peer must present exactly that node ID; when it is empty
// (a lighthouse, whose identity a caller may not know in advance) the peer is
// merely recorded. Confidentiality of relayed traffic never rests on the
// lighthouse handshake: the payload is a second, pinned TLS session nested
// inside the circuit.
func (id *Identity) ClientConfig(expect string, peer *string) *tls.Config {
	return &tls.Config{
		Certificates:           []tls.Certificate{id.cert},
		MinVersion:             tls.VersionTLS13,
		NextProtos:             []string{ALPN},
		SessionTicketsDisabled: true,
		// The peer is authenticated by raw public key, so chain building,
		// name matching and expiry have nothing to check. VerifyConnection
		// below is the real verification and also runs for resumed sessions.
		InsecureSkipVerify: true,
		ServerName:         "littlemesh",
		VerifyConnection:   pinConnection(expect, peer),
	}
}

// ServerConfig returns the TLS configuration for accepting a peer. Every
// caller must present a certificate; its key becomes the peer's node ID and is
// written to peer. When expect is non-empty the caller must be that node,
// which is how the target of a relayed circuit confirms it was joined to the
// peer the lighthouse named rather than to whoever attached first.
func (id *Identity) ServerConfig(expect string, peer *string) *tls.Config {
	return &tls.Config{
		Certificates:           []tls.Certificate{id.cert},
		MinVersion:             tls.VersionTLS13,
		NextProtos:             []string{ALPN},
		ClientAuth:             tls.RequireAnyClientCert,
		SessionTicketsDisabled: true,
		VerifyConnection:       pinConnection(expect, peer),
	}
}

// pinConnection checks that the presented certificate carries an Ed25519 key
// whose node ID matches expect, and records the observed node ID in peer.
func pinConnection(expect string, peer *string) func(tls.ConnectionState) error {
	return func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return fmt.Errorf("mesh: peer presented no certificate")
		}
		cert := state.PeerCertificates[0]
		pub, ok := cert.PublicKey.(ed25519.PublicKey)
		if !ok {
			return fmt.Errorf("mesh: peer key is %T, want Ed25519", cert.PublicKey)
		}
		got := NodeIDFromPublicKey(pub)
		if expect != "" && got != expect {
			return fmt.Errorf("mesh: peer is %s, expected %s", got, expect)
		}
		if peer != nil {
			*peer = got
		}
		return nil
	}
}

// SyntheticIP maps a node ID into a stable unique-local IPv6 address. Mesh
// peers have no IP address of their own, but the HTTP stack keys rate-limit
// buckets and proxy trust off RemoteAddr, so every peer needs one address that
// parses, never collides with a real network, and never lands in an operator's
// trusted-proxy set.
func SyntheticIP(nodeID string) net.IP {
	sum := sha256.Sum256([]byte(nodeID))
	ip := make(net.IP, net.IPv6len)
	copy(ip, sum[:net.IPv6len])
	ip[0] = 0xfd // fd00::/8, unique-local
	return ip
}

// Addr is the net.Addr of a mesh peer.
type Addr struct {
	NodeID string
}

func (a Addr) Network() string { return "mesh" }

func (a Addr) String() string {
	return net.JoinHostPort(SyntheticIP(a.NodeID).String(), "1")
}
