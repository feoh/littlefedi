package mesh

import (
	"net"
)

// Listener returns the node's inbound circuits as a net.Listener, so the
// existing HTTP server serves the mesh with no separate code path:
//
//	go http.Serve(node.Listener(), router)
func (n *Node) Listener() net.Listener {
	return &listener{node: n}
}

type listener struct {
	node *Node
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.node.incoming:
		return conn, nil
	case <-l.node.closed:
		return nil, net.ErrClosed
	}
}

// Close stops the node, which unblocks Accept and lets http.Serve return.
func (l *listener) Close() error { return l.node.Close() }

func (l *listener) Addr() net.Addr { return Addr{NodeID: l.node.id.NodeID} }
