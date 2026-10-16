package mesh

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/netutil"
)

// Config configures a mesh node.
type Config struct {
	// Suffix is the mesh suffix this node's own domain uses, without the node
	// ID, e.g. "mesh.faro.example". Empty derives it from the first lighthouse.
	Suffix string
	// Lighthouses are the relay addresses this node registers with, as
	// "host[:port]" or "host[:port]@node-id" to pin the lighthouse identity.
	Lighthouses []string
	// Directory resolves signed, replaceable lighthouse lists for stable node
	// domains. Nil keeps the configured-lighthouse behavior.
	Directory *DirectoryClient
	// DiscoverLighthouses allows a foreign mesh suffix to select a relay that is
	// not present in Lighthouses. Keep disabled for bounded outbound networking.
	DiscoverLighthouses bool
	// DirectListen, when set, accepts unrelayed connections from peers that
	// can reach this node without help.
	DirectListen string
	// DirectAdvertise are the addresses peers should try before falling back
	// to the relay.
	DirectAdvertise []string
	// AllowPrivateDirect permits direct addresses on private networks, which
	// is what makes two nodes on the same LAN skip the relay entirely.
	AllowPrivateDirect bool
	// MaxCircuits bounds concurrent circuits in both directions.
	MaxCircuits int
	// DialTimeout bounds a whole outbound circuit setup.
	DialTimeout time.Duration
	// DirectTimeout bounds one attempt at an advertised direct address before
	// giving up on it and using the relay.
	DirectTimeout time.Duration
	// HandshakeTimeout bounds every pre-payload exchange with a lighthouse or
	// a peer.
	HandshakeTimeout time.Duration
	// PingInterval is how often the control connection is kept alive. It must
	// stay well under the shortest NAT mapping lifetime on the path.
	PingInterval time.Duration
}

func (c *Config) applyDefaults() {
	if c.MaxCircuits <= 0 {
		c.MaxCircuits = 64
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = 30 * time.Second
	}
	if c.DirectTimeout <= 0 {
		c.DirectTimeout = 2 * time.Second
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = defaultHandshakeTimeout
	}
	if c.PingInterval <= 0 {
		c.PingInterval = defaultPingInterval
	}
}

// controlReadTimeout is how long this node waits for anything from its
// lighthouse before treating the connection as dead.
func (c *Config) controlReadTimeout() time.Duration { return 3 * c.PingInterval }

// lighthouseRef is a parsed lighthouse entry.
type lighthouseRef struct {
	address string
	// pin, when set, is the node ID the lighthouse must present. Leaving it
	// empty is safe for payload confidentiality (the end-to-end session inside
	// the circuit is always pinned) but leaves circuit setup open to a
	// man-in-the-middle that can deny service.
	pin        string
	publicOnly bool
}

func parseLighthouse(entry string) (lighthouseRef, error) {
	address, pin, _ := strings.Cut(strings.TrimSpace(entry), "@")
	address = strings.TrimSpace(address)
	pin = strings.ToLower(strings.TrimSpace(pin))
	if address == "" {
		return lighthouseRef{}, fmt.Errorf("mesh: empty lighthouse address")
	}
	if pin != "" && !ValidNodeID(pin) {
		return lighthouseRef{}, fmt.Errorf("mesh: lighthouse pin %q is not a node id", pin)
	}
	return lighthouseRef{address: withDefaultPort(address), pin: pin}, nil
}

// Node is a participant in the mesh. It keeps one outbound control connection
// per lighthouse, dials circuits to peers, and hands inbound circuits to a
// net.Listener the HTTP server can serve.
type Node struct {
	id     *Identity
	cfg    Config
	suffix string
	hubs   []lighthouseRef

	incoming   chan net.Conn
	closed     chan struct{}
	closeOnce  sync.Once
	slots      chan struct{}
	directAddr string
	transport  http.RoundTripper

	mu sync.Mutex
	// directCache remembers the addresses a peer advertised, so later circuits
	// to the same peer can try a direct connection before touching a relay.
	directCache map[string][]string
	// lighthouseCache remembers which relay last reached a peer, so a node
	// that failed over to its second lighthouse is found immediately next
	// time instead of after another round of timeouts.
	lighthouseCache map[string]lighthouseRef
	connected       map[string]bool
}

