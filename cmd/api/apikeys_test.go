package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/work-frame/goflow/internal/auth"
	"github.com/work-frame/goflow/internal/database"
)

// The password signUp registers every e2e user with.
const e2ePassword = "correct horse battery"

// makeKey creates an API key through the API with the session token and
// returns its secret and ID.
func makeKey(t *testing.T, client *http.Client, base, session string, scopes []auth.Scope, days int) (secret, id string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": "e2e key", "scopes": scopes, "expires_in_days": days, "password": e2ePassword})
	status, header, resp := requestAs(t, client, http.MethodPost, base+"/api/v1/keys", session, string(body))
	if status != http.StatusCreated {
		t.Fatalf("create key: status %d, body %s", status, resp)
	}
	if header.Get("Cache-Control") != "no-store" {
		t.Errorf("key creation is cacheable: Cache-Control %q", header.Get("Cache-Control"))
	}
	var created struct {
		Secret string `json:"secret"`
		Key    struct {
			ID string `json:"id"`
		} `json:"key"`
	}
	if err := json.Unmarshal(resp, &created); err != nil {
		t.Fatal(err)
	}
	if !auth.ValidKeyFormat(created.Secret) {
		t.Fatalf("secret %.8s... isn't a gfk_ key", created.Secret)
	}
	return created.Secret, created.Key.ID
}

var (
	createOnly = []auth.Scope{auth.ScopeJobsCreate}
	readOnly   = []auth.Scope{auth.ScopeJobsRead}
	bothScopes = []auth.Scope{auth.ScopeJobsCreate, auth.ScopeJobsRead}
)

// keyRoute is one route and what each kind of key gets from it.
type keyRoute struct {
	method, path, body string
	create, read, both int // statuses for a create-only, a read-only and a two-scope key
}

// keyRoutes is every route that checks who is calling. A route a key may
// use answers 2xx to a key with the scope and 403 to one without; every
// other route answers 403 to every key. TestEveryRouteIsInTheKeyTable
// keeps it complete.
func keyRoutes(jobID, keyID string) []keyRoute {
	const no = http.StatusForbidden
	return []keyRoute{
		{"POST", "/api/v1/jobs", `{"title":"made by a key"}`, 201, no, 201},
		{"GET", "/api/v1/jobs", "", no, 200, 200},
		{"GET", "/api/v1/jobs/" + jobID, "", no, 200, 200},
		{"GET", "/api/v1/jobs/" + jobID + "/attempts", "", no, 200, 200},
		{"DELETE", "/api/v1/jobs/" + jobID, "", no, no, no},
		{"POST", "/api/v1/jobs/" + jobID + "/retry", "", no, no, no},
		{"POST", "/api/v1/webhook-secret", "", no, no, no},
		{"POST", "/api/v1/events/ticket", "", no, no, no},
		{"POST", "/api/v1/auth/logout", "", no, no, no},
		{"POST", "/api/v1/auth/logout-all", "", no, no, no},
		{"POST", "/api/v1/auth/password", `{"current_password":"` + e2ePassword + `","new_password":"a new password for the key"}`, no, no, no},
		{"GET", "/api/v1/auth/me", "", no, no, no},
		{"POST", "/api/v1/keys", `{"name":"k","scopes":["jobs:create"],"expires_in_days":30,"password":"` + e2ePassword + `"}`, no, no, no},
		{"GET", "/api/v1/keys", "", no, no, no},
		{"DELETE", "/api/v1/keys/" + keyID, "", no, no, no},
		{"POST", "/api/v1/keys/revoke-all", "", no, no, no},
	}
}

