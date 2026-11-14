package database

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/work-frame/goflow/internal/job"
)

func mustNewWebhookJob(t *testing.T, owner string) *job.Job {
	t.Helper()
	j := mustNewJob(t, owner, "order 1042 paid")
	j.Type = job.TypeWebhook
	j.Webhook = &job.Webhook{URL: "https://example.com/hooks?token=t0k3n", Body: json.RawMessage(`{"order": 1042, "items": [1, 2]}`)}
	j.MaxAttempts = 5
	return j
}

// compactJSON is b without insignificant spaces, for comparing jsonb's
// output with what went in.
func compactJSON(t *testing.T, b []byte) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		t.Fatalf("compact %q: %v", b, err)
	}
	return out.String()
}

func TestWebhookJobRoundTrip(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	ctx := context.Background()
	want := mustNewWebhookJob(t, f.alice)
	if err := f.s.Add(ctx, want); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := f.s.Get(ctx, f.alice, want.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Type != job.TypeWebhook || got.MaxAttempts != 5 || got.Webhook == nil || got.Webhook.URL != want.Webhook.URL {
		t.Fatalf("Get: %+v (webhook %+v), want the webhook job as added", got, got.Webhook)
	}
	if c := compactJSON(t, got.Webhook.Body); c != `{"items":[1,2],"order":1042}` {
		t.Errorf("Get: body %s, want the body as added", c)
	}

	byKey := mustNewWebhookJob(t, f.alice)
	byKey.IdempotencyKey = "order-1042"
	if err := f.s.Add(ctx, byKey); err != nil {
		t.Fatalf("Add with key: %v", err)
	}
	replay, err := f.s.GetByIdempotencyKey(ctx, f.alice, "order-1042")
	if err != nil || replay.Webhook == nil || replay.Webhook.Body == nil {
		t.Fatalf("GetByIdempotencyKey: %+v, %v; want the webhook with its body", replay, err)
	}

	// Lists and the worker's claim carry the type and URL, never the body.
	page, err := f.s.List(ctx, f.alice, job.ListParams{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, j := range page.Jobs {
		if j.Webhook == nil || j.Webhook.URL != want.Webhook.URL || j.Webhook.Body != nil {
			t.Errorf("List: webhook %+v, want the URL without the body", j.Webhook)
		}
	}
	claimed, won, err := NewWorkStore(f.db).Claim(ctx, want.ID, testLease)
	if err != nil || !won || claimed.Type != job.TypeWebhook {
		t.Errorf("Claim: %+v, won %v, err %v; want a webhook claim", claimed, won, err)
	}
}

// Old rows and jobs built without job.New are demo jobs with no payload.
func TestJobsWithoutATypeAreDemoJobs(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	ctx := context.Background()
	j := mustNewJob(t, f.alice, "sleep:1s")
	j.Type = ""
	if err := f.s.Add(ctx, j); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := f.s.Get(ctx, f.alice, j.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Type != job.TypeDemo || got.Webhook != nil {
		t.Errorf("Get: type %q, webhook %+v; want demo with none", got.Type, got.Webhook)
	}
}

// PostgreSQL refuses a payload that doesn't match the type, whatever Go
// sends: a webhook job without a URL could never be delivered.
func TestPayloadMustMatchTheType(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	for name, c := range map[string]struct {
		typ     string
		payload any
	}{
		"webhook without payload":  {"webhook", nil},
		"webhook without url":      {"webhook", `{"body": {}}`},
		"webhook with numeric url": {"webhook", `{"url": 1, "body": {}}`},
		"webhook without body":     {"webhook", `{"url": "https://example.com"}`},
		"demo with payload":        {"demo", `{"url": "https://example.com", "body": {}}`},
		"unknown type":             {"email", nil},
	} {
		_, err := f.db.ExecContext(context.Background(),
			`INSERT INTO jobs (id, user_id, title, status, created_at, updated_at, type, payload)
			 VALUES (md5(random()::text), $1, 'x', 'pending', now(), now(), $2, $3)`, f.alice, c.typ, c.payload)
		if err == nil {
			t.Errorf("%s: inserted, want a CHECK violation", name)
		}
	}
}
