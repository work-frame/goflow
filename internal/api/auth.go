package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/work-frame/goflow/internal/auth"
)

// UserStore is what the API needs from account storage.
// database.UserStore satisfies it in production.
type UserStore interface {
	Create(ctx context.Context, u *auth.User) error
	GetByEmail(ctx context.Context, email string) (*auth.User, error)
	UpdatePasswordHash(ctx context.Context, id, hash string) error
	ChangePassword(ctx context.Context, userID, hash string, keepTokenHash []byte) (int64, error)
}

// SessionStore is what the API needs from login storage. It only ever
// receives token hashes, never tokens. database.SessionStore satisfies it.
type SessionStore interface {
	Create(ctx context.Context, s *auth.Session) error
	Lookup(ctx context.Context, tokenHash []byte) (*auth.User, error)
	Delete(ctx context.Context, tokenHash []byte) error
	DeleteAllForUser(ctx context.Context, userID string) (int64, error)
}

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// userResponse is the public view of a user. It is a separate type so a field
// added to auth.User later can't reach clients by accident.
type userResponse struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

func newUserResponse(u *auth.User) userResponse {
	return userResponse{ID: u.ID, Email: u.Email, CreatedAt: u.CreatedAt}
}

func (h *handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeRequestError(w, err)
		return
	}

	// The password lives only in req and in this call. It is never logged,
	// stored, or put into an error message.
	u, err := auth.NewUser(req.Email, req.Password, h.hasher)
	switch {
	case errors.Is(err, auth.ErrInvalidEmail):
		writeError(w, http.StatusBadRequest, "email is invalid")
		return
	case writePasswordPolicyError(w, "password", err):
		return
	case err != nil:
		h.internalError(w, r, err)
		return
	}

	ctx, cancel := dbContext(r)
	defer cancel()

	err = h.users.Create(ctx, u)
	if errors.Is(err, auth.ErrEmailTaken) {
		writeError(w, http.StatusConflict, "email is already registered")
		return
	}
	if err != nil {
		h.storeError(w, r, err)
		return
	}

	writeJSON(w, http.StatusCreated, newUserResponse(u))
}

// writePasswordPolicyError answers 400 if err is a password rule violation,
// naming the JSON field, and reports whether it did.
func writePasswordPolicyError(w http.ResponseWriter, field string, err error) bool {
	switch {
	case errors.Is(err, auth.ErrPasswordTooShort):
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("%s must be at least %d characters", field, auth.MinPasswordLength))
	case errors.Is(err, auth.ErrPasswordTooLong):
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("%s must be at most %d bytes", field, auth.MaxPasswordBytes))
	default:
		return false
	}
	return true
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// invalidCredentials is the one message for every failed login, so the
// response never tells an attacker whether an email is registered.
const invalidCredentials = "invalid email or password"

// dummyHash is a bcrypt hash at the server's cost, made on first use.
// Comparing against it takes as long as checking a real password.
type dummyHash struct {
	once sync.Once
	hash string
}

func (d *dummyHash) get(h auth.Hasher) string {
	d.once.Do(func() {
		d.hash, _ = h.Hash("dummy password that never matches")
	})
	return d.hash
}

// prepareDummyHash builds the dummy hash in the background as soon as the
// router exists. Building it costs as much as a password check, so without
// this the first unknown-email login would take twice as long as the rest.
func (h *handler) prepareDummyHash() {
	if h.users != nil {
		go h.dummy.get(h.hasher)
	}
}

func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeRequestError(w, err)
		return
	}

	// A malformed email can't belong to anyone. Rejecting it fast reveals
	// nothing, because the format rules are public.
	email, err := auth.NormalizeEmail(req.Email)
	if err != nil {
		writeError(w, http.StatusUnauthorized, invalidCredentials)
		return
	}

	ctx, cancel := dbContext(r)
	defer cancel()

	u, err := h.users.GetByEmail(ctx, email)
	if errors.Is(err, auth.ErrUserNotFound) {
		// Spend the same ~0.5 s a real check would, so response time
		// doesn't reveal which emails exist.
		_ = h.hasher.Compare(h.dummy.get(h.hasher), req.Password)
		writeError(w, http.StatusUnauthorized, invalidCredentials)
		return
	}
	if err != nil {
		h.storeError(w, r, err)
		return
	}

	err = h.hasher.Compare(u.PasswordHash, req.Password)
	if errors.Is(err, auth.ErrWrongPassword) {
		writeError(w, http.StatusUnauthorized, invalidCredentials)
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}

	if h.hasher.NeedsRehash(u.PasswordHash) {
		h.upgradeHash(ctx, r, u.ID, req.Password)
	}

	token, sess := auth.NewSession(u.ID, time.Now())
	if err := h.sessions.Create(ctx, sess); err != nil {
		h.storeError(w, r, err)
		return
	}

	// The token is a credential: no browser or proxy may keep a copy.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, loginResponse{Token: token, ExpiresAt: sess.ExpiresAt})
}

