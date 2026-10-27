package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/auth"
)

// signIn registers email in users and starts a session for it in sessions,
// returning the token. It skips HTTP: tests that use it are about jobs.
func signIn(users *memUsers, sessions *memSessions, email string) string {
	u, err := auth.NewUser(email, password, testHasher)
	if err != nil {
		panic(err)
	}
	if err := users.Create(context.Background(), u); err != nil {
		panic(err)
	}
	token, s := auth.NewSession(u.ID, time.Now())
	sessions.Create(context.Background(), s)
	return token
}

// asUser sends every request that has no Authorization header with token.
func asUser(h http.Handler, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		h.ServeHTTP(w, r)
	})
}

// signedIn returns the router for d with one signed-in user, so tests about
// jobs don't have to log in first. Tests about login build their own router.
func signedIn(d Deps) http.Handler {
	users := newMemUsers()
	sessions := newMemSessions(users)
	d.Users, d.Sessions = users, sessions
	if d.Hasher.Cost == 0 {
		d.Hasher = testHasher // keeps the background dummy hash cheap
	}
	token := signIn(users, sessions, "owner@example.com")
	return asUser(NewRouter(d), token)
}

// twoUsers is one router shared by alice and bob, each with their own token.
func twoUsers(t *testing.T) (router http.Handler, alice, bob string) {
	t.Helper()
	users := newMemUsers()
	sessions := newMemSessions(users)
	router = NewRouter(Deps{Jobs: newTestStore(), Users: users, Sessions: sessions, Hasher: testHasher, DB: okPinger, Logger: discardLogger()})
	return router, signIn(users, sessions, "alice@example.com"), signIn(users, sessions, "bob@example.com")
}

func TestJobRoutesNeedASession(t *testing.T) {
	router, _, _ := twoUsers(t)

	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/jobs", `{"title":"x"}`},
		{http.MethodGet, "/api/v1/jobs", ""},
		{http.MethodGet, "/api/v1/jobs/abc", ""},
		{http.MethodDelete, "/api/v1/jobs/abc", ""},
	} {
		rec := do(t, router, route.method, route.path, route.body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: status %d, want 401", route.method, route.path, rec.Code)
		}
	}
}

// Users only ever see their own jobs. Someone else's job answers exactly
// like a job that doesn't exist: 404, same message.
func TestJobsAreScopedToTheirOwner(t *testing.T) {
	router, alice, bob := twoUsers(t)
	asAlice, asBob := asUser(router, alice), asUser(router, bob)

	created := mustCreate(t, asAlice, "alice's job")
	path := "/api/v1/jobs/" + created.ID

	if rec := do(t, asAlice, http.MethodGet, path, ""); rec.Code != http.StatusOK {
		t.Fatalf("alice reading her own job: status %d", rec.Code)
	}

	missing := do(t, asBob, http.MethodGet, "/api/v1/jobs/does-not-exist", "")
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		rec := do(t, asBob, method, path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("bob %s alice's job: status %d, want 404", method, rec.Code)
		}
		if rec.Body.String() != missing.Body.String() {
			t.Errorf("bob %s alice's job: body %s differs from a missing job's %s", method, rec.Body, missing.Body)
		}
	}

	if rec := do(t, asAlice, http.MethodGet, path, ""); rec.Code != http.StatusOK {
		t.Errorf("alice's job is gone after bob's delete attempt: status %d", rec.Code)
	}

	list := decode[listJobsResponse](t, do(t, asBob, http.MethodGet, "/api/v1/jobs", ""))
	if len(list.Jobs) != 0 {
		t.Errorf("bob's list has %d jobs, want 0", len(list.Jobs))
	}
}

// The owner comes from the session. A body that names one is refused.
func TestCreateJobRefusesAnOwnerInTheBody(t *testing.T) {
	router, alice, _ := twoUsers(t)

	rec := do(t, asUser(router, alice), http.MethodPost, "/api/v1/jobs", `{"title":"x","user_id":"someone-else"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown field", rec.Code)
	}
}

// The response never names the owner: clients only ever see their own jobs.
func TestJobResponsesHideTheOwner(t *testing.T) {
	router, alice, _ := twoUsers(t)
	rec := do(t, asUser(router, alice), http.MethodPost, "/api/v1/jobs", `{"title":"x"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d", rec.Code)
	}
	if body := decode[map[string]any](t, rec); body["user_id"] != nil || body["UserID"] != nil {
		t.Errorf("response exposes the owner: %v", body)
	}
}
