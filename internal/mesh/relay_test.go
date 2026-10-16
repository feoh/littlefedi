package mesh

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// Two nodes behind a lighthouse reach each other over HTTP through the relay,
// which is how a NATed littleFedi instance federates.
func TestNodesTalkThroughLighthouse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lighthouseID, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	lighthouse, err := NewLighthouse(lighthouseID, LighthouseConfig{})
	if err != nil {
		t.Fatalf("NewLighthouse: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = lighthouse.Serve(ctx, ln) }()
	pinned := ln.Addr().String() + "@" + lighthouseID.NodeID

	newNode := func() *Node {
		id, err := NewIdentity()
		if err != nil {
			t.Fatal(err)
		}
		node, err := New(id, Config{Suffix: "mesh.test", Lighthouses: []string{pinned}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := node.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = node.Close() })
		return node
	}
	server, client := newNode(), newNode()

	httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello from "+r.Host)
	})}
	go func() { _ = httpServer.Serve(server.Listener()) }()
	defer httpServer.Close()

	for len(lighthouse.Nodes()) < 2 {
		select {
		case <-ctx.Done():
			t.Fatalf("nodes never registered; lighthouse sees %v", lighthouse.Nodes())
		case <-time.After(20 * time.Millisecond):
		}
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+server.Domain()+"/hello", nil)
	resp, err := (&http.Client{Transport: client.Transport()}).Do(req)
	if err != nil {
		t.Fatalf("request over the mesh: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "hello from "+server.Domain() {
		t.Errorf("response = %d %q", resp.StatusCode, body)
	}
}
