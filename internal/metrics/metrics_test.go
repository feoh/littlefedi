package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCountersAppearInSnapshot(t *testing.T) {
	Init("test.example", "1.2.3")
	before := Snapshot()
	IncrementAPIRequests()
	IncrementDeliveriesAttempted()
	IncrementDeliveriesSucceeded()
	IncrementQueueEnqueued()
	IncrementFederationPolicyRejected("inbound", "blocked")
	ObserveTimelineStage("query", 5*time.Millisecond)

	after := Snapshot()
	if after.Domain != "test.example" || after.Version != "1.2.3" {
		t.Errorf("identity = %q %q", after.Domain, after.Version)
	}
	if after.APIRequests != before.APIRequests+1 {
		t.Errorf("APIRequests %d -> %d", before.APIRequests, after.APIRequests)
	}
	if after.DeliveriesAttempted != before.DeliveriesAttempted+1 || after.DeliveriesSucceeded != before.DeliveriesSucceeded+1 {
		t.Error("delivery counters did not advance")
	}
	if after.QueueEnqueued != before.QueueEnqueued+1 {
		t.Error("queue counter did not advance")
	}
	if sum(after.FederationPolicyRejected) != sum(before.FederationPolicyRejected)+1 {
		t.Errorf("policy rejection not recorded: %v", after.FederationPolicyRejected)
	}
	if after.Uptime < 0 {
		t.Errorf("Uptime = %v", after.Uptime)
	}
}

func sum(m map[string]int64) int64 {
	var total int64
	for _, v := range m {
		total += v
	}
	return total
}

func TestPoolWorkers(t *testing.T) {
	SetPoolConfiguredWorkers("test-pool", 4)
	IncrementPoolActiveWorkers("test-pool")
	pool := Snapshot().WorkerPools["test-pool"]
	DecrementPoolActiveWorkers("test-pool")
	if pool.Configured != 4 || pool.Active != 1 {
		t.Errorf("pool stats = %+v", pool)
	}
}

func TestHandlerServesPrometheusText(t *testing.T) {
	Init("test.example", "1.2.3")
	IncrementWebRequests()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, want := range []string{"# TYPE littlefedi_api_requests_total counter", "littlefedi_web_requests_total"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics output lacks %q", want)
		}
	}
}
