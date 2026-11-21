package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/work-frame/goflow/internal/auth"
	"github.com/work-frame/goflow/internal/ratelimit"
)

// API keys (Task 86). A key is a bearer token for another program: it can
// only use routes wrapped in requireAuth with a scope it holds. Everything
// else, account routes included, stays behind requireSession, which refuses
// keys. Keys are made, listed and revoked only by a logged-in person.

// KeyStore is what the API needs from key storage. It only ever receives
// hashes. database.APIKeyStore satisfies it.
type KeyStore interface {
	Create(ctx context.Context, k *auth.APIKey) error
	Lookup(ctx context.Context, keyHash []byte) (*auth.User, *auth.APIKey, bool, error)
	Touch(ctx context.Context, id string) error
	List(ctx context.Context, userID string) ([]*auth.APIKey, error)
	Revoke(ctx context.Context, userID, id string) error
	RevokeAll(ctx context.Context, userID string) (int64, error)
}

var (
	// One key can't flood the queue: 60 job creates a minute (K6).
	keyJobsPerKey = ratelimit.Rule{Name: "api_key_jobs", Limit: 60, Window: time.Minute}
	// Nor can ten keys together: 120 a minute for all of an account's keys
	// (condition 5). Both counters are atomic in Redis, so requests that
	// arrive together through different keys can't slip past the total.
	keyJobsPerAccount = ratelimit.Rule{Name: "api_key_account_jobs", Limit: 120, Window: time.Minute}
	// Making a key checks the password, so it is a password guess too:
	// counted like a password change, in a bucket of its own.
	keyCreatesPerAccount = ratelimit.Rule{Name: "api_key_create", Limit: 5, Window: 15 * time.Minute}
)

// requireAuth lets a request through with a valid session, which may do
// everything a person can, or with a valid API key that holds scope. A
// key without it gets 403 naming the scope. Deny by default: there is no
// way to call requireAuth without a scope, and a route not wrapped in it
// is closed to keys.
func (h *handler) requireAuth(scope auth.Scope, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			unauthorized(w)
			return
		}
		var s *session
		if auth.IsKey(token) {
			if s, ok = h.authenticateKey(w, r, token); !ok {
				return
			}
			if !s.key.Allows(scope) {
				h.logger.Info("api key refused", "request_id", RequestID(r.Context()), "key_id", s.key.ID, "missing_scope", string(scope))
				writeError(w, http.StatusForbidden, fmt.Sprintf("this API key doesn't have the %s scope", scope))
				return
			}
		} else if s, ok = h.sessionFor(w, r, token); !ok {
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey, s)))
	}
}

// authenticateKey finds the working key behind a gfk_ token, or answers 401
// (or the store's error) and returns false. Every request asks the
// database: revoked or expired keys fail at once (condition 3). It records
// the use at most once a minute; a failure to record is logged, not fatal.
func (h *handler) authenticateKey(w http.ResponseWriter, r *http.Request, token string) (*session, bool) {
	if h.keys == nil || !auth.ValidKeyFormat(token) {
		unauthorized(w)
		return nil, false
	}
	hash := auth.HashToken(token)
	ctx, cancel := dbContext(r)
	defer cancel()
	u, k, stale, err := h.keys.Lookup(ctx, hash)
	if errors.Is(err, auth.ErrKeyNotFound) {
		unauthorized(w)
		return nil, false
	}
	if err != nil {
		h.storeError(w, r, err)
		return nil, false
	}
	if stale {
		if err := h.keys.Touch(ctx, k.ID); err != nil {
			h.logger.Warn("api key use not recorded", "request_id", RequestID(r.Context()), "key_id", k.ID, "err", err)
		}
	}
	return &session{user: u, tokenHash: hash, key: k}, true
}

// currentKey is the API key the caller used, or nil for a logged-in person.
func currentKey(r *http.Request) *auth.APIKey {
	if s := currentSession(r.Context()); s != nil {
		return s.key
	}
	return nil
}

// limitKeyJobs counts a job create made with an API key, per key and then
// per account, and answers 429 if either is over. Session creates aren't
// counted: the limits are for programs, not the dashboard.
func (h *handler) limitKeyJobs(w http.ResponseWriter, r *http.Request) bool {
	k := currentKey(r)
	if k == nil {
		return false
	}
	return h.limited(w, r, keyJobsPerKey, k.ID) || h.limited(w, r, keyJobsPerAccount, k.UserID)
}

