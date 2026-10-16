package emoji

import "testing"

func TestKnown(t *testing.T) {
	for _, e := range []string{"👍", "👍🏽", "❤️", "❤"} {
		if !Known(e) {
			t.Errorf("Known(%q) = false", e)
		}
	}
	for _, e := range []string{"", "abc", ":blobcat:"} {
		if Known(e) {
			t.Errorf("Known(%q) = true", e)
		}
	}
	if len(UnicodeMap) == 0 {
		t.Error("UnicodeMap is empty")
	}
}

func TestParseCustom(t *testing.T) {
	shortcode, domain, ok := ParseCustom(":blobcat@Remote.Example:")
	if !ok || shortcode != "blobcat" || domain != "remote.example" {
		t.Errorf("ParseCustom = %q %q %v", shortcode, domain, ok)
	}
	for _, bad := range []string{":blobcat:", "blobcat@remote.example", ":@remote.example:", ":a b@remote.example:", ":x@a/b:"} {
		if _, _, ok := ParseCustom(bad); ok {
			t.Errorf("ParseCustom(%q) accepted", bad)
		}
	}
}
