package netutil

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ipv6BrokenTTL is how long a host whose dual-stack request timed out keeps
// being reached over IPv4 before the normal path is tried again.
const ipv6BrokenTTL = 10 * time.Minute

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

// dialError marks a failure that happened while establishing the connection,
// where the IPv4 retry below has already run. Anything else failed on a
// connection that was already up, and may still be worth another attempt.
type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// WithIPv4Fallback installs dialer on base and returns the RoundTripper to give
// to an http.Client. It covers the two ways a broken IPv6 path breaks a
// request: the connect that never completes, and the connect that succeeds onto
// a path which then swallows the TLS handshake. Only the first is Happy
// Eyeballs' problem, and a CDN in the second state (Azure Front Door does this
// from some networks) fails every single request to that host until the
// condition clears.
//
// base is modified in place, so call this while building it.
func WithIPv4Fallback(base *http.Transport, dialer *net.Dialer) http.RoundTripper {
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialWithIPv4Fallback(ctx, network, address, dialer.DialContext)
	}
	if base.TLSHandshakeTimeout == 0 {
		// Zero means the handshake waits forever, so the client timeout would
		// be the one to fire, and by then the context is dead and there is no
		// second attempt left to make.
		base.TLSHandshakeTimeout = dialer.Timeout
	}
	ipv4 := base.Clone()
	ipv4.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, "tcp4", address)
		if err != nil {
			return nil, &dialError{err}
		}
		return conn, nil
	}
	return &ipv4Fallback{dual: base, ipv4: ipv4, broken: make(map[string]time.Time)}
}

// dialWithIPv4Fallback first lets net.Dialer use its normal dual-stack Happy
// Eyeballs behavior. Some resolvers and network configurations still produce a
// failed IPv6-only attempt, so retry explicitly on IPv4 before giving up.
func dialWithIPv4Fallback(ctx context.Context, network, address string, dial dialContextFunc) (net.Conn, error) {
	conn, err := dial(ctx, network, address)
	if err == nil {
		return conn, nil
	}
	if ctx.Err() != nil || network == "tcp4" || !strings.HasPrefix(network, "tcp") {
		return conn, &dialError{err}
	}

	conn, ipv4Err := dial(ctx, "tcp4", address)
	if ipv4Err == nil {
		return conn, nil
	}
	return nil, &dialError{fmt.Errorf("dual-stack dial failed (%v); IPv4 fallback failed: %w", err, ipv4Err)}
}

// ipv4Fallback retries over IPv4 when a request times out on a connection that
// was already established, and remembers the host so the ones behind it on the
// same page do not each pay the timeout again.
type ipv4Fallback struct {
	dual, ipv4 http.RoundTripper

	mu     sync.Mutex
	broken map[string]time.Time
}

func (t *ipv4Fallback) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	if t.ipv6Broken(host) {
		return t.ipv4.RoundTrip(req)
	}

	resp, err := t.dual.RoundTrip(req)
	if err == nil || req.Context().Err() != nil {
		return resp, err
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() || errors.As(err, new(*dialError)) {
		return resp, err
	}
	retry, replayErr := replayable(req)
	if replayErr != nil {
		return resp, err
	}

	slog.Debug("net: dual-stack request timed out, retrying over IPv4", "host", host, "error", err)
	retryResp, retryErr := t.ipv4.RoundTrip(retry)
	if retryErr != nil {
		return retryResp, retryErr
	}
	// Only a retry that came back proves the address family was the problem.
	// A timeout that had nothing to do with IPv6 would otherwise pin the host
	// to IPv4 for the next ten minutes on its own.
	t.markIPv6Broken(host)
	return retryResp, nil
}

// replayable returns a copy of req that can be sent a second time. A delivery
// or a push notification carries a body the first attempt has already read, so
// it comes back from GetBody, which http.NewRequest fills in for the in-memory
// readers both of them are built from.
func replayable(req *http.Request) (*http.Request, error) {
	if req.Body == nil {
		return req, nil
	}
	if req.GetBody == nil {
		return nil, errors.New("body cannot be rewound")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	retry := req.Clone(req.Context())
	retry.Body = body
	return retry, nil
}

func (t *ipv4Fallback) ipv6Broken(host string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	until, ok := t.broken[host]
	if ok && time.Now().Before(until) {
		return true
	}
	delete(t.broken, host)
	return false
}

func (t *ipv4Fallback) markIPv6Broken(host string) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for h, until := range t.broken {
		if now.After(until) {
			delete(t.broken, h)
		}
	}
	t.broken[host] = now.Add(ipv6BrokenTTL)
}
