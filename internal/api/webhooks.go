package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

// WebhookSecrets is the part of database.SecretStore the API uses.
type WebhookSecrets interface {
	Rotate(ctx context.Context, userID string) (secret string, previousUntil time.Time, err error)
	Has(ctx context.Context, userID string) (bool, error)
}

// WebhookAttempts is the part of database.AttemptStore the API uses.
type WebhookAttempts interface {
	List(ctx context.Context, userID, jobID string) ([]job.Attempt, error)
}

type rotateSecretResponse struct {
	// Secret is shown once: GoFlow can't show it again.
	Secret string `json:"secret"`
	// PreviousValidUntil is when the secret this one replaced stops
	// signing deliveries. Omitted for a first secret.
	PreviousValidUntil *time.Time `json:"previous_valid_until,omitempty"`
}

// rotateSecret creates the caller's signing secret, or replaces it. The
// response is the only place the secret ever appears: it is never logged,
// and never stored unencrypted.
func (h *handler) rotateSecret(w http.ResponseWriter, r *http.Request) {
	if h.secrets == nil {
		writeError(w, http.StatusServiceUnavailable, "webhooks aren't available on this server")
		return
	}
	ctx, cancel := dbContext(r)
	defer cancel()
	secret, until, err := h.secrets.Rotate(ctx, callerID(r))
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	resp := rotateSecretResponse{Secret: secret}
	if !until.IsZero() {
		resp.PreviousValidUntil = &until
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, resp)
}

// requireSecret answers 400 (or 503) and returns false unless the caller
// has a signing secret.
func (h *handler) requireSecret(ctx context.Context, w http.ResponseWriter, r *http.Request) bool {
	if h.secrets == nil {
		writeError(w, http.StatusServiceUnavailable, "webhooks aren't available on this server")
		return false
	}
	ok, err := h.secrets.Has(ctx, callerID(r))
	if err != nil {
		h.storeError(w, r, err)
		return false
	}
	if !ok {
		writeError(w, http.StatusBadRequest, "create a signing secret first: POST /api/v1/webhook-secret")
		return false
	}
	return true
}

type listAttemptsResponse struct {
	Attempts []job.Attempt `json:"attempts"`
}

func (h *handler) listAttempts(w http.ResponseWriter, r *http.Request) {
	if h.attempts == nil {
		writeError(w, http.StatusServiceUnavailable, "webhooks aren't available on this server")
		return
	}
	ctx, cancel := dbContext(r)
	defer cancel()
	list, err := h.attempts.List(ctx, callerID(r), r.PathValue("id"))
	if errors.Is(err, job.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listAttemptsResponse{Attempts: list})
}
