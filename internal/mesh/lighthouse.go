package mesh

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// LighthouseConfig configures a relay node.
type LighthouseConfig struct {
	// Listen is the control and relay address, e.g. ":7842".
	Listen string
	// Allowlist, when non-empty, restricts registration and circuit opening to
	// these node IDs, turning an open lighthouse into a private one.
	Allowlist []string
	// MaxBytesPerNodePerHour caps relayed traffic charged to a node. Zero
	// means unlimited.
	MaxBytesPerNodePerHour int64
	// MaxCircuitsPerNode caps concurrent circuits a single node may hold open.
	MaxCircuitsPerNode int
	// MaxConnections caps all accepted connections, including handshakes,
	// controls, and half-open circuits.
	MaxConnections int
	// MaxConnectionsPerIP prevents identity rotation from bypassing every
	// node-ID-based limit on a public relay.
	MaxConnectionsPerIP int
	// HandshakeTimeout bounds every pre-payload exchange with a node.
	HandshakeTimeout time.Duration
	// ControlReadTimeout is how long a registered node may stay silent before
	// its control connection is dropped. It must exceed the nodes' ping
	// interval by a comfortable margin.
	ControlReadTimeout time.Duration
}

func (c *LighthouseConfig) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":" + DefaultLighthousePort
	}
	if c.MaxCircuitsPerNode <= 0 {
		c.MaxCircuitsPerNode = 64
	}
	if c.MaxConnections <= 0 {
		c.MaxConnections = 4096
	}
	if c.MaxConnectionsPerIP <= 0 {
		c.MaxConnectionsPerIP = 128
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = defaultHandshakeTimeout
	}
	if c.ControlReadTimeout <= 0 {
		c.ControlReadTimeout = defaultControlReadTimeout
	}
}

// Lighthouse is a publicly reachable relay. It learns which nodes are online,
// introduces them to each other, and copies bytes between them. It never sees
// plaintext: the payload of every circuit is a second TLS session pinned to
// the two endpoints' node IDs, negotiated after the splice.
type Lighthouse struct {
	id    *Identity
	cfg   LighthouseConfig
	allow map[string]bool

	connections chan struct{}

	mu             sync.Mutex
	nodes          map[string]*registration
	pending        map[string]*pending
	usage          map[string]*quota
	active         map[string]int
	ipConnections  map[string]int
	nextQuotaPrune time.Time
}

type registration struct {
	conn         net.Conn
	direct       []string
	writeMu      sync.Mutex
	since        time.Time
	writeTimeout time.Duration
}

func (r *registration) send(f frame) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	return sendFrame(r.conn, f, r.writeTimeout)
}

// pending is a circuit whose initiator is waiting for the target to call back.
type pending struct {
	target string
	ready  chan net.Conn
	done   chan struct{}
}

type quota struct {
	windowStart time.Time
	bytes       int64
}

type countedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *countedConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		c.release()
	})
	return err
}

// NewLighthouse creates a relay. Call ListenAndServe to run it.
func NewLighthouse(id *Identity, cfg LighthouseConfig) (*Lighthouse, error) {
	if id == nil {
		return nil, errors.New("mesh: lighthouse needs an identity")
	}
	cfg.applyDefaults()
	allow := make(map[string]bool, len(cfg.Allowlist))
	for _, nodeID := range cfg.Allowlist {
		nodeID = strings.ToLower(strings.TrimSpace(nodeID))
		if !ValidNodeID(nodeID) {
			return nil, fmt.Errorf("mesh: allowlist entry %q is not a node id", nodeID)
		}
		allow[nodeID] = true
	}
	if len(allow) > 0 {
		allow[id.NodeID] = true
	}
	return &Lighthouse{
		id:            id,
		cfg:           cfg,
		allow:         allow,
		connections:   make(chan struct{}, cfg.MaxConnections),
		nodes:         make(map[string]*registration),
		pending:       make(map[string]*pending),
		usage:         make(map[string]*quota),
		active:        make(map[string]int),
		ipConnections: make(map[string]int),
	}, nil
}

