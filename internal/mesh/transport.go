package mesh

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// defaultNode is the process-wide mesh node. Outbound clients that are built
// deep inside other packages (remote media, link previews) reach it through
// WrapDefault instead of having a node threaded through their constructors.
// It is written once at startup, before any of those clients exist.
var defaultNode atomic.Pointer[Node]

// SetDefault publishes the node used by WrapDefault. A nil node leaves every
// wrapped client on its ordinary transport.
func SetDefault(node *Node) { defaultNode.Store(node) }

// Default returns the process-wide mesh node, or nil when mesh is disabled.
func Default() *Node { return defaultNode.Load() }

// WrapDefault routes mesh hostnames over the process-wide node, if there is
// one, and changes nothing otherwise. The node is looked up per request rather
// than captured, so a client built before the mesh comes up still gets it, and
// no call site has to care about startup order.
func WrapDefault(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &defaultSwitcher{base: base}
}

type defaultSwitcher struct {
	base http.RoundTripper
}

func (s *defaultSwitcher) RoundTrip(req *http.Request) (*http.Response, error) {
	if node := Default(); node != nil && node.Matches(req.URL.Hostname()) {
		return node.Transport().RoundTrip(req)
	}
	return s.base.RoundTrip(req)
}

// WrapTransport routes requests for mesh hostnames over the mesh and leaves
// every other request on base. This is the whole outbound integration: the
// federation, media and link-preview clients keep their own transports,
// timeouts and SSRF guards for the public internet and gain mesh reachability
// by being wrapped once at construction.
//
// A nil node returns base unchanged, so callers need no conditional.
func WrapTransport(base http.RoundTripper, node *Node) http.RoundTripper {
	if node == nil {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &switcher{base: base, node: node}
}

type switcher struct {
	base http.RoundTripper
	node *Node
}

func (s *switcher) RoundTrip(req *http.Request) (*http.Response, error) {
	if s.node.Matches(req.URL.Hostname()) {
		return s.node.Transport().RoundTrip(req)
	}
	return s.base.RoundTrip(req)
}

// Transport returns the node's HTTP transport, which speaks only to mesh
// peers. It is created once per node so circuits are pooled and reused.
func (n *Node) Transport() http.RoundTripper { return n.transport }

func newMeshTransport(n *Node) http.RoundTripper {
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return n.DialContext(ctx, network, address)
	}
	return &http.Transport{
		DialContext: dial,
		// A circuit is already an authenticated, confidential TLS session
		// pinned to the peer's node ID, so https:// URLs travel inside it
		// without a second handshake. Returning a plain conn from
		// DialTLSContext is how net/http is told the transport-level security
		// is already in place; no public CA could vouch for a .mesh name in
		// the first place.
		DialTLSContext: dial,
		// Every circuit costs a relayed TCP connection plus a handshake, so
		// keeping a few warm matters more here than on the public internet.
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     5 * time.Minute,
		// Without a tls.Conn there is no ALPN result to negotiate h2 from;
		// asking for it would only add a failed upgrade attempt per circuit.
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: 60 * time.Second,
	}
}
