package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/webhook"
)

const secret = "whsec_cmVjZWl2ZXItdGVzdC1zZWNyZXQtMzItYnl0ZXMhIQ=="

var now = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

type sent struct {
	id   string
	ts   time.Time
	sign string // the secret it is signed with
	body string
	path string
}

func deliver(t *testing.T, h http.Handler, s sent) int {
	t.Helper()
	if s.sign == "" {
		s.sign = secret
	}
	if s.path == "" {
		s.path = "/hook"
	}
	if s.body == "" {
		s.body = `{"order":1042}`
	}
	sig, err := webhook.Sign(s.sign, s.id, s.ts.Unix(), []byte(s.body))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, s.path, strings.NewReader(s.body))
	req.Header.Set(webhook.HeaderID, s.id)
	req.Header.Set(webhook.HeaderTimestamp, strconv.FormatInt(s.ts.Unix(), 10))
	req.Header.Set(webhook.HeaderSignature, sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func newTest(t *testing.T) (http.Handler, *bytes.Buffer) {
	var logs bytes.Buffer
	rc := newReceiver(secret, slog.New(slog.NewTextHandler(&logs, nil)), func() time.Time { return now })
	return rc.routes(), &logs
}

func TestTheFourChecks(t *testing.T) {
	h, logs := newTest(t)
	if c := deliver(t, h, sent{id: "job1", ts: now, sign: "whsec_b3RoZXItc2VjcmV0"}); c != http.StatusUnauthorized {
		t.Errorf("wrong secret: %d, want 401", c)
	}
	if c := deliver(t, h, sent{id: "job1", ts: now.Add(-6 * time.Minute)}); c != http.StatusBadRequest {
		t.Errorf("signed 6 minutes ago: %d, want 400 (a replay)", c)
	}
	if c := deliver(t, h, sent{id: "job1", ts: now.Add(6 * time.Minute)}); c != http.StatusBadRequest {
		t.Errorf("signed 6 minutes in the future: %d, want 400", c)
	}
	if c := deliver(t, h, sent{id: "job1", ts: now.Add(-4 * time.Minute)}); c != http.StatusOK {
		t.Errorf("valid: %d, want 200", c)
	}
	if c := deliver(t, h, sent{id: "job1", ts: now}); c != http.StatusOK {
		t.Errorf("repeat: %d, want 200", c)
	}
	if n := strings.Count(logs.String(), `msg="webhook processed"`); n != 1 {
		t.Errorf("processed %d times, want once:\n%s", n, logs)
	}
	if strings.Contains(logs.String(), "1042") {
		t.Error("the log contains the body")
	}
}

// A signature over one body doesn't cover another.
func TestChangedBodyIsRefused(t *testing.T) {
	h, _ := newTest(t)
	sig, _ := webhook.Sign(secret, "job2", now.Unix(), []byte(`{"amount":1}`))
	req := httptest.NewRequest(http.MethodPost, "/hook", strings.NewReader(`{"amount":1000}`))
	req.Header.Set(webhook.HeaderID, "job2")
	req.Header.Set(webhook.HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
	req.Header.Set(webhook.HeaderSignature, sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("changed body: %d, want 401", rec.Code)
	}
}

func TestFailOnPurpose(t *testing.T) {
	h, _ := newTest(t)
	var codes []int
	for range 4 {
		codes = append(codes, deliver(t, h, sent{id: "job3", ts: now, path: "/hook?fail=2"}))
	}
	if codes[0] != 503 || codes[1] != 503 || codes[2] != 200 || codes[3] != 200 {
		t.Errorf("codes %v, want 503 503 200 200", codes)
	}
	// Each webhook-id gets its own failures.
	if c := deliver(t, h, sent{id: "job4", ts: now, path: "/hook?fail=1"}); c != 503 {
		t.Errorf("another job's first attempt: %d, want 503", c)
	}
}

func TestCheckSecret(t *testing.T) {
	for s, ok := range map[string]bool{secret: true, "": false, "abc": false, "whsec_***": false} {
		err := checkSecret(s)
		if (err == nil) != ok {
			t.Errorf("checkSecret(%q) = %v", s, err)
		}
		if err != nil && s != "" && strings.Contains(err.Error(), s) {
			t.Errorf("the error repeats the secret: %v", err)
		}
	}
}

func TestHealthz(t *testing.T) {
	h, _ := newTest(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthz: %d", rec.Code)
	}
}
