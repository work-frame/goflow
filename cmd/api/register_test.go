package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/database"
)

// deleteUser removes a test user so the shared test database stays clean.
// There is no delete endpoint for users, so this goes straight to SQL.
func deleteUser(t *testing.T, dbURL, email string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	db, err := database.Open(ctx, dbURL)
	if err != nil {
		t.Errorf("cleanup: open database: %v", err)
		return
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "DELETE FROM users WHERE email = $1", email); err != nil {
		t.Errorf("cleanup: delete user: %v", err)
	}
}

func TestRegisterEndToEnd(t *testing.T) {
	dbURL := testDatabaseURL(t)
	base, _ := startServer(t, dbURL)
	client := &http.Client{Timeout: 5 * time.Second}

	b := make([]byte, 6)
	rand.Read(b)
	email := "e2e-" + hex.EncodeToString(b) + "@example.com"
	t.Cleanup(func() { deleteUser(t, dbURL, email) })

	register := func(email string) (int, []byte) {
		status, _, body := request(t, client, http.MethodPost, base+"/api/v1/auth/register",
			`{"email":"`+email+`","password":"correct horse battery"}`)
		return status, body
	}

	if status, body := register(email); status != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", status, body)
	}
	// Same address in capitals: migration 0002, UserStore and the handler
	// must all agree it is the same account.
	if status, body := register(strings.ToUpper(email)); status != http.StatusConflict {
		t.Fatalf("register again: status %d, want 409, body %s", status, body)
	}
}