// Scope decides what a key may do, deny by default (condition 1): every
// route, every kind of key. Case 73 breaks this one.
func TestAPIKeyScopesOnEveryRoute(t *testing.T) {
	parallelTest(t)
	dbURL := testDatabaseURL(t)
	base, _ := startServer(t, dbURL)
	client := testClient(t, 10*time.Second)
	session := signUp(t, client, base, dbURL)
	jobID := createJob(t, client, base, session, "the session's job")

	keys := map[string]string{}
	var keyID string
	for name, scopes := range map[string][]auth.Scope{"create": createOnly, "read": readOnly, "both": bothScopes} {
		keys[name], keyID = makeKey(t, client, base, session, scopes, 30)
	}
	for _, rt := range keyRoutes(jobID, keyID) {
		for _, kind := range []string{"create", "read", "both"} {
			want := map[string]int{"create": rt.create, "read": rt.read, "both": rt.both}[kind]
			status, _, body := requestAs(t, client, rt.method, base+rt.path, keys[kind], rt.body)
			if status != want {
				t.Errorf("%s %s with a %s key: status %d, want %d (%s)", rt.method, rt.path, kind, status, want, body)
			}
			keyRoute := rt.create != http.StatusForbidden || rt.read != http.StatusForbidden
			if status == http.StatusForbidden && keyRoute && !strings.Contains(string(body), "scope") {
				t.Errorf("%s %s: the 403 doesn't name the missing scope: %s", rt.method, rt.path, body)
			}
		}
	}
	// The forbidden calls changed nothing: the session works, the job is there.
	if status, _, _ := requestAs(t, client, "GET", base+"/api/v1/jobs/"+jobID, session, ""); status != http.StatusOK {
		t.Errorf("after the walk, the session's job reads %d, want 200", status)
	}
	_, _, body := requestAs(t, client, "GET", base+"/api/v1/jobs/"+jobID+"/attempts", keys["create"], "")
	if !strings.Contains(string(body), "jobs:read") {
		t.Errorf("a create-only key reading attempts got %s; want the jobs:read scope named", body)
	}
}

// Every route in internal/api/api.go is in keyRoutes, or is public; and
// only the four key routes use requireAuth. A new route can't silently
// open to keys, or escape this test.
func TestEveryRouteIsInTheKeyTable(t *testing.T) {
	parallelTest(t)
	src := readRepoFile(t, "internal/api/api.go")
	re := regexp.MustCompile(`mux\.HandleFunc\("(\w+) ([^"]+)", (.+)\)\n`)
	public := map[string]bool{"GET /healthz": true, "GET /readyz": true, "POST /api/v1/auth/register": true,
		"POST /api/v1/auth/login": true, "GET /api/v1/events": true}
	table := map[string]keyRoute{}
	for _, rt := range keyRoutes("{id}", "{id}") {
		table[rt.method+" "+rt.path] = rt
	}
	found := re.FindAllStringSubmatch(src, -1)
	if len(found) < 20 {
		t.Fatalf("found only %d routes in api.go; did the registration style change?", len(found))
	}
	for _, m := range found {
		route, wrap := m[1]+" "+m[2], m[3]
		if public[route] {
			continue
		}
		rt, ok := table[route]
		if !ok {
			t.Errorf("%s isn't in keyRoutes: add it, with what each kind of key gets", route)
			continue
		}
		keyRoute := rt.create != http.StatusForbidden || rt.read != http.StatusForbidden
		switch {
		case keyRoute && !strings.HasPrefix(wrap, "h.requireAuth(auth.Scope"):
			t.Errorf("%s is open to keys in the table but isn't wrapped in requireAuth: %s", route, wrap)
		case !keyRoute && !strings.HasPrefix(wrap, "h.requireSession("):
			t.Errorf("%s must take a session only: %s", route, wrap)
		}
	}
	if n := strings.Count(src, "h.requireAuth("); n != 4 {
		t.Errorf("requireAuth is used %d times in api.go, want the 4 key routes", n)
	}
}

