package mesh

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
)

type GatewayMode string

const (
	GatewayModeFederation GatewayMode = "federation"
	GatewayModeFull       GatewayMode = "full"
	GatewayModeAllowlist  GatewayMode = "allowlist"
)

type GatewayConfig struct {
	// Empty is the safe federation-only default.
	Mode       GatewayMode
	FullAccess []string
	// Directory lets a readable alias host route to its stable node ID.
	Directory *DirectoryClient
}

// Gateway turns a lighthouse into an HTTPS front door for the nodes it
// relays. Served behind a reverse proxy holding a wildcard certificate for
// "*.mesh.<lighthouse-host>", it makes a node with no address of its own
// visible to the ordinary fediverse: Mastodon resolves the name in DNS,
// arrives here, and the request is carried the rest of the way inside a
// circuit.
//
// This is the one place where the lighthouse is trusted. It terminates TLS for
// non-mesh callers, so its operator can read and tamper with that traffic.
// Mesh peers are unaffected - they reach the node directly over a circuit
// pinned to its key and never pass through here.
func Gateway(node *Node, cfg GatewayConfig) http.Handler {
	mode := cfg.Mode
	if mode == "" {
		mode = GatewayModeFederation
	}
	fullAccess := make(map[string]bool, len(cfg.FullAccess))
	for _, nodeID := range cfg.FullAccess {
		fullAccess[strings.ToLower(nodeID)] = true
	}
	proxy := &httputil.ReverseProxy{
		Transport: node.Transport(),
		Rewrite: func(r *httputil.ProxyRequest) {
			host := hostOf(r.In.Host)
			r.Out.URL.Scheme = "https"
			r.Out.URL.Host = host
			r.Out.Host = host
			// Rewrite strips forwarding headers before this callback. Restore
			// the chain supplied by the gateway's loopback-only reverse proxy.
			if forwarded := r.In.Header.Get("X-Forwarded-For"); forwarded != "" {
				r.Out.Header.Set("X-Forwarded-For", forwarded)
			} else if client, _, err := net.SplitHostPort(r.In.RemoteAddr); err == nil {
				r.Out.Header.Set("X-Forwarded-For", client)
			}
			r.Out.Header.Del("X-Forwarded-Host")
			r.Out.Header.Del("X-Forwarded-Proto")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "mesh node unreachable", http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostOf(r.Host)
		nodeID, suffix, ok := SplitDomain(host)
		targetHost := host
		if !ok || !strings.EqualFold(suffix, node.Suffix()) {
			if cfg.Directory == nil {
				http.Error(w, "not a mesh domain", http.StatusNotFound)
				return
			}
			payload, err := cfg.Directory.ResolveAlias(r.Context(), host)
			if err != nil {
				http.Error(w, "mesh alias not found", http.StatusNotFound)
				return
			}
			nodeID = payload.NodeID
			targetHost = nodeID + "." + node.Suffix()
		}
		if !RequestAllowed(mode, fullAccess[nodeID], r) {
			http.NotFound(w, r)
			return
		}
		request := r.Clone(r.Context())
		request.Host = targetHost
		proxy.ServeHTTP(w, request)
	})
}

// RequestAllowed applies the common exposure policy used by both the public
// gateway and an instance's direct mesh listener.
func RequestAllowed(mode GatewayMode, fullAccess bool, r *http.Request) bool {
	if mode == "" {
		mode = GatewayModeFederation
	}
	switch mode {
	case GatewayModeFull:
		return true
	case GatewayModeAllowlist:
		if fullAccess {
			return true
		}
	case GatewayModeFederation:
	default:
		return false
	}
	return federationGatewayRequest(r)
}

func federationGatewayRequest(r *http.Request) bool {
	escaped := strings.ToLower(r.URL.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") ||
		strings.Contains(r.URL.Path, "\\") || path.Clean(r.URL.Path) != r.URL.Path {
		return false
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		switch r.URL.Path {
		case "/.well-known/webfinger", "/.well-known/nodeinfo", "/.well-known/host-meta",
			"/nodeinfo/2.1", "/instance":
			return true
		}
		for _, prefix := range []string{"/users/", "/objects/", "/media/", "/emoji/"} {
			if strings.HasPrefix(r.URL.Path, prefix) {
				return true
			}
		}
		if strings.HasPrefix(r.URL.Path, "/@") {
			accept := strings.ToLower(r.Header.Get("Accept"))
			return strings.Contains(accept, "application/activity+json") ||
				strings.Contains(accept, "application/ld+json")
		}
	case http.MethodPost:
		return r.URL.Path == "/inbox" ||
			(strings.HasPrefix(r.URL.Path, "/users/") && strings.HasSuffix(r.URL.Path, "/inbox"))
	}
	return false
}

// GatewayURL is the public URL a gateway serves for a node, useful in
// diagnostics and documentation.
func GatewayURL(domain string) (*url.URL, error) {
	if _, _, ok := SplitDomain(domain); !ok {
		return nil, fmt.Errorf("mesh: %q is not a mesh domain", domain)
	}
	return &url.URL{Scheme: "https", Host: domain, Path: "/"}, nil
}
