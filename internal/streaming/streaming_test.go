package streaming_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/streaming"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

type jsonSerializer struct{}

func (jsonSerializer) SerializeStatus(_ *models.Account, s *models.Status) ([]byte, error) {
	return json.Marshal(map[string]string{"id": s.ID, "content": s.ContentHTML})
}

func (jsonSerializer) SerializeNotification(_ *models.Account, n *models.Notification) ([]byte, error) {
	return json.Marshal(map[string]string{"id": n.ID, "type": n.Type})
}

func newServer(t *testing.T) (*httptest.Server, *streaming.Hub, *streaming.EventRouter, *models.Account) {
	t.Helper()
	store := testutil.NewStore(t)
	alice := testutil.CreateLocalAccount(t, store, "alice")
	hub := streaming.NewHub()
	r := chi.NewRouter()
	streaming.NewHandler(hub, store).RegisterRoutes(r)
	server := httptest.NewServer(r)
	// An SSE handler notices a departed client only when its next heartbeat
	// write fails, so server.Close, which waits for every handler, would stall
	// for up to a minute. Drop the connections and the listener instead.
	t.Cleanup(func() {
		server.CloseClientConnections()
		_ = server.Listener.Close()
	})
	return server, hub, streaming.NewEventRouter(hub, store, jsonSerializer{}), alice
}

func TestHealth(t *testing.T) {
	server, _, _, _ := newServer(t)
	resp, err := http.Get(server.URL + "/api/v1/streaming/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "OK" {
		t.Errorf("health = %d %q", resp.StatusCode, body)
	}
}

func TestUserStreamRequiresToken(t *testing.T) {
	server, _, _, _ := newServer(t)
	resp, err := http.Get(server.URL + "/api/v1/streaming/user")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous user stream = %d, want 401", resp.StatusCode)
	}
}

func TestPublicLocalStreamDeliversNewStatus(t *testing.T) {
	server, hub, router, alice := newServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/streaming/public/local", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	lines := make(chan string, 64)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	for !hub.HasSubscribers() {
		select {
		case <-ctx.Done():
			t.Fatal("the stream never subscribed")
		case <-time.After(10 * time.Millisecond):
		}
	}

	status := &models.Status{ID: "01STATUS", AccountID: alice.ID, Account: alice, ContentHTML: "<p>live!</p>", Visibility: models.VisibilityPublic}
	router.StatusCreated(ctx, status)

	sawUpdate := false
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed before the event arrived")
			}
			if line == "event: update" {
				sawUpdate = true
			}
			if sawUpdate && strings.HasPrefix(line, "data:") {
				if !strings.Contains(line, "01STATUS") || !strings.Contains(line, "live!") {
					t.Errorf("event data = %s", line)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("no update event received")
		}
	}
}

func TestLocalBrokerDelivers(t *testing.T) {
	var got []streaming.BrokerEvent
	broker := &streaming.LocalBroker{}
	if err := broker.Start(context.Background(), func(_ context.Context, e streaming.BrokerEvent) { got = append(got, e) }); err != nil {
		t.Fatal(err)
	}
	streaming.NewBrokerPublisher(broker).StatusCreated(context.Background(), &models.Status{ID: "s1", AccountID: "a1", Visibility: models.VisibilityPublic})
	if len(got) != 1 || got[0].Kind != "status.created" || got[0].ID != "s1" {
		t.Errorf("broker events = %+v", got)
	}
}