// New creates a node. Call Start to bring it online.
func New(id *Identity, cfg Config) (*Node, error) {
	if id == nil {
		return nil, errors.New("mesh: node needs an identity")
	}
	cfg.applyDefaults()

	hubs := make([]lighthouseRef, 0, len(cfg.Lighthouses))
	for _, entry := range cfg.Lighthouses {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		ref, err := parseLighthouse(entry)
		if err != nil {
			return nil, err
		}
		hubs = append(hubs, ref)
	}
	if len(hubs) == 0 && cfg.DirectListen == "" && cfg.Directory == nil {
		return nil, errors.New("mesh: configure at least one lighthouse, a directory, or a direct listener")
	}

	suffix := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(cfg.Suffix)), ".")
	if suffix == "" {
		if len(hubs) == 0 {
			return nil, errors.New("mesh: suffix is required when no lighthouse is configured")
		}
		host, port, err := net.SplitHostPort(hubs[0].address)
		if err != nil {
			return nil, fmt.Errorf("mesh: derive suffix from lighthouse: %w", err)
		}
		if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
			return nil, errors.New("mesh: suffix is required when the first lighthouse uses IPv6")
		}
		if port != DefaultLighthousePort {
			return nil, fmt.Errorf("mesh: suffix is required when the first lighthouse uses non-default port %s", port)
		}
		suffix = SuffixForLighthouse(hubs[0].address)
	}
	if !ValidSuffix(suffix) {
		return nil, fmt.Errorf("mesh: suffix %q is not a DNS-safe mesh suffix", suffix)
	}

	node := &Node{
		id:              id,
		cfg:             cfg,
		suffix:          suffix,
		hubs:            hubs,
		incoming:        make(chan net.Conn),
		closed:          make(chan struct{}),
		slots:           make(chan struct{}, cfg.MaxCircuits),
		directCache:     make(map[string][]string),
		lighthouseCache: make(map[string]lighthouseRef),
		connected:       make(map[string]bool),
	}
	node.transport = newMeshTransport(node)
	return node, nil
}

// NodeID returns this node's identity.
func (n *Node) NodeID() string { return n.id.NodeID }

// Suffix returns the mesh suffix this node's domain uses.
func (n *Node) Suffix() string { return n.suffix }

// Domain returns the ActivityPub domain this node is reachable at.
func (n *Node) Domain() string { return n.id.NodeID + "." + n.suffix }

// DirectAddr reports the address the direct listener bound to, if any.
func (n *Node) DirectAddr() string { return n.directAddr }

func hostOf(address string) string {
	if host, _, err := net.SplitHostPort(address); err == nil {
		return strings.Trim(strings.ToLower(host), "[]")
	}
	return strings.ToLower(address)
}

// wildcardHost reports whether a listen address binds every interface, in
// which case it says nothing about how peers should reach this node.
func wildcardHost(listen string) bool {
	host := hostOf(listen)
	return host == "" || host == "0.0.0.0" || host == "::"
}

// Matches reports whether a hostname should be routed over the mesh. Any
// "<node-id>.mesh..." name qualifies, including names rooted at a lighthouse
// this node does not itself use: the suffix says where to ask for the circuit.
func (n *Node) Matches(host string) bool {
	_, suffix, ok := SplitDomain(host)
	return ok && (suffix == "mesh" || strings.HasPrefix(suffix, "mesh."))
}

