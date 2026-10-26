package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("GOFLOW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("GOFLOW_TEST_DATABASE_URL not set; skipping test that needs PostgreSQL")
	}
	return url
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// startServer runs the real server on a random port and returns its base URL
// and a stop function. stop is safe to call more than once and also runs at cleanup.
func startServer(t *testing.T, dbURL string) (base string, stop func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan string, 1)
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, discardLogger(), "127.0.0.1:0", dbURL, func(addr string) { addrCh <- addr })
	}()

	select {
	case addr := <-addrCh:
		base = "http://" + addr
	case err := <-runErr:
		cancel()
		t.Fatalf("server failed to start: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("server did not start within 10s")
	}

	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-runErr:
				if err != nil {
					t.Errorf("run returned error after shutdown: %v", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("server did not shut down within 15s")
			}
		})
	}
	t.Cleanup(stop)
	return base, stop
}

// request sends one HTTP request and returns the status, headers and full body.
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

func createJob(t *testing.T, client *http.Client, base, title string) string {
	t.Helper()
	status, _, body := request(t, client, http.MethodPost, base+"/api/v1/jobs", `{"title":"`+title+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", status, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create body %s: %v", body, err)
	}
	return created.ID
}

func TestServerEndToEnd(t *testing.T) {
	base, _ := startServer(t, testDatabaseURL(t))
	client := &http.Client{Timeout: 5 * time.Second}

	if status, _, body := request(t, client, http.MethodGet, base+"/readyz", ""); status != http.StatusOK {
		t.Fatalf("readyz: status %d, body %s", status, body)
	}

	id := createJob(t, client, base, "e2e job")
	jobURL := base + "/api/v1/jobs/" + id

	if status, _, body := request(t, client, http.MethodGet, jobURL, ""); status != http.StatusOK {
		t.Fatalf("get: status %d, body %s", status, body)
	}
	if status, _, body := request(t, client, http.MethodDelete, jobURL, ""); status != http.StatusNoContent {
		t.Fatalf("delete: status %d, body %s", status, body)
	}
	if status, _, body := request(t, client, http.MethodGet, jobURL, ""); status != http.StatusNotFound {
		t.Fatalf("get after delete: status %d, body %s", status, body)
	}
}

func TestJobsSurviveRestart(t *testing.T) {
	dbURL := testDatabaseURL(t)
	client := &http.Client{Timeout: 5 * time.Second}

	base, stop := startServer(t, dbURL)
	id := createJob(t, client, base, "survive a restart")
	stop()

	// A brand-new server: new process state, new port, same database.
	base, _ = startServer(t, dbURL)
	jobURL := base + "/api/v1/jobs/" + id

	if status, _, body := request(t, client, http.MethodGet, jobURL, ""); status != http.StatusOK {
		t.Fatalf("job lost after restart: status %d, body %s", status, body)
	}

	// Leave the shared test database as we found it.
	if status, _, body := request(t, client, http.MethodDelete, jobURL, ""); status != http.StatusNoContent {
		t.Fatalf("cleanup delete: status %d, body %s", status, body)
	}
}

func TestRunRequiresDatabaseURL(t *testing.T) {
	err := run(context.Background(), discardLogger(), "127.0.0.1:0", "", nil)
	if err == nil || !strings.Contains(err.Error(), "GOFLOW_DATABASE_URL") {
		t.Errorf("err = %v, want an error naming GOFLOW_DATABASE_URL", err)
	}
}

func TestRunFailsWhenPortIsTaken(t *testing.T) {
	dbURL := testDatabaseURL(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	err = run(context.Background(), discardLogger(), ln.Addr().String(), dbURL, nil)
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("err = %v, want address already in use", err)
	}
}
