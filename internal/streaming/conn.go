package streaming

import "sync"

// sendBuffer is the per-connection queue depth. A client that falls this far
// behind is dropped instead of being allowed to grow memory unbounded - the
// backpressure rule that keeps the feature safe on low-power hosts.
const sendBuffer = 64

// Conn is one streaming client connection (WebSocket or SSE). It is transport
// agnostic: the transport drains Out() and writes to the wire, and calls Close()
// when the socket goes away.
type Conn struct {
	hub       *Hub
	accountID string // "" for an anonymous (public-only) connection

	mu   sync.Mutex
	keys map[string]bool // subscribed stream keys

	send      chan envelope
	closeOnce sync.Once
	done      chan struct{}
}

// newConn creates a connection bound to hub for accountID ("" = anonymous).
func newConn(hub *Hub, accountID string) *Conn {
	return &Conn{
		hub:       hub,
		accountID: accountID,
		keys:      make(map[string]bool),
		send:      make(chan envelope, sendBuffer),
		done:      make(chan struct{}),
	}
}

// Out is the channel the transport reads rendered messages from.
func (c *Conn) Out() <-chan envelope { return c.send }

// Done is closed when the connection is being torn down.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Subscribe adds a stream key (e.g. "user", "public:local", "hashtag:catpics").
func (c *Conn) Subscribe(key string) {
	if key == "" {
		return
	}
	c.hub.subscribe(c, key)
}

// Unsubscribe removes a stream key.
func (c *Conn) Unsubscribe(key string) {
	if key == "" {
		return
	}
	c.hub.unsubscribe(c, key)
}

// enqueue queues env without blocking. If the connection's buffer is full the
// client is too slow: drop it rather than block the producer or grow memory.
func (c *Conn) enqueue(env envelope) {
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.send <- env:
	default:
		// Backpressure: slow consumer, drop the connection.
		c.Close()
	}
}

// Close tears down the connection exactly once, removing it from the hub.
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		c.hub.unregister(c)
		close(c.done)
	})
}
