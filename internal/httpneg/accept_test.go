package httpneg

import "testing"

func TestContentNegotiation(t *testing.T) {
	tests := []struct {
		accept   string
		wantAP   bool
		wantHTML bool
	}{
		{"application/activity+json", true, false},
		{`application/ld+json; profile="https://www.w3.org/ns/activitystreams"`, true, false},
		{"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", false, true},
		{"application/activity+json, text/html;q=0.5", true, false},
		{"text/html, application/activity+json;q=0.5", false, true},
		{"application/json", false, false},
		{"", false, false},
	}
	for _, tt := range tests {
		if got := ActivityPubPreferred(tt.accept); got != tt.wantAP {
			t.Errorf("ActivityPubPreferred(%q) = %v, want %v", tt.accept, got, tt.wantAP)
		}
		if got := HTMLPreferred(tt.accept); got != tt.wantHTML {
			t.Errorf("HTMLPreferred(%q) = %v, want %v", tt.accept, got, tt.wantHTML)
		}
	}
}

func TestLDJSONProfile(t *testing.T) {
	if !ActivityPubPreferred("application/ld+json") {
		t.Error("application/ld+json without a profile should count as ActivityPub")
	}
	if ActivityPubPreferred(`application/ld+json; profile="https://example.com/other"`) {
		t.Error("application/ld+json with another profile should not count as ActivityPub")
	}
}
