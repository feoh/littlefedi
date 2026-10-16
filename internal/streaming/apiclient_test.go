package streaming

import "testing"

// Notifications of littleFedi's own types reach the web UI's stream but not
// Mastodon API clients, which cannot show them.
func TestAPIClientsSkipExtensionNotificationTypes(t *testing.T) {
	hub := NewHub()
	web := newConn(hub, "alice")
	app := newConn(hub, "alice")
	app.apiClient = true
	for _, c := range []*Conn{web, app} {
		hub.register(c)
		c.Subscribe(StreamUser)
		defer c.Close()
	}
	received := func(c *Conn) int {
		n := 0
		for {
			select {
			case <-c.Out():
				n++
			default:
				return n
			}
		}
	}

	accept := makeEnvelope([]string{StreamUser}, "notification", `{"type":"follow_accept"}`)
	accept.notificationType = "follow_accept"
	hub.sendToAccount("alice", StreamUser, accept)
	if received(web) != 1 || received(app) != 0 {
		t.Error("follow_accept should reach the web UI only")
	}

	follow := makeEnvelope([]string{StreamUser}, "notification", `{"type":"follow"}`)
	follow.notificationType = "follow"
	hub.sendToAccount("alice", StreamUser, follow)
	if received(web) != 1 || received(app) != 1 {
		t.Error("follow should reach both")
	}
}
