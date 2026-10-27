package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
		runErr <- run(ctx, discardLogger(), testConfig("127.0.0.1:0", dbURL), func(addr string) { addrCh <- addr })
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

// request sends one HTTP request without a session and returns the status,
// headers and full body.
func request(t *testing.T, client *http.Client, method, url, body string) (int, http.Header, []byte) {
	t.Helper()
	return requestAs(t, client, method, url, "", body)
}

// requestAs is request with "Authorization: Bearer <token>" when token isn't "".
func requestAs(t *testing.T, client *http.Client, method, url, token, body string) (int, http.Header, []byte) {
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
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
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

// signUp registers a new random user through the API, logs in, and returns
// the session token. The user, and with it every job and session they own,
// is deleted when the test ends.
func signUp(t *testing.T, client *http.Client, base, dbURL string) string {
	t.Helper()
	b := make([]byte, 6)
	rand.Read(b)
	email := "e2e-" + hex.EncodeToString(b) + "@example.com"
	t.Cleanup(func() { deleteUser(t, dbURL, email) })

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
	return login.Token
}

func createJob(t *testing.T, client *http.Client, base, token, title string) string {
	t.Helper()
	status, _, body := requestAs(t, client, http.MethodPost, base+"/api/v1/jobs", token, `{"title":"`+title+`"}`)
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
	dbURL := testDatabaseURL(t)
	base, _ := startServer(t, dbURL)
	client := &http.Client{Timeout: 5 * time.Second}

	if status, _, body := request(t, client, http.MethodGet, base+"/readyz", ""); status != http.StatusOK {
		t.Fatalf("readyz: status %d, body %s", status, body)
	}

	alice := signUp(t, client, base, dbURL)
	bob := signUp(t, client, base, dbURL)
	id := createJob(t, client, base, alice, "e2e job")
	jobURL := base + "/api/v1/jobs/" + id

	if status, _, body := request(t, client, http.MethodGet, jobURL, ""); status != http.StatusUnauthorized {
		t.Fatalf("get without a session: status %d, body %s", status, body)
	}
	if status, _, body := requestAs(t, client, http.MethodGet, jobURL, bob, ""); status != http.StatusNotFound {
		t.Fatalf("get as another user: status %d, body %s", status, body)
	}
	if status, _, body := requestAs(t, client, http.MethodGet, jobURL, alice, ""); status != http.StatusOK {
		t.Fatalf("get: status %d, body %s", status, body)
	}
	if status, _, body := requestAs(t, client, http.MethodDelete, jobURL, alice, ""); status != http.StatusNoContent {
		t.Fatalf("delete: status %d, body %s", status, body)
	}
	if status, _, body := requestAs(t, client, http.MethodGet, jobURL, alice, ""); status != http.StatusNotFound {
		t.Fatalf("get after delete: status %d, body %s", status, body)
	}
}

// Jobs and sessions both live in PostgreSQL, so a restarted server still
// knows the token and still has the job.
func TestJobsSurviveRestart(t *testing.T) {
	dbURL := testDatabaseURL(t)
	client := &http.Client{Timeout: 5 * time.Second}

	base, stop := startServer(t, dbURL)
	token := signUp(t, client, base, dbURL)
	id := createJob(t, client, base, token, "survive a restart")
	stop()

	// A brand-new server: new process state, new port, same database.
	base, _ = startServer(t, dbURL)
	jobURL := base + "/api/v1/jobs/" + id

	if status, _, body := requestAs(t, client, http.MethodGet, jobURL, token, ""); status != http.StatusOK {
		t.Fatalf("job or session lost after restart: status %d, body %s", status, body)
	}
}

func TestRunFailsWhenPortIsTaken(t *testing.T) {
	dbURL := testDatabaseURL(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	err = run(context.Background(), discardLogger(), testConfig(ln.Addr().String(), dbURL), nil)
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("err = %v, want address already in use", err)
	}
}
