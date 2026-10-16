package netutil

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsPublicIP(t *testing.T) {
	public := []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"}
	private := []string{
		"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.5.4", "100.64.0.1",
		"169.254.1.1", "0.0.0.0", "192.0.2.1", "203.0.113.9", "255.255.255.255",
		"::1", "fe80::1", "fc00::1", "2001:db8::1", "64:ff9b::808:808",
	}
	for _, s := range public {
		if !IsPublicIP(net.ParseIP(s)) {
			t.Errorf("IsPublicIP(%s) = false, want true", s)
		}
	}
	for _, s := range private {
		if IsPublicIP(net.ParseIP(s)) {
			t.Errorf("IsPublicIP(%s) = true, want false", s)
		}
	}
	if IsPublicIP(nil) {
		t.Error("IsPublicIP(nil) = true")
	}
}

func TestWithIPv4FallbackRoundTrip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	client := &http.Client{Transport: WithIPv4Fallback(http.DefaultTransport.(*http.Transport).Clone(), &net.Dialer{})}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("GET through the fallback transport: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("got %d %q", resp.StatusCode, body)
	}
}