// Start brings the node online: control connections to every lighthouse plus,
// if configured, a listener for unrelayed connections.
func (n *Node) Start(ctx context.Context) error {
	if n.cfg.DirectListen != "" {
		listener, err := net.Listen("tcp", n.cfg.DirectListen)
		if err != nil {
			return fmt.Errorf("mesh: direct listener: %w", err)
		}
		n.directAddr = listener.Addr().String()
		// Binding a concrete address is the operator saying "peers can reach
		// me here", so advertise it rather than making them repeat it. A
		// wildcard bind says nothing about what the outside world sees.
		if len(n.cfg.DirectAdvertise) == 0 && !wildcardHost(n.cfg.DirectListen) {
			n.cfg.DirectAdvertise = []string{n.directAddr}
		}
		slog.Info("mesh direct listener ready", "addr", n.directAddr, "advertised", n.cfg.DirectAdvertise)
		go n.acceptDirect(ctx, listener)
	}
	for _, hub := range n.hubs {
		go n.controlLoop(ctx, hub)
	}
	slog.Info("mesh node started", "node_id", n.id.NodeID, "domain", n.Domain(), "lighthouses", len(n.hubs))
	return nil
}

// Close stops accepting circuits. In-flight connections are left to finish.
func (n *Node) Close() error {
	n.closeOnce.Do(func() { close(n.closed) })
	return nil
}

// Connected reports which lighthouses currently hold a live control connection.
func (n *Node) Connected() map[string]bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make(map[string]bool, len(n.connected))
	for k, v := range n.connected {
		out[k] = v
	}
	return out
}

// controlLoop keeps one lighthouse control connection up, reconnecting with
// capped exponential backoff.
func (n *Node) controlLoop(ctx context.Context, hub lighthouseRef) {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.closed:
			return
		default:
		}

		err := n.runControl(ctx, hub, func() { backoff = time.Second })
		if err != nil && ctx.Err() == nil {
			slog.Warn("mesh control connection lost", "lighthouse", hub.address, "error", err, "retry_in", backoff)
		}

		// Equal jitter: half the delay fixed, half random, so a lighthouse
		// restart does not bring every node back in the same instant.
		delay := backoff/2 + time.Duration(rand.Int63n(int64(backoff/2)+1))
		select {
		case <-ctx.Done():
			return
		case <-n.closed:
			return
		case <-time.After(delay):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func (n *Node) runControl(ctx context.Context, hub lighthouseRef, registered func()) error {
	conn, err := n.dialLighthouse(ctx, hub)
	if err != nil {
		return err
	}
	defer conn.Close()

	// A shutting-down node must drop its registration promptly: until the
	// lighthouse notices, peers are handed circuits to somewhere nobody is
	// listening. Waiting for the read timeout would leave that window open for
	// minutes.
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-n.closed:
			conn.Close()
		case <-ctx.Done():
			conn.Close()
		case <-stopped:
		}
	}()

	if err := conn.SetDeadline(time.Now().Add(n.cfg.HandshakeTimeout)); err != nil {
		return err
	}
	if err := writeFrame(conn, frame{Op: opRegister, Direct: n.cfg.DirectAdvertise}); err != nil {
		return err
	}
	reader := newFrameReader(conn)
	reply, err := readFrame(reader)
	if err != nil {
		return err
	}
	if reply.Op != opOK {
		return fmt.Errorf("mesh: lighthouse refused registration: %s", reply.Error)
	}
	// Drop the handshake deadline: from here the connection is long-lived, and
	// a leftover write deadline would kill it at the first keepalive.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return err
	}
	registered()

	n.setConnected(hub.address, true)
	defer n.setConnected(hub.address, false)
	slog.Info("mesh registered with lighthouse", "lighthouse", hub.address, "domain", n.Domain())

	pingCtx, stopPings := context.WithCancel(ctx)
	defer stopPings()
	var writeMu sync.Mutex
	go func() {
		ticker := time.NewTicker(n.cfg.PingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-ticker.C:
				writeMu.Lock()
				err := sendFrame(conn, frame{Op: opPing}, n.cfg.HandshakeTimeout)
				writeMu.Unlock()
				if err != nil {
					conn.Close()
					return
				}
			}
		}
	}()

	for {
		if err := conn.SetReadDeadline(time.Now().Add(n.cfg.controlReadTimeout())); err != nil {
			return err
		}
		f, err := readFrame(reader)
		if err != nil {
			return err
		}
		switch f.Op {
		case opAccept:
			go n.attachCircuit(ctx, hub, f.Circuit, f.From)
		case opPing:
			writeMu.Lock()
			err = sendFrame(conn, frame{Op: opPong}, n.cfg.HandshakeTimeout)
			writeMu.Unlock()
			if err != nil {
				return err
			}
		case opPong:
		case opError:
			return fmt.Errorf("mesh: lighthouse error: %s", f.Error)
		default:
			slog.Debug("mesh ignoring unknown control frame", "op", f.Op)
		}
	}
}

