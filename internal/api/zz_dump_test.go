package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestDumpNotificationJSON(t *testing.T) {
	f := newFixture(t)
	var st map[string]interface{}
	f.call(t, "alice", http.MethodPost, "/api/v1/statuses", map[string]interface{}{"status": "hi"}, &st)
	f.call(t, "bob", http.MethodPost, "/api/v1/accounts/"+f.ids["alice"]+"/follow", map[string]interface{}{}, nil)
	f.call(t, "bob", http.MethodPost, "/api/v1/statuses/"+st["id"].(string)+"/favourite", map[string]interface{}{}, nil)
	f.call(t, "bob", http.MethodPost, "/api/v1/statuses", map[string]interface{}{"status": "@alice hello"}, nil)
	for _, path := range []string{"/api/v1/notifications", "/api/v2/notifications", "/api/v2/instance"} {
		var out interface{}
		code := f.call(t, "alice", http.MethodGet, path, nil, &out)
		b, _ := json.MarshalIndent(out, "", "  ")
		t.Logf("%s -> %d\n%s", path, code, b)
	}
}
