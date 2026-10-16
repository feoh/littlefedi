package mesh

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Control-frame operations. Frames are newline-delimited JSON and only ever
// appear before a circuit is spliced or on a node's long-lived control
// connection; once a circuit carries payload the lighthouse copies bytes and
// never parses them again.
const (
	// opRegister announces a node's presence on its control connection.
	opRegister = "register"
	// opOpen asks a lighthouse for a circuit to another node.
	opOpen = "open"
	// opAccept tells a registered node that someone wants a circuit to it.
	opAccept = "accept"
	// opAttach is the callback a node sends to claim a circuit it was told about.
	opAttach = "attach"
	// opOK acknowledges register, open, or attach.
	opOK = "ok"
	// opError reports a refusal; the connection closes afterwards.
	opError = "error"
	// opPing and opPong keep NAT bindings for the control connection alive.
	opPing = "ping"
	opPong = "pong"
)

const (
	// maxFrameBytes bounds a single control frame. Frames carry a handful of
	// short strings; anything larger is a peer misbehaving.
	maxFrameBytes = 8 << 10

	// attachTimeout bounds how long a lighthouse holds a half-open circuit
	// waiting for the target to call back.
	attachTimeout = 20 * time.Second

	// defaultHandshakeTimeout bounds every pre-payload exchange: TLS,
	// register, open and attach.
	defaultHandshakeTimeout = 15 * time.Second

	// defaultPingInterval is how often a node pings its lighthouse. NAT
	// mappings commonly expire after two minutes of silence.
	defaultPingInterval = 30 * time.Second

	// defaultControlReadTimeout must exceed the peer's ping interval by enough
	// that a slow link does not look like a dead peer.
	defaultControlReadTimeout = 3 * defaultPingInterval
)

// sendFrame writes a control frame under its own write deadline, then clears
// it. Control connections live for hours, so no deadline may be left in place
// to expire mid-session - a leftover handshake deadline once killed every
// control connection at its first keepalive.
func sendFrame(conn net.Conn, f frame, timeout time.Duration) error {
	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	return writeFrame(conn, f)
}

// frame is the control message envelope.
type frame struct {
	Op string `json:"op"`
	// Circuit identifies a pending or attaching circuit.
	Circuit string `json:"circuit,omitempty"`
	// To is the destination node ID of an open request.
	To string `json:"to,omitempty"`
	// From is the originating node ID reported to the target of a circuit.
	From string `json:"from,omitempty"`
	// Direct carries the addresses a node advertises for direct connections,
	// both on register (the node's own) and on the reply to open (the
	// target's, so the caller can try to skip the relay).
	Direct []string `json:"direct,omitempty"`
	// Error explains an opError frame.
	Error string `json:"error,omitempty"`
}

func writeFrame(w io.Writer, f frame) error {
	encoded, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("mesh: encode %s frame: %w", f.Op, err)
	}
	if len(encoded) > maxFrameBytes {
		return fmt.Errorf("mesh: %s frame is %d bytes, limit %d", f.Op, len(encoded), maxFrameBytes)
	}
	if _, err := w.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("mesh: write %s frame: %w", f.Op, err)
	}
	return nil
}

func readFrame(r *bufio.Reader) (frame, error) {
	line, err := r.ReadSlice('\n')
	if err != nil {
		if err == bufio.ErrBufferFull {
			return frame{}, fmt.Errorf("mesh: control frame exceeds %d bytes", maxFrameBytes)
		}
		return frame{}, err
	}
	var f frame
	if err := json.Unmarshal(line, &f); err != nil {
		return frame{}, fmt.Errorf("mesh: decode control frame: %w", err)
	}
	return f, nil
}

// newFrameReader returns a reader bounded to one frame's worth of buffer, so a
// peer cannot force unbounded buffering by withholding a newline.
func newFrameReader(conn net.Conn) *bufio.Reader {
	return bufio.NewReaderSize(conn, maxFrameBytes)
}

// SplitDomain divides a mesh domain into its node ID and the suffix that
// follows it. It returns ok=false for anything that is not a mesh name.
func SplitDomain(host string) (nodeID, suffix string, ok bool) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	label, rest, found := strings.Cut(host, ".")
	if !found || !ValidNodeID(label) {
		return "", "", false
	}
	return label, rest, true
}

// ValidSuffix reports whether suffix is a DNS-safe littleMesh suffix.
func ValidSuffix(suffix string) bool {
	suffix = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(suffix), "."))
	if suffix != "mesh" && !strings.HasPrefix(suffix, "mesh.") {
		return false
	}
	if len(suffix) > 253 {
		return false
	}
	for _, label := range strings.Split(suffix, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

// LighthouseForSuffix derives a lighthouse address from a mesh suffix. The
// naming convention "<node-id>.mesh.<lighthouse-host>" means the domain itself
// says where to ask for a circuit, which is why littleMesh needs no discovery
// protocol. A bare "mesh" suffix carries no host, so the caller must fall back
// to its own configured lighthouses.
func LighthouseForSuffix(suffix string) string {
	rest, found := strings.CutPrefix(strings.ToLower(suffix), "mesh.")
	if !found || rest == "" {
		return ""
	}
	return withDefaultPort(rest)
}

// SuffixForLighthouse is the inverse: the mesh suffix a node registered with
// the given lighthouse should use.
func SuffixForLighthouse(address string) string {
	host := address
	if h, _, err := net.SplitHostPort(address); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "" {
		return "mesh"
	}
	return "mesh." + host
}

func withDefaultPort(address string) string {
	if _, _, err := net.SplitHostPort(address); err == nil {
		return address
	}
	return net.JoinHostPort(address, DefaultLighthousePort)
}
