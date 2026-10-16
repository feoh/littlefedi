package mesh

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIdentityRoundTrip(t *testing.T) {
	id, err := NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	if !ValidNodeID(id.NodeID) {
		t.Fatalf("generated node id %q is not valid", id.NodeID)
	}
	pemText, err := id.PrivateKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := IdentityFromPEM(pemText)
	if err != nil {
		t.Fatalf("IdentityFromPEM: %v", err)
	}
	if loaded.NodeID != id.NodeID {
		t.Errorf("reloaded identity has node id %q, want %q", loaded.NodeID, id.NodeID)
	}

	pub, err := PublicKeyFromNodeID(id.NodeID)
	if err != nil {
		t.Fatalf("PublicKeyFromNodeID: %v", err)
	}
	if NodeIDFromPublicKey(pub) != id.NodeID {
		t.Error("node id does not round-trip through its public key")
	}
	if _, err := IdentityFromPEM("not pem"); err == nil {
		t.Error("garbage PEM accepted")
	}
}

func TestDomains(t *testing.T) {
	id, _ := NewIdentity()
	host := id.NodeID + ".mesh.lighthouse.example"
	nodeID, suffix, ok := SplitDomain(strings.ToUpper(host) + ".")
	if !ok || nodeID != id.NodeID || suffix != "mesh.lighthouse.example" {
		t.Fatalf("SplitDomain = %q %q %v", nodeID, suffix, ok)
	}
	if !ValidSuffix(suffix) || ValidSuffix("example.com") {
		t.Error("ValidSuffix misclassifies")
	}
	if _, _, ok := SplitDomain("www.example.com"); ok {
		t.Error("an ordinary domain was treated as a mesh domain")
	}

	if got := SuffixForLighthouse("lighthouse.example:7946"); got != "mesh.lighthouse.example" {
		t.Errorf("SuffixForLighthouse = %q", got)
	}
	if got := LighthouseForSuffix("mesh.lighthouse.example"); got != "lighthouse.example:"+DefaultLighthousePort {
		t.Errorf("LighthouseForSuffix = %q", got)
	}
	if u, err := GatewayURL(host); err != nil || u == nil {
		t.Errorf("GatewayURL(%q) = %v, %v", host, u, err)
	}

	a, b := SyntheticIP(id.NodeID), SyntheticIP(id.NodeID)
	if !a.Equal(b) || a.To4() != nil {
		t.Errorf("SyntheticIP should be a stable IPv6 address: %v %v", a, b)
	}
}

func TestDirectoryRecordSignAndVerify(t *testing.T) {
	id, _ := NewIdentity()
	payload := DirectoryPayload{
		Version: DirectoryRecordVersion, NodeID: id.NodeID, Alias: "alice", Sequence: 1,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
	record, err := SignDirectoryRecord(id, payload)
	if err != nil {
		t.Fatalf("SignDirectoryRecord: %v", err)
	}
	got, err := VerifyDirectoryRecord(record, time.Now())
	if err != nil {
		t.Fatalf("VerifyDirectoryRecord: %v", err)
	}
	if got.NodeID != id.NodeID || got.Alias != "alice" {
		t.Errorf("verified payload = %+v", got)
	}

	tampered := record
	tampered.Payload = []byte(strings.Replace(string(record.Payload), "alice", "mallory", 1))
	if _, err := VerifyDirectoryRecord(tampered, time.Now()); err == nil {
		t.Error("a tampered record verified")
	}
	if _, err := VerifyDirectoryRecord(record, time.Now().Add(2*time.Hour)); err == nil {
		t.Error("an expired record verified")
	}
}

func TestDirectoryPublishAndResolve(t *testing.T) {
	server := httptest.NewServer(NewDirectory())
	defer server.Close()
	client := NewDirectoryClient(server.URL, server.Client())
	defer client.Close()

	id, _ := NewIdentity()
	ctx := context.Background()
	if _, err := client.Publish(ctx, id, "alice", time.Hour, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	byAlias, err := client.ResolveAlias(ctx, "alice")
	if err != nil || byAlias.NodeID != id.NodeID {
		t.Fatalf("ResolveAlias = %+v, %v", byAlias, err)
	}
	byNode, err := client.ResolveNode(ctx, id.NodeID)
	if err != nil || byNode.Alias != "alice" {
		t.Fatalf("ResolveNode = %+v, %v", byNode, err)
	}

	other, _ := NewIdentity()
	if _, err := client.Publish(ctx, other, "alice", time.Hour, nil); err == nil {
		t.Error("a second node took over an existing alias")
	}
}
