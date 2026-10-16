package media

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mesh"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/netutil"
)

func newRemoteMediaClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:       15 * time.Second,
		KeepAlive:     30 * time.Second,
		FallbackDelay: 300 * time.Millisecond,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || isPrivateOrReservedIP(ip) {
				return fmt.Errorf("refusing remote media connection to non-public address %q", host)
			}
			return nil
		},
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A proxy resolves the destination itself, bypassing the dial guard.
	transport.Proxy = nil

	return &http.Client{
		Timeout: 60 * time.Second,
		// Avatars and attachments belonging to mesh peers are fetched over a
		// circuit; everything else keeps the guarded dialer above.
		Transport: mesh.WrapDefault(netutil.WithIPv4Fallback(transport, dialer)),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return validateRemoteURL(req.URL.String())
		},
	}
}
