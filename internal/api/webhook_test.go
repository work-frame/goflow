package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/work-frame/goflow/internal/job"
)

const webhookCreate = `{"title":"order 1042 paid","type":"webhook","webhook":{"url":"https://example.com/hooks/goflow?token=t0k3n","body":{"order":1042,"amount":"19.99"}},"max_attempts":5}`

func TestCreateWebhookJob(t *testing.T) {
	h := newRouter()
	rec := do(t, h, http.MethodPost, "/api/v1/jobs", webhookCreate)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	j := decode[job.Job](t, rec)
	if j.Type != job.TypeWebhook || j.MaxAttempts != 5 || j.Webhook == nil ||
		j.Webhook.URL != "https://example.com/hooks/goflow?token=t0k3n" || !sameJSON(j.Webhook.Body, []byte(`{"amount":"19.99","order":1042}`)) {
		t.Fatalf("created %+v (webhook %+v), want the webhook as sent with max_attempts 5", j, j.Webhook)
	}

	got := decode[job.Job](t, do(t, h, http.MethodGet, "/api/v1/jobs/"+j.ID, ""))
	if got.Webhook == nil || !sameJSON(got.Webhook.Body, j.Webhook.Body) {
		t.Errorf("GET one job: webhook %+v, want the body included", got.Webhook)
	}

	// A list leaves bodies out: 100 jobs with 64 KiB bodies would be 6 MiB.
	list := do(t, h, http.MethodGet, "/api/v1/jobs", "")
	if strings.Contains(list.Body.String(), "19.99") {
		t.Errorf("list includes a webhook body: %s", list.Body)
	}
	if !strings.Contains(list.Body.String(), `"type":"webhook"`) || !strings.Contains(list.Body.String(), "hooks/goflow") {
		t.Errorf("list lacks the job's type or URL: %s", list.Body)
	}
}

func TestCreateDemoJobByDefault(t *testing.T) {
	j := mustCreate(t, newRouter(), "sleep:1s")
	if j.Type != job.TypeDemo || j.Webhook != nil || j.MaxAttempts != job.DefaultMaxAttempts {
		t.Errorf("created %+v, want a demo job with %d attempts and no webhook", j, job.DefaultMaxAttempts)
	}
}

func TestCreateWebhookJobRefusals(t *testing.T) {
	big := `"` + strings.Repeat("a", job.MaxWebhookBodyBytes) + `"`
	for _, c := range []struct {
		name, body, want string
	}{
		{"webhook without type", `{"title":"x","webhook":{"url":"https://example.com/x","body":{}}}`, `only allowed with "type": "webhook"`},
		{"type without webhook", `{"title":"x","type":"webhook"}`, `needs "webhook"`},
		{"unknown type", `{"title":"x","type":"email"}`, `type must be "demo" or "webhook"`},
		{"http", `{"title":"x","type":"webhook","webhook":{"url":"http://example.com/x","body":{}}}`, "must use https"},
		{"port", `{"title":"x","type":"webhook","webhook":{"url":"https://example.com:8080/x","body":{}}}`, "port 443"},
		{"credentials", `{"title":"x","type":"webhook","webhook":{"url":"https://u:p@example.com/x","body":{}}}`, "username or password"},
		{"no body", `{"title":"x","type":"webhook","webhook":{"url":"https://example.com/x"}}`, "body is required"},
		{"big body", `{"title":"x","type":"webhook","webhook":{"url":"https://example.com/x","body":` + big + `}}`, "at most 65536 bytes"},
		{"unknown webhook field", `{"title":"x","type":"webhook","webhook":{"url":"https://example.com/x","body":{},"headers":{"Authorization":"x"}}}`, `unknown field "headers"`},
		{"zero attempts", `{"title":"x","max_attempts":0}`, "between 1 and 10"},
		{"eleven attempts", `{"title":"x","max_attempts":11}`, "between 1 and 10"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, newRouter(), http.MethodPost, "/api/v1/jobs", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400; body %s", rec.Code, rec.Body)
			}
			if msg := decode[map[string]string](t, rec)["error"]; !strings.Contains(msg, c.want) {
				t.Errorf("error %q doesn't say %q", msg, c.want)
			}
		})
	}
}

func createWebhookWithKey(t *testing.T, h http.Handler, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idempotencyKeyHeader, key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A replay compares the work, not just the title: the same key with another
// URL or body is a different request.
func TestWebhookReplayComparesURLAndBody(t *testing.T) {
	h := newRouter()
	if rec := createWebhookWithKey(t, h, webhookCreate, "order-1042"); rec.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", rec.Code, rec.Body)
	}
	same := strings.Replace(webhookCreate, `{"order":1042,"amount":"19.99"}`, `{ "amount" : "19.99", "order" : 1.042e3 }`, 1)
	if rec := createWebhookWithKey(t, h, same, "order-1042"); rec.Code != http.StatusOK {
		t.Errorf("same body, other spelling: %d %s, want 200 (a replay)", rec.Code, rec.Body)
	}
	for name, body := range map[string]string{
		"other url":      strings.Replace(webhookCreate, "hooks/goflow", "hooks/other", 1),
		"other body":     strings.Replace(webhookCreate, `"order":1042`, `"order":1043`, 1),
		"other attempts": strings.Replace(webhookCreate, `"max_attempts":5`, `"max_attempts":4`, 1),
		"demo instead":   `{"title":"order 1042 paid"}`,
	} {
		if rec := createWebhookWithKey(t, h, body, "order-1042"); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s, want 422", name, rec.Code, rec.Body)
		}
	}
}

func TestSameJSONComparesNumbersExactly(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{`{"a":1,"b":[true,null,"x"]}`, `{"b":[true,null,"x"],"a":1}`, true},
		{`100`, `1e2`, true},
		{`1.50`, `1.5`, true},
		{`12345678901234567890`, `12345678901234567891`, false},
		{`{"a":1}`, `{"a":1,"b":2}`, false},
		{`[1,2]`, `[2,1]`, false},
		{`"1"`, `1`, false},
		{`{"a":`, `{"a":1}`, false},
	} {
		if got := sameJSON([]byte(c.a), []byte(c.b)); got != c.want {
			t.Errorf("sameJSON(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