// upgradeHash re-hashes a correct password at the current cost. A failure
// here must not fail the login: the old hash still works, so it is only logged.
func (h *handler) upgradeHash(ctx context.Context, r *http.Request, userID, password string) {
	hash, err := h.hasher.Hash(password)
	if err == nil {
		err = h.users.UpdatePasswordHash(ctx, userID, hash)
	}
	if err != nil {
		h.logger.Warn("password rehash failed", "request_id", RequestID(r.Context()), "user_id", userID, "err", err)
		return
	}
	h.logger.Info("password rehashed", "request_id", RequestID(r.Context()), "user_id", userID)
}

// sessionKey stores the caller's session in the request context.
const sessionKey ctxKey = 1

// session is who is calling, as found by requireSession.
type session struct {
	user      *auth.User
	tokenHash []byte
}

func currentSession(ctx context.Context) *session {
	s, _ := ctx.Value(sessionKey).(*session)
	return s
}

// requireSession lets a request through only with a valid, unexpired session
// token in "Authorization: Bearer <token>". It puts the session in the context.
func (h *handler) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok || !auth.ValidTokenFormat(token) {
			unauthorized(w)
			return
		}
		hash := auth.HashToken(token)

		ctx, cancel := dbContext(r)
		u, err := h.sessions.Lookup(ctx, hash)
		cancel()
		if errors.Is(err, auth.ErrSessionNotFound) {
			unauthorized(w)
			return
		}
		if err != nil {
			h.storeError(w, r, err)
			return
		}

		ctx = context.WithValue(r.Context(), sessionKey, &session{user: u, tokenHash: hash})
		next(w, r.WithContext(ctx))
	}
}

// bearerToken extracts the token from "Authorization: Bearer <token>".
// The scheme name is case-insensitive (RFC 9110).
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(token), true
}

// unauthorized is the single 401 for every session problem. It doesn't say
// which one: missing, malformed, unknown and expired look the same.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="goflow"`)
	writeError(w, http.StatusUnauthorized, "missing, invalid or expired session token")
}

func (h *handler) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, newUserResponse(currentSession(r.Context()).user))
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := dbContext(r)
	defer cancel()

	if err := h.sessions.Delete(ctx, currentSession(r.Context()).tokenHash); err != nil {
		h.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// callerID is the user ID of the signed-in caller. Only handlers behind
// requireSession may call it.
func callerID(r *http.Request) string {
	return currentSession(r.Context()).user.ID
}

type sessionsEndedResponse struct {
	SessionsEnded int64 `json:"sessions_ended"`
}

// logoutAll ends every session of the caller, including this one: the
// "my laptop was stolen" button. Afterwards every device has to log in again.
func (h *handler) logoutAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := dbContext(r)
	defer cancel()

	n, err := h.sessions.DeleteAllForUser(ctx, callerID(r))
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	h.logger.Info("logged out everywhere", "request_id", RequestID(r.Context()), "user_id", callerID(r), "sessions_ended", n)
	writeJSON(w, http.StatusOK, sessionsEndedResponse{SessionsEnded: n})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// changePassword needs the current password as well as a session: a stolen
// session alone must not be enough to lock the real owner out. On success it
// ends every other session, because a password change often means "someone
// else may be signed in", and keeps the one that made the change.
func (h *handler) changePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeRequestError(w, err)
		return
	}
	s := currentSession(r.Context())

	err := h.hasher.Compare(s.user.PasswordHash, req.CurrentPassword)
	if errors.Is(err, auth.ErrWrongPassword) {
		writeError(w, http.StatusForbidden, "current password is incorrect")
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}

	hash, err := h.hasher.Hash(req.NewPassword)
	if writePasswordPolicyError(w, "new_password", err) {
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}

	ctx, cancel := dbContext(r)
	defer cancel()

	ended, err := h.users.ChangePassword(ctx, s.user.ID, hash, s.tokenHash)
	if errors.Is(err, auth.ErrUserNotFound) {
		unauthorized(w) // the account was deleted while this request ran
		return
	}
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	h.logger.Info("password changed", "request_id", RequestID(r.Context()), "user_id", s.user.ID, "other_sessions_ended", ended)
	writeJSON(w, http.StatusOK, sessionsEndedResponse{SessionsEnded: ended})
}