// ListenAndServe runs the lighthouse until ctx is cancelled.
func (l *Lighthouse) ListenAndServe(ctx context.Context) error {
	listener, err := net.Listen("tcp", l.cfg.Listen)
	if err != nil {
		return fmt.Errorf("mesh: lighthouse listen: %w", err)
	}
	return l.Serve(ctx, listener)
}

// Serve runs the lighthouse on an existing listener.
func (l *Lighthouse) Serve(ctx context.Context, listener net.Listener) error {
	defer listener.Close()
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	slog.Info("mesh lighthouse listening",
		"addr", listener.Addr().String(),
		"node_id", l.id.NodeID,
		"allowlist", len(l.allow))

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("mesh: lighthouse accept: %w", err)
		}
		host, _, splitErr := net.SplitHostPort(conn.RemoteAddr().String())
		if splitErr != nil {
			host = conn.RemoteAddr().String()
		}
		l.mu.Lock()
		ipLimited := l.ipConnections[host] >= l.cfg.MaxConnectionsPerIP
		if !ipLimited {
			l.ipConnections[host]++
		}
		l.mu.Unlock()
		if ipLimited {
			conn.Close()
			slog.Warn("mesh lighthouse per-IP connection limit reached", "remote", conn.RemoteAddr().String())
			continue
		}
		releaseIP := func() {
			l.mu.Lock()
			l.ipConnections[host]--
			if l.ipConnections[host] <= 0 {
				delete(l.ipConnections, host)
			}
			l.mu.Unlock()
		}
		select {
		case l.connections <- struct{}{}:
			go l.handle(ctx, &countedConn{Conn: conn, release: func() {
				<-l.connections
				releaseIP()
			}})
		default:
			releaseIP()
			conn.Close()
			slog.Warn("mesh lighthouse connection limit reached", "remote", conn.RemoteAddr().String())
		}
	}
}

// NodeID reports the lighthouse's own node ID, which peers may pin.
func (l *Lighthouse) NodeID() string { return l.id.NodeID }

// Nodes lists the currently registered node IDs and how long they have been up.
func (l *Lighthouse) Nodes() map[string]time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]time.Time, len(l.nodes))
	for nodeID, reg := range l.nodes {
		out[nodeID] = reg.since
	}
	return out
}

func (l *Lighthouse) handle(ctx context.Context, raw net.Conn) {
	var peer string
	conn := tls.Server(raw, l.id.ServerConfig("", &peer))
	if err := handshake(ctx, conn, l.cfg.HandshakeTimeout); err != nil {
		raw.Close()
		slog.Debug("mesh lighthouse handshake failed", "remote", raw.RemoteAddr().String(), "error", err)
		return
	}

	keep := false
	defer func() {
		if !keep {
			conn.Close()
		}
	}()

	if len(l.allow) > 0 && !l.allow[peer] {
		_ = writeFrame(conn, frame{Op: opError, Error: "node not allowed on this lighthouse"})
		slog.Warn("mesh lighthouse rejected node", "node_id", peer, "remote", raw.RemoteAddr().String())
		return
	}

	if err := conn.SetDeadline(time.Now().Add(l.cfg.HandshakeTimeout)); err != nil {
		return
	}
	reader := newFrameReader(conn)
	f, err := readFrame(reader)
	if err != nil {
		slog.Debug("mesh lighthouse read failed", "node_id", peer, "error", err)
		return
	}

	switch f.Op {
	case opRegister:
		l.serveControl(ctx, peer, conn, reader, f)
	case opOpen:
		l.serveOpen(ctx, peer, conn, f)
	case opAttach:
		keep = l.serveAttach(peer, conn, f)
	default:
		_ = writeFrame(conn, frame{Op: opError, Error: "expected register, open, or attach"})
	}
}