// dialLighthouse opens an authenticated connection to a lighthouse. Every
// connection to a lighthouse (control, open, attach) starts this way.
func (n *Node) dialLighthouse(ctx context.Context, hub lighthouseRef) (*tls.Conn, error) {
	dialer := &net.Dialer{Timeout: n.cfg.HandshakeTimeout}
	if hub.publicOnly {
		dialer.Control = publicDialGuard("lighthouse")
	}
	raw, err := dialer.DialContext(ctx, "tcp", hub.address)
	if err != nil {
		return nil, fmt.Errorf("mesh: dial lighthouse %s: %w", hub.address, err)
	}
	var peer string
	conn := tls.Client(raw, n.id.ClientConfig(hub.pin, &peer))
	if err := handshake(ctx, conn, n.cfg.HandshakeTimeout); err != nil {
		raw.Close()
		return nil, fmt.Errorf("mesh: lighthouse %s handshake: %w", hub.address, err)
	}
	return conn, nil
}

// attachCircuit answers a lighthouse's accept notice by calling back with a
// fresh connection, which the lighthouse then splices to the caller.
func (n *Node) attachCircuit(ctx context.Context, hub lighthouseRef, circuit, from string) {
	if circuit == "" || !ValidNodeID(from) {
		return
	}
	release, ok := n.acquire(ctx)
	if !ok {
		slog.Warn("mesh circuit limit reached, refusing inbound circuit", "from", from)
		return
	}

	conn, err := n.dialLighthouse(ctx, hub)
	if err != nil {
		release()
		slog.Warn("mesh attach failed", "circuit", circuit, "error", err)
		return
	}
	if err := conn.SetDeadline(time.Now().Add(n.cfg.HandshakeTimeout)); err != nil {
		conn.Close()
		release()
		return
	}
	if err := writeFrame(conn, frame{Op: opAttach, Circuit: circuit}); err != nil {
		conn.Close()
		release()
		slog.Warn("mesh attach failed", "circuit", circuit, "error", err)
		return
	}
	reply, err := readFrame(newFrameReader(conn))
	if err != nil || reply.Op != opOK {
		conn.Close()
		release()
		slog.Warn("mesh attach refused", "circuit", circuit, "error", errOrFrame(err, reply))
		return
	}

	n.serveInbound(ctx, conn, from, release)
}

// acceptDirect serves peers that can reach this node without a relay.
func (n *Node) acceptDirect(ctx context.Context, listener net.Listener) {
	defer listener.Close()
	go func() {
		select {
		case <-ctx.Done():
		case <-n.closed:
		}
		listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("mesh direct listener stopped", "error", err)
			}
			return
		}
		release, ok := n.acquire(ctx)
		if !ok {
			conn.Close()
			continue
		}
		go n.serveInbound(ctx, conn, "", release)
	}
}