// Ownership decides whose data a key reaches, separately from scope
// (condition 2): a key with every scope still sees only its owner's jobs.
func TestAPIKeyOwnership(t *testing.T) {
	parallelTest(t)
	dbURL := testDatabaseURL(t)
	base, _ := startServer(t, dbURL)
	client := testClient(t, 10*time.Second)
	alice, bob := signUp(t, client, base, dbURL), signUp(t, client, base, dbURL)
	aliceJob := createJob(t, client, base, alice, "alice's")
	bobJob := createJob(t, client, base, bob, "bob's")
	aliceKey, aliceKeyID := makeKey(t, client, base, alice, bothScopes, 90)

	for _, path := range []string{"/api/v1/jobs/" + bobJob, "/api/v1/jobs/" + bobJob + "/attempts"} {
		if status, _, body := requestAs(t, client, "GET", base+path, aliceKey, ""); status != http.StatusNotFound {
			t.Errorf("alice's key on bob's %s: %d %s, want 404", path, status, body)
		}
	}
	keyJob := createJob(t, client, base, aliceKey, "made with alice's key")
	_, _, body := requestAs(t, client, "GET", base+"/api/v1/jobs", aliceKey, "")
	if !strings.Contains(string(body), aliceJob) || !strings.Contains(string(body), keyJob) || strings.Contains(string(body), bobJob) {
		t.Errorf("alice's key lists %s; want alice's two jobs and not bob's", body)
	}
	// The job made with the key belongs to alice and names the key.
	status, _, body := requestAs(t, client, "GET", base+"/api/v1/jobs/"+keyJob, alice, "")
	if status != http.StatusOK || !strings.Contains(string(body), `"api_key_id":"`+aliceKeyID+`"`) {
		t.Errorf("alice's session reads the key's job: %d %s; want it, with api_key_id", status, body)
	}
	if status, _, _ := requestAs(t, client, "GET", base+"/api/v1/jobs/"+keyJob, bob, ""); status != http.StatusNotFound {
		t.Errorf("bob reads the job alice's key made: %d", status)
	}
	if status, _, _ := requestAs(t, client, "DELETE", base+"/api/v1/keys/"+aliceKeyID, bob, ""); status != http.StatusNotFound {
		t.Errorf("bob revoking alice's key: %d, want 404", status)
	}
	if status, _, _ := requestAs(t, client, "GET", base+"/api/v1/jobs", aliceKey, ""); status != http.StatusOK {
		t.Error("bob's revoke attempt stopped alice's key")
	}
}