// serveControl owns a node's long-lived control connection.
func (l *Lighthouse) serveControl(ctx context.Context, peer string, conn net.Conn, reader *bufio.Reader, f frame) {
	reg := &registration{conn: conn, direct: f.Direct, since: time.Now(), writeTimeout: l.cfg.HandshakeTimeout}

	// Publish and acknowledge as one ordered operation: open requests may find
	// the registration now, but cannot write an accept before this ACK.
	reg.writeMu.Lock()
	l.mu.Lock()
	previous := l.nodes[peer]
	l.nodes[peer] = reg
	l.mu.Unlock()
	if err := writeFrame(conn, frame{Op: opOK}); err != nil {
		l.mu.Lock()
		if l.nodes[peer] == reg {
			delete(l.nodes, peer)
		}
		l.mu.Unlock()
		reg.writeMu.Unlock()
		return
	}
	reg.writeMu.Unlock()
	if previous != nil {
		// A node that reconnected before its old connection timed out would
		// otherwise keep receiving accept notices on a socket nobody reads.
		previous.conn.Close()
	}

	defer func() {
		l.mu.Lock()
		if l.nodes[peer] == reg {
			delete(l.nodes, peer)
		}
		l.mu.Unlock()
		slog.Info("mesh node left", "node_id", peer)
	}()

	slog.Info("mesh node registered", "node_id", peer, "direct", len(f.Direct), "remote", conn.RemoteAddr().String())

	for {
		if err := conn.SetReadDeadline(time.Now().Add(l.cfg.ControlReadTimeout)); err != nil {
			return
		}
		next, err := readFrame(reader)
		if err != nil {
			return
		}
		switch next.Op {
		case opPing:
			if err := reg.send(frame{Op: opPong}); err != nil {
				return
			}
		case opPong:
		default:
			// Control connections carry nothing else; a node that sends
			// otherwise is confused or malicious.
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// serveOpen introduces a caller to a registered node and splices them.
func (l *Lighthouse) serveOpen(ctx context.Context, peer string, conn net.Conn, f frame) {
	if !ValidNodeID(f.To) {
		_ = writeFrame(conn, frame{Op: opError, Error: "open needs a target node id"})
		return
	}
	if f.To == peer {
		_ = writeFrame(conn, frame{Op: opError, Error: "cannot open a circuit to self"})
		return
	}

	l.mu.Lock()
	target := l.nodes[f.To]
	limitReached := target != nil &&
		(l.active[peer] >= l.cfg.MaxCircuitsPerNode || l.active[f.To] >= l.cfg.MaxCircuitsPerNode)
	if target != nil && !limitReached {
		l.active[peer]++
		l.active[f.To]++
	}
	l.mu.Unlock()

	if target == nil {
		_ = writeFrame(conn, frame{Op: opError, Error: "target node is not registered here"})
		return
	}
	if limitReached {
		_ = writeFrame(conn, frame{Op: opError, Error: "circuit limit reached"})
		return
	}
	defer l.endCircuit(peer, f.To)
	if !l.withinQuota(peer) {
		_ = writeFrame(conn, frame{Op: opError, Error: "relay quota exhausted"})
		return
	}

	circuit, err := newCircuitID()
	if err != nil {
		_ = writeFrame(conn, frame{Op: opError, Error: "internal error"})
		return
	}
	waiter := &pending{target: f.To, ready: make(chan net.Conn), done: make(chan struct{})}

	l.mu.Lock()
	l.pending[circuit] = waiter
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.pending, circuit)
		l.mu.Unlock()
		close(waiter.done)
	}()

	if err := target.send(frame{Op: opAccept, Circuit: circuit, From: peer}); err != nil {
		_ = writeFrame(conn, frame{Op: opError, Error: "target node is unreachable"})
		return
	}

	// The caller is told the target's advertised addresses now rather than
	// after the splice, so it can try a direct connection while the relay is
	// still being set up and skip this lighthouse for the rest of the session.
	if err := writeFrame(conn, frame{Op: opOK, Circuit: circuit, Direct: target.direct}); err != nil {
		return
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return
	}

	timer := time.NewTimer(attachTimeout)
	defer timer.Stop()
	select {
	case other := <-waiter.ready:
		l.splice(peer, f.To, conn, other)
	case <-timer.C:
		slog.Warn("mesh circuit was never attached", "circuit", circuit, "from", peer, "to", f.To)
	case <-ctx.Done():
	}
}

// serveAttach hands a target's callback connection to the waiting caller. It
// returns true when ownership of conn passed to the splice.
func (l *Lighthouse) serveAttach(peer string, conn net.Conn, f frame) bool {
	l.mu.Lock()
	waiter := l.pending[f.Circuit]
	if waiter != nil && waiter.target == peer {
		delete(l.pending, f.Circuit)
	} else {
		waiter = nil
	}
	l.mu.Unlock()

	if waiter == nil {
		// Either the circuit expired or a node is trying to claim a circuit
		// meant for someone else.
		_ = writeFrame(conn, frame{Op: opError, Error: "unknown circuit"})
		return false
	}
	if err := writeFrame(conn, frame{Op: opOK}); err != nil {
		return false
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return false
	}
	select {
	case waiter.ready <- conn:
		return true
	case <-waiter.done:
		return false
	}
}

// splice copies bytes in both directions until either side stops. Both
// endpoints are charged for the traffic, so neither can use the other's budget.
func (l *Lighthouse) splice(a, b string, left, right net.Conn) {
	slog.Debug("mesh circuit spliced", "from", a, "to", b)

	done := make(chan struct{}, 2)
	copyOne := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		_, err := l.meteredCopy(dst, src, a, b)
		if err != nil && !errors.Is(err, io.EOF) {
			slog.Debug("mesh relay ended", "from", a, "to", b, "error", err)
		}
		// Half-close is meaningless here: once one direction stops the HTTP
		// session above is over, so tearing both down releases the relay.
		left.Close()
		right.Close()
	}
	go copyOne(left, right)
	go copyOne(right, left)
	<-done
	<-done
}