// keyResponse is the public view of a key: never its hash or secret.
type keyResponse struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Scopes     []auth.Scope `json:"scopes"`
	Hint       string       `json:"hint"`
	Status     string       `json:"status"`
	CreatedAt  time.Time    `json:"created_at"`
	ExpiresAt  time.Time    `json:"expires_at"`
	LastUsedAt *time.Time   `json:"last_used_at"`
	RevokedAt  *time.Time   `json:"revoked_at"`
}

func newKeyResponse(k *auth.APIKey) keyResponse {
	return keyResponse{ID: k.ID, Name: k.Name, Scopes: k.Scopes, Hint: k.Hint, Status: k.Status,
		CreatedAt: k.CreatedAt, ExpiresAt: k.ExpiresAt, LastUsedAt: k.LastUsedAt, RevokedAt: k.RevokedAt}
}

type createKeyRequest struct {
	Name          string       `json:"name"`
	Scopes        []auth.Scope `json:"scopes"`
	ExpiresInDays int          `json:"expires_in_days"`
	Password      string       `json:"password"`
}

type createKeyResponse struct {
	Key keyResponse `json:"key"`
	// Secret is the key itself, sent this once and never again (condition 4).
	Secret string `json:"secret"`
}

// createKey makes a key. It needs the current password as well as the
// session (K5): a stolen session lasts 24 hours at most, and must not be
// turned into a key that lasts a year.
func (h *handler) createKey(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeRequestError(w, err)
		return
	}
	s := currentSession(r.Context())
	if h.limited(w, r, keyCreatesPerAccount, s.user.ID) {
		return
	}
	err := h.hasher.Compare(s.user.PasswordHash, req.Password)
	if errors.Is(err, auth.ErrWrongPassword) {
		writeError(w, http.StatusForbidden, "password is incorrect")
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}

	secret, k, err := auth.NewAPIKey(s.user.ID, req.Name, req.Scopes, req.ExpiresInDays, time.Now())
	switch {
	case errors.Is(err, auth.ErrInvalidKeyName):
		writeError(w, http.StatusBadRequest, fmt.Sprintf("name must be 1 to %d characters", auth.MaxKeyNameLength))
		return
	case errors.Is(err, auth.ErrInvalidScopes):
		writeError(w, http.StatusBadRequest, "scopes must be one or more of jobs:create, jobs:read")
		return
	case errors.Is(err, auth.ErrInvalidLifetime):
		writeError(w, http.StatusBadRequest, "expires_in_days must be 30, 90 or 365")
		return
	case err != nil:
		h.internalError(w, r, err)
		return
	}

	ctx, cancel := dbContext(r)
	defer cancel()
	err = h.keys.Create(ctx, k)
	if errors.Is(err, auth.ErrTooManyKeys) {
		writeError(w, http.StatusConflict, fmt.Sprintf("an account can have at most %d active keys; revoke one first", auth.MaxActiveKeys))
		return
	}
	if errors.Is(err, auth.ErrUserNotFound) {
		unauthorized(w)
		return
	}
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	h.logger.Info("api key created", "request_id", RequestID(r.Context()), "user_id", s.user.ID, "key_id", k.ID)
	// The secret is a credential: no browser or proxy may keep a copy.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, createKeyResponse{Key: newKeyResponse(k), Secret: secret})
}

// listKeys lists every key the caller ever made, newest first, with its
// status: active, expired or revoked (permanent history).
func (h *handler) listKeys(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := dbContext(r)
	defer cancel()
	keys, err := h.keys.List(ctx, callerID(r))
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	out := make([]keyResponse, len(keys))
	for i, k := range keys {
		out[i] = newKeyResponse(k)
	}
	writeJSON(w, http.StatusOK, map[string][]keyResponse{"keys": out})
}

// revokeKey ends one key at once. Revoking it twice is fine; someone
// else's key is 404, the same as one that doesn't exist.
func (h *handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := dbContext(r)
	defer cancel()
	err := h.keys.Revoke(ctx, callerID(r), r.PathValue("id"))
	if errors.Is(err, auth.ErrKeyNotFound) {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	h.logger.Info("api key revoked", "request_id", RequestID(r.Context()), "user_id", callerID(r), "key_id", r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// revokeAllKeys ends every key the caller has.
func (h *handler) revokeAllKeys(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := dbContext(r)
	defer cancel()
	n, err := h.keys.RevokeAll(ctx, callerID(r))
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	h.logger.Info("api keys revoked", "request_id", RequestID(r.Context()), "user_id", callerID(r), "revoked", n)
	writeJSON(w, http.StatusOK, map[string]int64{"revoked": n})
}

// keysAvailable answers 503 when no key store is configured.
func (h *handler) keysAvailable(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.keys == nil {
			writeError(w, http.StatusServiceUnavailable, "API keys are not available")
			return
		}
		next(w, r)
	}
}