// Making, listing, revoking and expiring keys.
func TestAPIKeyLifecycle(t *testing.T) {
	parallelTest(t)
	dbURL := testDatabaseURL(t)
	base, _ := startServer(t, dbURL)
	client := testClient(t, 10*time.Second)

	// Re-authentication (K5), the request's rules, and the limit on tries:
	// every try with a well-formed body counts, right or wrong, so the 6th
	// in 15 minutes is refused before the password is even checked.
	guesser := signUp(t, client, base, dbURL)
	good := `"password":"` + e2ePassword + `"`
	for i, c := range []struct {
		body string
		want int
	}{
		{`{"name":"k","scopes":["jobs:create"],"expires_in_days":90,` + good + `,"x":1}`, 400}, // not counted: unreadable
		{`{"name":"k","scopes":["jobs:create"],"expires_in_days":90,"password":"not the password!"}`, 403},
		{`{"name":"k","scopes":["jobs:create"],"expires_in_days":90}`, 403},
		{`{"name":"","scopes":["jobs:create"],"expires_in_days":90,` + good + `}`, 400},
		{`{"name":"k","scopes":["jobs:delete"],"expires_in_days":90,` + good + `}`, 400},
		{`{"name":"k","scopes":["jobs:create"],"expires_in_days":0,` + good + `}`, 400},
		{`{"name":"k","scopes":["jobs:create"],"expires_in_days":90,` + good + `}`, 429},
	} {
		if status, _, resp := requestAs(t, client, "POST", base+"/api/v1/keys", guesser, c.body); status != c.want {
			t.Errorf("try %d %s: %d %s, want %d", i+1, c.body, status, resp, c.want)
		}
	}
	if status, _, _ := request(t, client, "POST", base+"/api/v1/keys", `{"name":"k"}`); status != http.StatusUnauthorized {
		t.Errorf("create without a session: %d, want 401", status)
	}

	session := signUp(t, client, base, dbURL)
	key, id := makeKey(t, client, base, session, createOnly, 30)
	createJob(t, client, base, key, "first use")

	// The list: status, hint, last use; never the hash or the secret.
	_, _, list := requestAs(t, client, "GET", base+"/api/v1/keys", session, "")
	var listed struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(list, &listed); err != nil || len(listed.Keys) != 1 {
		t.Fatalf("list %s: %v", list, err)
	}
	k := listed.Keys[0]
	if k["id"] != id || k["status"] != "active" || k["hint"] != key[43:] || k["last_used_at"] == nil {
		t.Errorf("listed key %v: want active, hint %s, a last use", k, key[43:])
	}
	for _, field := range []string{"key_hash", "secret", "KeyHash", "hash"} {
		if _, ok := k[field]; ok {
			t.Errorf("the list has a %q field", field)
		}
	}
	if strings.Contains(string(list), key) || strings.Contains(string(list), hex.EncodeToString(auth.HashToken(key))) {
		t.Error("the list contains the secret or its hash")
	}

	// Revoked: refused on the very next request; revoking again is fine.
	for i := 0; i < 2; i++ {
		if status, _, body := requestAs(t, client, "DELETE", base+"/api/v1/keys/"+id, session, ""); status != http.StatusNoContent {
			t.Fatalf("revoke %d: %d %s", i+1, status, body)
		}
	}
	if status, _, _ := requestAs(t, client, "POST", base+"/api/v1/jobs", key, `{"title":"x"}`); status != http.StatusUnauthorized {
		t.Errorf("revoked key: %d, want 401", status)
	}
	// On a session-only route too: 401, not 403, for a key that doesn't work.
	if status, _, _ := requestAs(t, client, "GET", base+"/api/v1/auth/me", key, ""); status != http.StatusUnauthorized {
		t.Errorf("revoked key on a session route: %d, want 401", status)
	}

	// Expired, on PostgreSQL's clock: refused at once.
	expiring, expID := makeKey(t, client, base, session, createOnly, 30)
	db, err := database.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE api_keys SET created_at = now() - interval '31 days', expires_at = now() - interval '1 second' WHERE id = $1`, expID); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := requestAs(t, client, "POST", base+"/api/v1/jobs", expiring, `{"title":"x"}`); status != http.StatusUnauthorized {
		t.Errorf("expired key: %d, want 401", status)
	}
	_, _, list = requestAs(t, client, "GET", base+"/api/v1/keys", session, "")
	for _, want := range []string{`"status":"revoked"`, `"status":"expired"`} {
		if !strings.Contains(string(list), want) {
			t.Errorf("the list lost a key's history: no %s in %s", want, list)
		}
	}

	// Malformed and made-up keys: 401, like a bad session.
	for _, bad := range []string{"gfk_", "gfk_short", key[:46] + "!", "gfk_" + strings.Repeat("A", 43)} {
		if status, _, _ := requestAs(t, client, "GET", base+"/api/v1/jobs", bad, ""); status != http.StatusUnauthorized {
			t.Errorf("key %q: %d, want 401", bad, status)
		}
	}

	// At most 10 active (9 made straight in the database: the API allows
	// 5 tries in 15 minutes), then revoke-all ends them.
	var userID string
	if err := db.QueryRow(`SELECT user_id FROM api_keys WHERE id = $1`, id).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	keys := database.NewAPIKeyStore(db)
	for i := 0; i < auth.MaxActiveKeys-1; i++ {
		_, k, _ := auth.NewAPIKey(userID, "k", readOnly, 90, time.Now())
		if err := keys.Create(context.Background(), k); err != nil {
			t.Fatal(err)
		}
	}
	makeKey(t, client, base, session, readOnly, 90) // the 10th
	status, _, body := requestAs(t, client, "POST", base+"/api/v1/keys", session,
		`{"name":"k","scopes":["jobs:create"],"expires_in_days":90,`+good+`}`)
	if status != http.StatusConflict {
		t.Errorf("11th active key: %d %s, want 409", status, body)
	}
	status, _, body = requestAs(t, client, "POST", base+"/api/v1/keys/revoke-all", session, "")
	if status != http.StatusOK || !strings.Contains(string(body), `"revoked":10`) {
		t.Errorf("revoke-all: %d %s, want 10 revoked", status, body)
	}
}

// A key's 60 a minute, and the account's 120 a minute across all its
// keys, hold when the requests arrive together (condition 5): three keys
// send 50 each at once, 150 in all, and exactly 120 get through. The
// counters are in Redis, as in production.
func TestAPIKeyRateLimitsHoldUnderConcurrency(t *testing.T) {
	parallelTest(t)
	dbURL := testDatabaseURL(t)
	redisURL := testRedisURL(t)
	cfg := testConfig("127.0.0.1:0", dbURL)
	cfg.RedisURL = redisURL
	base, _ := startServerWith(t, cfg)
	client := testClient(t, 30*time.Second)
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { rdb.Close() })
	t.Cleanup(func() {
		ctx := context.Background()
		if keys, _ := rdb.Keys(ctx, cfg.RateLimitPrefix+"*").Result(); len(keys) > 0 {
			rdb.Del(ctx, keys...)
		}
	})

	session := signUp(t, client, base, dbURL)
	var keys []string
	for i := 0; i < 3; i++ {
		k, _ := makeKey(t, client, base, session, createOnly, 30)
		keys = append(keys, k)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[int]int{}
	start := make(chan struct{})
	for _, k := range keys {
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				req, _ := http.NewRequest("POST", base+"/api/v1/jobs", strings.NewReader(`{"title":"burst"}`))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+k)
				resp, err := client.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				resp.Body.Close()
				mu.Lock()
				counts[resp.StatusCode]++
				mu.Unlock()
			}()
		}
	}
	close(start)
	wg.Wait()
	if counts[201] != 120 || counts[429] != 30 {
		t.Fatalf("150 creates at once through 3 keys: %v; want exactly 120 created and 30 refused", counts)
	}
	// Session creates don't count: the dashboard isn't a program.
	createJob(t, client, base, session, "from the dashboard")

	// One key alone: 60, then 429 with Retry-After.
	other := signUp(t, client, base, dbURL)
	key, _ := makeKey(t, client, base, other, createOnly, 30)
	for i := 1; i <= 60; i++ {
		if status, _, body := requestAs(t, client, "POST", base+"/api/v1/jobs", key, `{"title":"one key"}`); status != http.StatusCreated {
			t.Fatalf("create %d: %d %s", i, status, body)
		}
	}
	status, header, body := requestAs(t, client, "POST", base+"/api/v1/jobs", key, `{"title":"one key"}`)
	if status != http.StatusTooManyRequests || header.Get("Retry-After") == "" {
		t.Errorf("61st create on one key: %d %s, Retry-After %q", status, body, header.Get("Retry-After"))
	}
}

// lockedBuffer is a bytes.Buffer the server's goroutines can log into.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The secret never reaches the log (condition 6): every key flow runs
// against a server whose log the test reads.
func TestAPIKeysNeverReachTheLog(t *testing.T) {
	parallelTest(t)
	dbURL := testDatabaseURL(t)
	var logs lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	base, stop := startServerLogging(t, testConfig("127.0.0.1:0", dbURL), logger)
	client := testClient(t, 10*time.Second)
	session := signUp(t, client, base, dbURL)

	key, id := makeKey(t, client, base, session, createOnly, 30)
	requestAs(t, client, "POST", base+"/api/v1/jobs", key, `{"title":"used"}`)
	requestAs(t, client, "GET", base+"/api/v1/jobs", key, "")             // missing scope
	requestAs(t, client, "GET", base+"/api/v1/auth/me", key, "")          // session-only route
	requestAs(t, client, "POST", base+"/api/v1/jobs", key[:46]+"!", `{}`) // malformed
	requestAs(t, client, "POST", base+"/api/v1/keys", session, `{"name":"k","scopes":["jobs:create"],"expires_in_days":30,"password":"wrong password!!"}`)
	requestAs(t, client, "DELETE", base+"/api/v1/keys/"+id, session, "")
	requestAs(t, client, "POST", base+"/api/v1/jobs", key, `{"title":"revoked"}`)
	stop()

	got := logs.String()
	if !strings.Contains(got, "api key refused") || !strings.Contains(got, id) {
		t.Fatalf("the log doesn't show the refusal by key ID; is the logger wired? %.500s", got)
	}
	for name, secret := range map[string]string{
		"the key": key, "its random part": key[4:], "its first half": key[:24],
		"its hash": hex.EncodeToString(auth.HashToken(key)), "the password": e2ePassword,
	} {
		if strings.Contains(got, secret) {
			t.Errorf("the log contains %s", name)
		}
	}
}
