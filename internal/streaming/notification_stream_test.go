package streaming

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

type fakeRouterStore struct{ account *models.Account }

func (f fakeRouterStore) IsFollowing(context.Context, string, string) (bool, error) {
	return false, nil
}
func (f fakeRouterStore) GetAccount(context.Context, string) (*models.Account, error) {
	return f.account, nil
}
func (f fakeRouterStore) GetMentionsForStatus(context.Context, string) ([]*models.Mention, error) {
	return nil, nil
}

type fakeSerializer struct{}

func (fakeSerializer) SerializeStatus(*models.Account, *models.Status) ([]byte, error) {
	return []byte(`{"id":"s"}`), nil
}
func (fakeSerializer) SerializeNotification(_ *models.Account, n *models.Notification) ([]byte, error) {
	return json.Marshal(map[string]string{"id": n.ID, "type": n.Type})
}

func drain(c *Conn) []envelope {
	var got []envelope
	for {
		select {
		case env := <-c.Out():
			got = append(got, env)
		default:
			return got
		}
	}
}

// Mastodon's user:notification stream carries nothing but notifications.
// Tuba treats every event on it as a notification, so home timeline posts
// sent there showed up as "Unknown notification type: (null)".
func TestUserNotificationStreamCarriesOnlyNotifications(t *testing.T) {
	desc, ok := descriptorFor("user:notification", httptest.NewRequest("GET", "/", nil))
	if !ok || desc.key != StreamNotification {
		t.Fatalf("user:notification maps to %q", desc.key)
	}
	if msgDesc, ok := descriptorForClientMessage(clientMessage{Stream: "user:notification"}); !ok || msgDesc.key != StreamNotification {
		t.Fatalf("subscribe message maps to %q", msgDesc.key)
	}

	hub := NewHub()
	user := newConn(hub, "alice")
	notifications := newConn(hub, "alice")
	for c, key := range map[*Conn]string{user: StreamUser, notifications: StreamNotification} {
		hub.register(c)
		c.Subscribe(key)
		defer c.Close()
	}

	hub.sendToAccount("alice", StreamUser, makeEnvelope([]string{StreamUser}, "update", `{"id":"s"}`))
	if got := drain(notifications); len(got) != 0 {
		t.Errorf("a home timeline update reached the notification stream: %v", got)
	}
	if got := drain(user); len(got) != 1 {
		t.Errorf("user stream got %d updates, want 1", len(got))
	}

	router := NewEventRouter(hub, fakeRouterStore{account: &models.Account{ID: "alice"}}, fakeSerializer{})
	router.NotificationCreated(context.Background(), &models.Notification{ID: "n1", Type: "follow", AccountID: "alice"})
	for name, c := range map[string]*Conn{"user": user, "user:notification": notifications} {
		got := drain(c)
		if len(got) != 1 || got[0].event != "notification" {
			t.Errorf("%s stream got %+v, want one notification", name, got)
		}
	}
}
