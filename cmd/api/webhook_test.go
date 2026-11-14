package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/egress"
	"github.com/work-frame/goflow/internal/job"
	"github.com/work-frame/goflow/internal/webhook"
)

// The whole path: a user gets a signing secret, creates a webhook job, the
// receiver answers 503 once and then 200, and GoFlow retries, signs both
// attempts with the same webhook-id, and records both.
func TestWebhookIsDeliveredSignedRetriedAndRecorded(t *testing.T) {
	parallelTest(t)
	var mu sync.Mutex
	var secret string
	var ids []string
	var verified int
	recv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts, _ := strconv.ParseInt(r.Header.Get(webhook.HeaderTimestamp), 10, 64)
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, r.Header.Get(webhook.HeaderID))
		if webhook.Verify(secret, r.Header.Get(webhook.HeaderID), ts, body, r.Header.Get(webhook.HeaderSignature)) {
			verified++
		}
		if len(ids) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(recv.Close)

	cfg := testConfig("127.0.0.1:0", testDatabaseURL(t))
	cfg.RedisURL = testRedisURL(t)
	cfg.Workers = 2
	cfg.WebhookClient = egress.NewTestClient(t, egress.TestOptions{
		Allow:   []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
		Hosts:   map[string]netip.AddrPort{"example.com": netip.MustParseAddrPort(recv.Listener.Addr().String())},
		RootCAs: recv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
	})
	base, _ := startServerWith(t, cfg)
	client := testClient(t, 5*time.Second)
	token := signUp(t, client, base, cfg.DatabaseURL)
	create := `{"title":"order 1042 paid","type":"webhook","webhook":{"url":"https://example.com/hook","body":{"order":1042}}}`

	if status, _, body := requestAs(t, client, http.MethodPost, base+"/api/v1/jobs", token, create); status != http.StatusBadRequest {
		t.Fatalf("webhook job before a secret: %d %s, want 400", status, body)
	}
	status, _, body := requestAs(t, client, http.MethodPost, base+"/api/v1/webhook-secret", token, "")
	if status != http.StatusCreated {
		t.Fatalf("webhook-secret: %d %s", status, body)
	}
	var got struct{ Secret string }
	json.Unmarshal(body, &got)
	mu.Lock()
	secret = got.Secret
	mu.Unlock()

	status, _, body = requestAs(t, client, http.MethodPost, base+"/api/v1/jobs", token, create)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	var created struct{ ID string }
	json.Unmarshal(body, &created)

	deadline := time.Now().Add(15 * time.Second)
	for {
		st, attempts := jobState(t, client, base, token, created.ID)
		if st == "completed" {
			if attempts != 2 {
				t.Errorf("completed on attempt %d, want 2 (503, then 200)", attempts)
			}
			break
		}
		if st == "dead" || time.Now().After(deadline) {
			t.Fatalf("job %s is %s after %d attempts", created.ID, st, attempts)
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	if len(ids) != 2 || ids[0] != created.ID || ids[1] != created.ID || verified != 2 {
		t.Errorf("receiver saw webhook-ids %v, %d verified; want the job ID twice, both signed with the secret", ids, verified)
	}
	mu.Unlock()

	status, _, body = requestAs(t, client, http.MethodGet, base+"/api/v1/jobs/"+created.ID+"/attempts", token, "")
	var list struct{ Attempts []job.Attempt }
	json.Unmarshal(body, &list)
	if status != http.StatusOK || len(list.Attempts) != 2 ||
		list.Attempts[0].Outcome != job.OutcomeRetry || list.Attempts[0].StatusCode != 503 ||
		list.Attempts[1].Outcome != job.OutcomeDelivered || list.Attempts[1].StatusCode != 200 {
		t.Errorf("attempts: %d %s, want retry 503 then delivered 200", status, body)
	}
}
