package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// request sends one HTTP request and returns the status, headers and full body.
// It always closes the response body.
func request(t *testing.T, client *http.Client, method, url, body string) (int, http.Header, []byte) {
	t.Helper()

	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s body: %v", method, url, err)
	}
	return resp.StatusCode, resp.Header, data
}

func TestServerEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addrCh := make(chan string, 1)
	runErr := make(chan error, 1)
	go func() {
		logger := slog.New(slog.DiscardHandler)
		runErr <- run(ctx, logger, "127.0.0.1:0", func(addr string) { addrCh <- addr })
	}()

	// Wait for the server to be listening, fail to start, or take too long.
	var base string
	select {
	case addr := <-addrCh:
		base = "http://" + addr
	case err := <-runErr:
		t.Fatalf("server failed to start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start within 5s")
	}

	// The default http.Client has no timeout. Never use it against a real server.
	client := &http.Client{Timeout: 5 * time.Second}

	// Create.
	status, header, body := request(t, client, http.MethodPost, base+"/api/v1/jobs", `{"title":"e2e job"}`)
	if status != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", status, body)
	}
	if header.Get("X-Request-ID") == "" {
		t.Error("create: missing X-Request-ID header")
	}
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create body %s: %v", body, err)
	}
	if created.Status != "pending" {
		t.Errorf("create: status field %q, want pending", created.Status)
	}

	// Get.
	jobURL := base + "/api/v1/jobs/" + created.ID
	if status, _, body := request(t, client, http.MethodGet, jobURL, ""); status != http.StatusOK {
		t.Fatalf("get: status %d, body %s", status, body)
	}

	// Delete.
	if status, _, body := request(t, client, http.MethodDelete, jobURL, ""); status != http.StatusNoContent {
		t.Fatalf("delete: status %d, body %s", status, body)
	}

	// Get again: gone, and the error is JSON.
	status, header, body = request(t, client, http.MethodGet, jobURL, "")
	if status != http.StatusNotFound {
		t.Fatalf("get after delete: status %d, body %s", status, body)
	}
	if ct := header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("404 Content-Type = %q, want application/json", ct)
	}

	// Shut down and check that run returns cleanly.
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run returned error after shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("server did not shut down within 15s")
	}
}

func TestRunFailsWhenPortIsTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	err = run(context.Background(), slog.New(slog.DiscardHandler), ln.Addr().String(), nil)
	if err == nil {
		t.Fatal("run succeeded on a port that is already taken, want an error")
	}
}