// serveInbound completes the end-to-end handshake on an inbound circuit and
// hands the result to Accept. expect, when set, is the peer the lighthouse
// said was calling; a mismatch means the circuit was hijacked.
func (n *Node) serveInbound(ctx context.Context, raw net.Conn, expect string, release func()) {
	var peer string
	conn := tls.Server(raw, n.id.ServerConfig(expect, &peer))
	if err := handshake(ctx, conn, n.cfg.HandshakeTimeout); err != nil {
		raw.Close()
		release()
		slog.Warn("mesh inbound handshake failed", "expected", expect, "error", err)
		return
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		release()
		return
	}

	wrapped := &peerConn{Conn: conn, peer: Addr{NodeID: peer}, local: Addr{NodeID: n.id.NodeID}, release: release}
	select {
	case n.incoming <- wrapped:
	case <-ctx.Done():
		wrapped.Close()
	case <-n.closed:
		wrapped.Close()
	}
}

// DialContext opens a circuit to a mesh peer. It satisfies the dialer shape
// http.Transport expects, so HTTP over the mesh needs no other adaptation.
func (n *Node) DialContext(ctx context.Context, _, address string) (net.Conn, error) {
	host := address
	if h, _, err := net.SplitHostPort(address); err == nil {
		host = h
	}
	nodeID, suffix, ok := SplitDomain(host)
	if !ok {
		return nil, fmt.Errorf("mesh: %q is not a mesh address", address)
	}
	if nodeID == n.id.NodeID {
		return nil, fmt.Errorf("mesh: refusing to dial self")
	}

	ctx, cancel := context.WithTimeout(ctx, n.cfg.DialTimeout)
	defer cancel()

	release, ok := n.acquire(ctx)
	if !ok {
		return nil, fmt.Errorf("mesh: circuit limit of %d reached", n.cfg.MaxCircuits)
	}

	// A peer met before may still be at the address it advertised, so try that
	// before asking a lighthouse for anything.
	if conn, err := n.dialDirect(ctx, nodeID, n.cachedDirect(nodeID), release); err == nil {
		return conn, nil
	}

	relay, advertised, err := n.openCircuitAnywhere(ctx, suffix, nodeID)
	if err != nil {
		release()
		return nil, err
	}
	n.rememberDirect(nodeID, advertised)

	// The lighthouse just told us where the peer thinks it can be reached; a
	// direct hop now saves the relay every byte of this session.
	if conn, err := n.dialDirect(ctx, nodeID, advertised, release); err == nil {
		relay.Close()
		return conn, nil
	}

	var peer string
	conn := tls.Client(relay, n.id.ClientConfig(nodeID, &peer))
	if err := handshake(ctx, conn, n.cfg.HandshakeTimeout); err != nil {
		relay.Close()
		release()
		return nil, fmt.Errorf("mesh: end-to-end handshake with %s: %w", nodeID, err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		release()
		return nil, err
	}
	return &peerConn{Conn: conn, peer: Addr{NodeID: nodeID}, local: Addr{NodeID: n.id.NodeID}, release: release}, nil
}

// openCircuitAnywhere tries every lighthouse that might know the peer, so one
// relay going down does not take its nodes off the mesh. A node registers with
// all of its configured lighthouses, so any of them that the caller can also
// reach will do; the one named by the peer's domain is only the first guess.
func (n *Node) openCircuitAnywhere(ctx context.Context, suffix, nodeID string) (net.Conn, []string, error) {
	var lastErr error
	hubs, err := n.lighthousesForNode(ctx, suffix, nodeID)
	if err != nil {
		return nil, nil, err
	}
	for _, hub := range hubs {
		relay, advertised, err := n.openCircuit(ctx, hub, nodeID)
		if err == nil {
			n.rememberLighthouse(nodeID, hub)
			return relay, advertised, nil
		}
		lastErr = err
		slog.Debug("mesh lighthouse could not reach peer", "peer", nodeID, "lighthouse", hub.address, "error", err)
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("mesh: no lighthouse configured for %s", suffix)
	}
	return nil, nil, lastErr
}

func (n *Node) lighthousesForNode(ctx context.Context, suffix, nodeID string) ([]lighthouseRef, error) {
	hubs := n.lighthousesFor(suffix, nodeID)
	if n.cfg.Directory != nil {
		payload, err := n.cfg.Directory.ResolveNode(ctx, nodeID)
		if err != nil {
			if len(hubs) == 0 {
				return nil, err
			}
			slog.Debug("mesh directory unavailable; using manually configured lighthouses", "peer", nodeID, "error", err)
		} else {
			resolved := make([]lighthouseRef, 0, len(payload.Lighthouses)+len(hubs))
			seen := make(map[string]bool, len(payload.Lighthouses)+len(hubs))
			for _, locator := range payload.Lighthouses {
				ref := lighthouseRef{address: withDefaultPort(locator.Address), pin: locator.Pin, publicOnly: true}
				resolved = append(resolved, ref)
				seen[ref.address] = true
			}
			for _, hub := range hubs {
				if !seen[hub.address] {
					resolved = append(resolved, hub)
				}
			}
			hubs = resolved
		}
	}
	return hubs, nil
}

// lighthousesFor orders the relays to try for a peer: the one that worked last
// time, then a configured relay for our own suffix (important for gateways
// behind NAT), otherwise the relay named by the peer, then the rest of ours.
func (n *Node) lighthousesFor(suffix, nodeID string) []lighthouseRef {
	ordered := make([]lighthouseRef, 0, len(n.hubs)+2)
	seen := make(map[string]bool, len(n.hubs)+2)
	add := func(hub lighthouseRef) {
		if hub.address == "" || seen[hub.address] {
			return
		}
		seen[hub.address] = true
		ordered = append(ordered, hub)
	}

	if last, ok := n.cachedLighthouse(nodeID); ok {
		add(last)
	}
	if strings.EqualFold(suffix, n.suffix) {
		for _, hub := range n.hubs {
			add(hub)
		}
	} else if n.cfg.DiscoverLighthouses {
		address := LighthouseForSuffix(suffix)
		if address == "" {
			return ordered
		}
		// A configured entry for the same host wins: it carries the real port
		// and any identity pin the operator set.
		add(n.configuredOrNew(address))
	}
	for _, hub := range n.hubs {
		add(hub)
	}
	return ordered
}

func (n *Node) configuredOrNew(address string) lighthouseRef {
	wanted := hostOf(address)
	for _, hub := range n.hubs {
		if hostOf(hub.address) == wanted {
			return hub
		}
	}
	return lighthouseRef{address: address}
}

// openCircuit asks a lighthouse to join us to nodeID and returns the raw
// relayed connection plus whatever direct addresses the target advertised.
func (n *Node) openCircuit(ctx context.Context, hub lighthouseRef, nodeID string) (net.Conn, []string, error) {
	conn, err := n.dialLighthouse(ctx, hub)
	if err != nil {
		return nil, nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(n.cfg.HandshakeTimeout)); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if err := writeFrame(conn, frame{Op: opOpen, To: nodeID}); err != nil {
		conn.Close()
		return nil, nil, err
	}
	reply, err := readFrame(newFrameReader(conn))
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("mesh: open circuit to %s: %w", nodeID, err)
	}
	if reply.Op != opOK {
		conn.Close()
		return nil, nil, fmt.Errorf("mesh: lighthouse %s refused circuit to %s: %s", hub.address, nodeID, reply.Error)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, reply.Direct, nil
}

// dialDirect tries the peer's advertised addresses. Failure is the normal
// case for a node behind NAT and is never worth logging above debug.
func (n *Node) dialDirect(ctx context.Context, nodeID string, addresses []string, release func()) (net.Conn, error) {
	if len(addresses) == 0 {
		return nil, errors.New("mesh: no direct addresses")
	}
	dialer := &net.Dialer{
		Timeout: n.cfg.DirectTimeout,
		Control: n.directDialGuard(),
	}
	for _, address := range addresses {
		raw, err := dialer.DialContext(ctx, "tcp", withDefaultPort(address))
		if err != nil {
			slog.Debug("mesh direct dial failed", "peer", nodeID, "addr", address, "error", err)
			continue
		}
		var peer string
		conn := tls.Client(raw, n.id.ClientConfig(nodeID, &peer))
		hsCtx, cancel := context.WithTimeout(ctx, n.cfg.DirectTimeout)
		err = handshake(hsCtx, conn, n.cfg.DirectTimeout)
		cancel()
		if err != nil {
			raw.Close()
			slog.Debug("mesh direct handshake failed", "peer", nodeID, "addr", address, "error", err)
			continue
		}
		if err := conn.SetDeadline(time.Time{}); err != nil {
			conn.Close()
			continue
		}
		slog.Debug("mesh circuit went direct", "peer", nodeID, "addr", address)
		return &peerConn{Conn: conn, peer: Addr{NodeID: nodeID}, local: Addr{NodeID: n.id.NodeID}, direct: true, release: release}, nil
	}
	return nil, errors.New("mesh: no direct address answered")
}

// directDialGuard keeps a peer from steering direct connections at hosts on
// the operator's own network. It reuses the same public-IP policy as the
// federation SSRF guard, and is only relaxed when the operator opts in for a
// LAN mesh.
func (n *Node) directDialGuard() func(network, address string, c syscall.RawConn) error {
	if n.cfg.AllowPrivateDirect {
		return nil
	}
	return publicDialGuard("direct")
}

func publicDialGuard(kind string) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("mesh: %s address %s: %w", kind, address, err)
		}
		if !netutil.IsPublicIP(net.ParseIP(host)) {
			return fmt.Errorf("mesh: refusing %s connection to non-public address %s", kind, host)
		}
		return nil
	}
}

