package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// authed sends a request with a bearer token and returns the status code.
func authed(t *testing.T, client *http.Client, method, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestSessionEndToEnd runs register → login → me → logout → me against the
// real server and database: migration 0003, SessionStore and the wiring in run.
func TestSessionEndToEnd(t *testing.T) {
	dbURL := testDatabaseURL(t)
	base, _ := startServer(t, dbURL)
	client := &http.Client{Timeout: 5 * time.Second}

	b := make([]byte, 6)
	rand.Read(b)
	email := "e2e-" + hex.EncodeToString(b) + "@example.com"
	t.Cleanup(func() { deleteUser(t, dbURL, email) }) // also ends its sessions (ON DELETE CASCADE)

	creds := `{"email":"` + email + `","password":"correct horse battery"}`
	if status, _, body := request(t, client, http.MethodPost, base+"/api/v1/auth/register", creds); status != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", status, body)
	}

	status, _, body := request(t, client, http.MethodPost, base+"/api/v1/auth/login", creds)
	if status != http.StatusOK {
		t.Fatalf("login: status %d, body %s", status, body)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &login); err != nil {
		t.Fatalf("decode login body %s: %v", body, err)
	}

	if status := authed(t, client, http.MethodGet, base+"/api/v1/auth/me", login.Token); status != http.StatusOK {
		t.Fatalf("me: status %d, want 200", status)
	}
	if status := authed(t, client, http.MethodPost, base+"/api/v1/auth/logout", login.Token); status != http.StatusNoContent {
		t.Fatalf("logout: status %d, want 204", status)
	}
	if status := authed(t, client, http.MethodGet, base+"/api/v1/auth/me", login.Token); status != http.StatusUnauthorized {
		t.Fatalf("me after logout: status %d, want 401", status)
	}
}
