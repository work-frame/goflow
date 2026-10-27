package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/work-frame/goflow/internal/auth"
)

const (
	logoutAllPath = "/api/v1/auth/logout-all"
	passwordPath  = "/api/v1/auth/password"
	newPassword   = "a brand new password"
)

// postAs sends a JSON body with "Authorization: Bearer <token>".
func postAs(t *testing.T, h http.Handler, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func changePasswordBody(current, next string) string {
	return `{"current_password":"` + current + `","new_password":"` + next + `"}`
}

func TestLogoutAll(t *testing.T) {
	f := newAuthFixture(t, testHasher, discardLogger())
	laptop, phone, tablet := f.mustLogin(t), f.mustLogin(t), f.mustLogin(t)

	rec := postAs(t, f.router, logoutAllPath, phone, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("logout-all: status %d, body %s", rec.Code, rec.Body)
	}
	if got := decode[map[string]int](t, rec)["sessions_ended"]; got != 3 {
		t.Errorf("sessions_ended = %d, want 3", got)
	}
	for name, token := range map[string]string{"laptop": laptop, "phone": phone, "tablet": tablet} {
		if rec := withToken(t, f.router, http.MethodGet, mePath, token); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s still signed in after logout-all: status %d", name, rec.Code)
		}
	}
}

func TestChangePassword(t *testing.T) {
	f := newAuthFixture(t, testHasher, discardLogger())
	laptop, stolenTablet := f.mustLogin(t), f.mustLogin(t)

	rec := postAs(t, f.router, passwordPath, laptop, changePasswordBody(password, newPassword))
	if rec.Code != http.StatusOK {
		t.Fatalf("change password: status %d, body %s", rec.Code, rec.Body)
	}
	if got := decode[map[string]int](t, rec)["sessions_ended"]; got != 1 {
		t.Errorf("sessions_ended = %d, want 1 (the tablet)", got)
	}

	if rec := withToken(t, f.router, http.MethodGet, mePath, laptop); rec.Code != http.StatusOK {
		t.Errorf("the session that changed the password was ended: status %d", rec.Code)
	}
	if rec := withToken(t, f.router, http.MethodGet, mePath, stolenTablet); rec.Code != http.StatusUnauthorized {
		t.Errorf("another session survived the password change: status %d", rec.Code)
	}
	if rec := f.login(t, "dan@example.com", password); rec.Code != http.StatusUnauthorized {
		t.Errorf("old password still works: status %d", rec.Code)
	}
	if rec := f.login(t, "dan@example.com", newPassword); rec.Code != http.StatusOK {
		t.Errorf("new password doesn't work: status %d", rec.Code)
	}
}

// A stolen session alone must not be enough to take over the account.
func TestChangePasswordNeedsTheCurrentPassword(t *testing.T) {
	f := newAuthFixture(t, testHasher, discardLogger())
	token, other := f.mustLogin(t), f.mustLogin(t)

	rec := postAs(t, f.router, passwordPath, token, changePasswordBody("wrong horse battery", newPassword))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body %s", rec.Code, rec.Body)
	}
	if rec := f.login(t, "dan@example.com", password); rec.Code != http.StatusOK {
		t.Errorf("password changed although the current one was wrong: login status %d", rec.Code)
	}
	if rec := withToken(t, f.router, http.MethodGet, mePath, other); rec.Code != http.StatusOK {
		t.Errorf("a refused change still ended other sessions: status %d", rec.Code)
	}
}

func TestChangePasswordValidatesTheNewOne(t *testing.T) {
	f := newAuthFixture(t, testHasher, discardLogger())
	token := f.mustLogin(t)

	rec := postAs(t, f.router, passwordPath, token, changePasswordBody(password, "short"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if msg := decode[map[string]string](t, rec)["error"]; msg != "new_password must be at least 12 characters" {
		t.Errorf("error = %q", msg)
	}
}

func TestAccountRoutesNeedASession(t *testing.T) {
	f := newAuthFixture(t, testHasher, discardLogger())
	for _, path := range []string{logoutAllPath, passwordPath} {
		rec := do(t, f.router, http.MethodPost, path, changePasswordBody(password, newPassword))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s without a token: status %d, want 401", path, rec.Code)
		}
	}
}

func TestChangePasswordNeverLogsPasswords(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := newAuthFixture(t, testHasher, logger)
	token := f.mustLogin(t)

	postAs(t, f.router, passwordPath, token, changePasswordBody("wrong horse battery", newPassword))
	postAs(t, f.router, passwordPath, token, changePasswordBody(password, newPassword))

	if !strings.Contains(logs.String(), "password changed") {
		t.Fatalf("expected a password-changed log line, got: %s", logs.String())
	}
	for _, secret := range []string{password, "wrong horse battery", newPassword, token} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain a secret %q", secret)
		}
	}
}

// Login brings every hash to the configured cost, down as well as up, so
// login timing is the same for every account.
func TestLoginRehashesDownToo(t *testing.T) {
	stronger := auth.Hasher{Cost: 6}
	f := newAuthFixture(t, stronger, discardLogger()) // dan hashed at cost 6
	router := NewRouter(Deps{Users: f.users, Sessions: f.sessions, Hasher: testHasher, Logger: discardLogger()})

	if rec := do(t, router, http.MethodPost, loginPath, `{"email":"dan@example.com","password":"`+password+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("login: status %d", rec.Code)
	}
	u := f.users.byEmail["dan@example.com"]
	if testHasher.NeedsRehash(u.PasswordHash) {
		t.Error("hash is still at the old cost after login")
	}
}
