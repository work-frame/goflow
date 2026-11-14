package client

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/api"
	"github.com/work-frame/goflow/internal/auth"
	"github.com/work-frame/goflow/internal/job"
)

type secretsStub struct{ rotated int }

func (s *secretsStub) Has(context.Context, string) (bool, error) { return s.rotated > 0, nil }
func (s *secretsStub) Rotate(context.Context, string) (string, time.Time, error) {
	s.rotated++
	if s.rotated == 1 {
		return "whsec_one", time.Time{}, nil
	}
	return "whsec_two", time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC), nil
}

type attemptsStub struct{}

func (attemptsStub) List(_ context.Context, _, id string) ([]job.Attempt, error) {
	if id == "missing" {
		return nil, job.ErrNotFound
	}
	return []job.Attempt{{Attempt: 1, Outcome: job.OutcomeDelivered, StatusCode: 204, DurationMS: 31}}, nil
}

// The client's webhook calls against the real router: creating a webhook
// job, the secret (first and rotated), and the attempts.
func TestClientWebhooks(t *testing.T) {
	token, sess := auth.NewSession("client-test-user", time.Now())
	router := api.NewRouter(api.Deps{
		Jobs:            memStore{m: job.NewMemoryStore()},
		Sessions:        oneSession{hash: sess.TokenHash, user: auth.User{ID: "client-test-user"}},
		DB:              alwaysReady{},
		Logger:          slog.New(slog.DiscardHandler),
		WebhookSecrets:  &secretsStub{},
		WebhookAttempts: attemptsStub{},
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	c := New(srv.URL, token)
	ctx := context.Background()

	secret, until, err := c.RotateWebhookSecret(ctx)
	if err != nil || secret != "whsec_one" || until != nil {
		t.Fatalf("first secret: %q, %v, %v", secret, until, err)
	}
	secret, until, err = c.RotateWebhookSecret(ctx)
	if err != nil || secret != "whsec_two" || until == nil || !until.Equal(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("rotation: %q, %v, %v", secret, until, err)
	}

	j, err := c.CreateJob(ctx, NewJob{Title: "paid", Type: job.TypeWebhook, MaxAttempts: 4,
		Webhook: &job.Webhook{URL: "https://example.com/hook", Body: json.RawMessage(`{"order":1042}`)}})
	if err != nil || j.Type != job.TypeWebhook || j.MaxAttempts != 4 || j.Webhook == nil || j.Webhook.URL != "https://example.com/hook" {
		t.Fatalf("CreateJob = %+v, %v", j, err)
	}

	list, err := c.Attempts(ctx, j.ID)
	if err != nil || len(list) != 1 || list[0].StatusCode != 204 {
		t.Errorf("Attempts = %+v, %v", list, err)
	}
	if _, err := c.Attempts(ctx, "missing"); err != job.ErrNotFound {
		t.Errorf("Attempts of a missing job: %v, want job.ErrNotFound", err)
	}
}