// meteredCopy is io.Copy with a byte budget, so a single circuit cannot spend
// a lighthouse's bandwidth without bound.
func (l *Lighthouse) meteredCopy(dst io.Writer, src io.Reader, chargeTo ...string) (int64, error) {
	if l.cfg.MaxBytesPerNodePerHour <= 0 {
		return io.Copy(dst, src)
	}
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			for _, nodeID := range chargeTo {
				if !l.charge(nodeID, int64(n)) {
					return total, fmt.Errorf("mesh: relay quota exhausted for %s", nodeID)
				}
			}
			written, writeErr := dst.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}

func (l *Lighthouse) endCircuit(nodes ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, nodeID := range nodes {
		if l.active[nodeID] <= 1 {
			delete(l.active, nodeID)
			continue
		}
		l.active[nodeID]--
	}
}

func (l *Lighthouse) withinQuota(nodeID string) bool {
	if l.cfg.MaxBytesPerNodePerHour <= 0 || nodeID == l.id.NodeID {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.quotaLocked(nodeID).bytes < l.cfg.MaxBytesPerNodePerHour
}

func (l *Lighthouse) charge(nodeID string, n int64) bool {
	if nodeID == l.id.NodeID {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	q := l.quotaLocked(nodeID)
	q.bytes += n
	return q.bytes <= l.cfg.MaxBytesPerNodePerHour
}

// quotaLocked returns a node's current hourly bucket, rolling the window over
// when it has expired.
//
// Fixed hourly windows, not a token bucket. A node can spend two
// budgets across a window boundary; swap in a rate limiter if that burst ever
// matters.
func (l *Lighthouse) quotaLocked(nodeID string) *quota {
	now := time.Now()
	if !now.Before(l.nextQuotaPrune) {
		for id, old := range l.usage {
			if now.Sub(old.windowStart) >= time.Hour {
				delete(l.usage, id)
			}
		}
		l.nextQuotaPrune = now.Add(time.Hour)
	}
	q := l.usage[nodeID]
	if q == nil {
		q = &quota{windowStart: now}
		l.usage[nodeID] = q
	}
	if now.Sub(q.windowStart) >= time.Hour {
		q.windowStart = now
		q.bytes = 0
	}
	return q
}

func newCircuitID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