// acquire takes a circuit slot without blocking: at the limit, refusing now is
// better than queueing work that the peer's own timeout will discard anyway.
func (n *Node) acquire(_ context.Context) (func(), bool) {
	select {
	case n.slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-n.slots }) }, true
	default:
		return nil, false
	}
}

func (n *Node) setConnected(address string, up bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.connected[address] = up
}

func (n *Node) cachedDirect(nodeID string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.directCache[nodeID]
}

func (n *Node) cachedLighthouse(nodeID string) (lighthouseRef, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	hub, ok := n.lighthouseCache[nodeID]
	return hub, ok
}

func (n *Node) rememberLighthouse(nodeID string, hub lighthouseRef) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lighthouseCache[nodeID] = hub
}

func (n *Node) rememberDirect(nodeID string, addresses []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(addresses) == 0 {
		delete(n.directCache, nodeID)
		return
	}
	n.directCache[nodeID] = addresses
}

// handshake performs a TLS handshake bounded by ctx.
func handshake(ctx context.Context, conn *tls.Conn, timeout time.Duration) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return conn.HandshakeContext(ctx)
}

func errOrFrame(err error, f frame) error {
	if err != nil {
		return err
	}
	if f.Error != "" {
		return errors.New(f.Error)
	}
	return fmt.Errorf("unexpected %s frame", f.Op)
}

// peerConn presents a circuit as an ordinary connection whose remote address
// identifies the peer node.
type peerConn struct {
	net.Conn
	peer      Addr
	local     Addr
	direct    bool
	release   func()
	closeOnce sync.Once
}

func (c *peerConn) RemoteAddr() net.Addr { return c.peer }
func (c *peerConn) LocalAddr() net.Addr  { return c.local }

func (c *peerConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(func() {
		if c.release != nil {
			c.release()
		}
	})
	return err
}

// PeerOf returns the node ID behind a connection produced by this package.
func PeerOf(conn net.Conn) (string, bool) {
	addr, ok := conn.RemoteAddr().(Addr)
	return addr.NodeID, ok
}

// IsDirect reports whether a circuit reached the peer without a relay, which
// is what `littlefedi mesh ping` uses to tell the two paths apart.
func IsDirect(conn net.Conn) bool {
	circuit, ok := conn.(*peerConn)
	return ok && circuit.direct
}
