package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testHook   = "https://receiver.example.com/hook"
	testJobID  = "0123456789abcdef0123456789abcdef"
	leakMarker = "INTERNAL-DETAIL-7f3a" // put in GoFlow's error bodies; must never reach the visitor
)

func testKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return "gfk_" + base64.RawURLEncoding.EncodeToString(b)
}

// lockedBuffer collects log output from the handler's goroutines.
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

// recorded is one request the fake GoFlow received.
type recorded struct {
	method, path string
	header       http.Header
	body         []byte
}

// fakeGoFlow records every request and answers with reply.
type fakeGoFlow struct {
	mu    sync.Mutex
	got   []recorded
	reply func(w http.ResponseWriter, r *http.Request)
}

func (f *fakeGoFlow) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.got = append(f.got, recorded{r.Method, r.URL.Path, r.Header.Clone(), body})
	reply := f.reply
	f.mu.Unlock()
	reply(w, r)
}

func (f *fakeGoFlow) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.got...)
}

func created(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	io.WriteString(w, `{"id":"`+testJobID+`","title":"contact form message","status":"pending"}`)
}

// harness is the app wired to a fake GoFlow, with its log captured.
type harness struct {
	t      *testing.T
	key    string
	fake   *fakeGoFlow
	log    *lockedBuffer
	server *httptest.Server // the contact form itself
	client *http.Client     // a browser that doesn't follow redirects
	pages  []string         // every response body and header the visitor got
}

func newHarness(t *testing.T, reply func(http.ResponseWriter, *http.Request)) *harness {
	t.Helper()
	fake := &fakeGoFlow{reply: reply}
	gf := httptest.NewServer(fake)
	t.Cleanup(gf.Close)
	key := testKey()
	cfg, err := loadConfig(env(map[string]string{"CONTACT_GOFLOW_KEY": key, "CONTACT_GOFLOW_URL": gf.URL, "CONTACT_WEBHOOK_URL": testHook}))
	if err != nil {
		t.Fatal(err)
	}
	log := &lockedBuffer{}
	a, err := newApp(cfg, slog.New(slog.NewTextHandler(log, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.routes())
	t.Cleanup(srv.Close)
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(client.CloseIdleConnections)
	return &harness{t: t, key: key, fake: fake, log: log, server: srv, client: client}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// do sends one request and keeps what the visitor saw.
func (h *harness) do(req *http.Request) (*http.Response, string) {
	h.t.Helper()
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var hdr strings.Builder
	resp.Header.Write(&hdr)
	h.pages = append(h.pages, hdr.String(), string(b))
	return resp, string(b)
}

var tokenField = regexp.MustCompile(`name="token" value="([^"]+)"`)

// formToken loads the form and returns its token.
func (h *harness) formToken() string {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/", nil)
	resp, body := h.do(req)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET /: %d", resp.StatusCode)
	}
	m := tokenField.FindStringSubmatch(body)
	if m == nil {
		h.t.Fatalf("no token in the form:\n%s", body)
	}
	return m[1]
}

// post sends the form. query, if not "", is added to the URL.
func (h *harness) post(form url.Values, query string) (*http.Response, string) {
	h.t.Helper()
	u := h.server.URL + "/send"
	if query != "" {
		u += "?" + query
	}
	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return h.do(req)
}

func goodForm(token string) url.Values {
	return url.Values{"token": {token}, "website": {""}, "name": {"Test Person"}, "email": {"test@example.com"}, "message": {"Hello from the test.\r\nSecond line."}}
}

// noLeaks checks the key never reached the visitor or the log, and that
// the log holds none of the submitted text.
func (h *harness) noLeaks(submitted ...string) {
	h.t.Helper()
	seen := strings.Join(h.pages, "\n")
	logs := h.log.String()
	for _, s := range []string{h.key, h.key[4:20], "Bearer"} {
		if strings.Contains(seen, s) {
			h.t.Errorf("the visitor was shown the key or its header (%.8s...)", s)
		}
		if strings.Contains(logs, s) {
			h.t.Errorf("the log holds the key or its header (%.8s...)", s)
		}
	}
	for _, s := range submitted {
		if s != "" && strings.Contains(logs, s) {
			h.t.Errorf("the log holds submitted text %q:\n%s", s, logs)
		}
	}
	if strings.Contains(seen, leakMarker) {
		h.t.Error("the visitor was shown GoFlow's own error text")
	}
}

// One good submission makes exactly one request, built entirely by the
// backend, and the visitor is told it was received.
func TestSendCreatesOneWebhookJob(t *testing.T) {
	h := newHarness(t, created)
	token := h.formToken()
	resp, _ := h.post(goodForm(token), "")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/thanks" {
		t.Fatalf("send: %d to %q, want 303 to /thanks", resp.StatusCode, resp.Header.Get("Location"))
	}
	got := h.fake.requests()
	if len(got) != 1 {
		t.Fatalf("GoFlow got %d requests, want 1", len(got))
	}
	r := got[0]
	id, _, _ := strings.Cut(token, ".")
	if r.method != http.MethodPost || r.path != "/api/v1/jobs" {
		t.Errorf("request %s %s, want POST /api/v1/jobs", r.method, r.path)
	}
	if r.header.Get("Authorization") != "Bearer "+h.key {
		t.Error("the Authorization header isn't exactly Bearer and the key")
	}
	if r.header.Get("Idempotency-Key") != "contactform-"+id {
		t.Errorf("Idempotency-Key %q, want contactform-%s", r.header.Get("Idempotency-Key"), id)
	}
	if r.header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type %q", r.header.Get("Content-Type"))
	}
	var job map[string]any
	if err := json.Unmarshal(r.body, &job); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"title": "contact form message", "type": "webhook", "max_attempts": float64(5),
		"webhook": map[string]any{"url": testHook, "body": map[string]any{
			"submission_id": id, "name": "Test Person", "email": "test@example.com", "message": "Hello from the test.\nSecond line.",
		}},
	}
	a, _ := json.Marshal(job)
	b, _ := json.Marshal(want)
	if !bytes.Equal(a, b) {
		t.Errorf("job\n got %s\nwant %s", a, b)
	}

	req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/thanks", nil)
	_, page := h.do(req)
	if !strings.Contains(page, "your message was received") || strings.Contains(strings.ToLower(page), "delivered.") {
		t.Errorf("the thanks page must say received, and not claim delivery:\n%s", page)
	}
	if !strings.Contains(h.log.String(), "job_id="+testJobID) {
		t.Errorf("the log doesn't name the job:\n%s", h.log.String())
	}
	h.noLeaks("Test Person", "test@example.com", "Hello from the test")
}

// The form may set three values and nothing else (requirement 1): extra
// fields and a query string asking for another URL, type, title or retry
// count change nothing in the job.
func TestTheVisitorChoosesOnlyNameEmailAndMessage(t *testing.T) {
	h := newHarness(t, created)
	form := goodForm(h.formToken())
	for k, v := range map[string]string{"url": "https://attacker.example/x", "webhook_url": "https://attacker.example/x", "type": "demo", "title": "pwned", "max_attempts": "10", "Authorization": "Bearer gfk_other"} {
		form.Set(k, v)
	}
	h.post(form, "url=https://attacker.example/q&type=demo&title=pwned")
	got := h.fake.requests()
	if len(got) != 1 {
		t.Fatalf("GoFlow got %d requests, want 1", len(got))
	}
	body := string(got[0].body)
	for _, bad := range []string{"attacker.example", "pwned", `"demo"`, "gfk_other", `"max_attempts":10`} {
		if strings.Contains(body, bad) {
			t.Errorf("the visitor's %q reached the job: %s", bad, body)
		}
	}
	if !strings.Contains(body, `"url":"`+testHook+`"`) || !strings.Contains(body, `"max_attempts":5`) {
		t.Errorf("the job isn't the backend's own: %s", body)
	}
}

// The same page sent twice carries the same Idempotency-Key; a new page
// gets a new token.
func TestResendingAPageReusesItsKey(t *testing.T) {
	h := newHarness(t, created)
	token := h.formToken()
	h.post(goodForm(token), "")
	h.post(goodForm(token), "")
	other := h.formToken()
	if other == token {
		t.Fatal("two pages got the same token")
	}
	h.post(goodForm(other), "")
	got := h.fake.requests()
	if len(got) != 3 {
		t.Fatalf("GoFlow got %d requests, want 3", len(got))
	}
	k := func(i int) string { return got[i].header.Get("Idempotency-Key") }
	if k(0) != k(1) {
		t.Errorf("a resend used a new key: %q then %q", k(0), k(1))
	}
	if k(2) == k(0) {
		t.Error("a new page reused the old page's key")
	}
}

// A token this process didn't sign sends nothing: missing, made up,
// edited, or signed by another process (a restart).
func TestTokensOnlyFromThisProcess(t *testing.T) {
	h := newHarness(t, created)
	real := h.formToken()
	id, mac, _ := strings.Cut(real, ".")
	otherID := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, tokenIDBytes))
	for name, token := range map[string]string{
		"missing":       "",
		"made up":       "hello",
		"id changed":    otherID + "." + mac,
		"mac cut":       id + "." + mac[:10],
		"other process": newTokens().issue(),
	} {
		resp, page := h.post(goodForm(token), "")
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, "expired") {
			t.Errorf("%s token: %d, want 400 and the expired message", name, resp.StatusCode)
		}
	}
	if n := len(h.fake.requests()); n != 0 {
		t.Errorf("GoFlow got %d requests from bad tokens, want 0", n)
	}
}

// The honeypot answers like a success and sends nothing (condition 6).
func TestHoneypotSendsNothing(t *testing.T) {
	h := newHarness(t, created)
	form := goodForm(h.formToken())
	form.Set("website", "https://spam.example")
	resp, _ := h.post(form, "")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/thanks" {
		t.Errorf("honeypot: %d to %q, want the same 303 to /thanks as a real send", resp.StatusCode, resp.Header.Get("Location"))
	}
	if n := len(h.fake.requests()); n != 0 {
		t.Errorf("GoFlow got %d requests from a filled honeypot, want 0", n)
	}
	if !strings.Contains(h.log.String(), "honeypot") {
		t.Error("the drop isn't logged")
	}
}

// Every invalid field sends nothing. Lengths are characters, not bytes
// (requirement 2): 100 four-byte characters are a valid name.
func TestValidationCountsCharacters(t *testing.T) {
	h := newHarness(t, created)
	long := func(s string, n int) string { return strings.Repeat(s, n) }
	bad := map[string][3]string{
		"empty name":         {"   ", "test@example.com", "hi"},
		"101 chars":          {long("é", 101), "test@example.com", "hi"},
		"control in name":    {"Test\x00Person", "test@example.com", "hi"},
		"bidi override":      {"Test\u202ePerson", "test@example.com", "hi"},
		"escape in message":  {"Test", "test@example.com", "hi\x1b[31m"},
		"invalid UTF-8":      {"Test\xff", "test@example.com", "hi"},
		"no at":              {"Test", "test.example.com", "hi"},
		"two ats":            {"Test", "a@b@example.com", "hi"},
		"space in email":     {"Test", "te st@example.com", "hi"},
		"no dot in domain":   {"Test", "test@localhost", "hi"},
		"empty message":      {"Test", "test@example.com", " \r\n "},
		"2001 chars":         {"Test", "test@example.com", long("😀", 2001)},
		"newline in name":    {"Test\nPerson", "test@example.com", "hi"},
		"255-character mail": {"Test", long("a", 243) + "@example.com", "hi"},
	}
	for name, f := range bad {
		form := goodForm(h.formToken())
		form.Set("name", f[0])
		form.Set("email", f[1])
		form.Set("message", f[2])
		resp, _ := h.post(form, "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
	if n := len(h.fake.requests()); n != 0 {
		t.Fatalf("GoFlow got %d requests from invalid forms, want 0", n)
	}

	form := goodForm(h.formToken())
	form.Set("name", long("😀", 100))
	form.Set("message", long("ọ", 1992)+"\n\ttabbed")
	if resp, page := h.post(form, ""); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("100 emoji and a 2,000-character message were refused: %d\n%s", resp.StatusCode, page)
	}
	form = goodForm(h.formToken())
	form.Set("message", long("😀", 2000))
	if resp, page := h.post(form, ""); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("a message of 2,000 four-byte characters was refused: %d\n%s", resp.StatusCode, page)
	}
	if n := len(h.fake.requests()); n != 2 {
		t.Errorf("GoFlow got %d requests, want 2", n)
	}
}

// What a visitor typed comes back as text, never as markup.
func TestRefusedFormComesBackEscaped(t *testing.T) {
	h := newHarness(t, created)
	form := goodForm(h.formToken())
	form.Set("name", `<script>alert(1)</script>`)
	form.Set("email", `"><img src=x>`)
	_, page := h.post(form, "")
	if strings.Contains(page, "<script>alert") || strings.Contains(page, "<img src=x>") {
		t.Errorf("the refused form put the visitor's text in as markup:\n%s", page)
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Errorf("the name isn't shown back, escaped:\n%s", page)
	}
}

// GoFlow's failures show the visitor fixed words, never GoFlow's text
// (condition 5); retryable ones keep the token so a resend reuses the key.
func TestGoFlowFailuresShowFixedMessages(t *testing.T) {
	cases := []struct {
		name       string
		reply      func(http.ResponseWriter, *http.Request)
		status     int
		says       string
		keepsToken bool
	}{
		{"422", errorReply(http.StatusUnprocessableEntity), http.StatusConflict, "different text", false},
		{"429", errorReply(http.StatusTooManyRequests), http.StatusTooManyRequests, "Too many messages", true},
		{"500", errorReply(http.StatusInternalServerError), http.StatusBadGateway, "couldn&#39;t be sent", true},
		{"403", errorReply(http.StatusForbidden), http.StatusBadGateway, "couldn&#39;t be sent", true},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere?"+leakMarker, http.StatusTemporaryRedirect)
		}, http.StatusBadGateway, "couldn&#39;t be sent", true},
		{"huge 201", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"title":"`+strings.Repeat("x", 1<<20)+`","id":"`+testJobID+`"}`)
		}, http.StatusSeeOther, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.reply)
			token := h.formToken()
			resp, page := h.post(goodForm(token), "")
			if resp.StatusCode != c.status {
				t.Fatalf("status %d, want %d:\n%s", resp.StatusCode, c.status, page)
			}
			if c.says != "" && !strings.Contains(page, c.says) {
				t.Errorf("page doesn't say %q:\n%s", c.says, page)
			}
			if c.keepsToken != strings.Contains(page, token) {
				t.Errorf("token kept for a retry: %v, want %v", !c.keepsToken, c.keepsToken)
			}
			if c.keepsToken && !strings.Contains(page, "Test Person") {
				t.Error("a retryable failure lost the visitor's text")
			}
			h.noLeaks("Test Person", "test@example.com", "Hello from the test")
		})
	}
}

func errorReply(status int) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, `{"error":"`+leakMarker+` api key gfk_... user 42"}`)
	}
}

// A huge success answer is read only up to the limit (requirement 3): the
// job counts as accepted, its ID unknown, and the page still works.
func TestGoFlowAnswersAreBounded(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		chunk := []byte(strings.Repeat(" ", 32<<10))
		// Valid JSON, 2 MiB, with the ID at the end: read whole, it would
		// give the ID; read up to the limit, it can't.
		io.WriteString(w, `{"pad":"`)
		for range 64 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		io.WriteString(w, `","id":"`+testJobID+`"}`)
	})
	resp, _ := h.post(goodForm(h.formToken()), "")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", resp.StatusCode)
	}
	if strings.Contains(h.log.String(), testJobID) || !strings.Contains(h.log.String(), "message accepted") {
		t.Errorf("an answer over the limit still gave a job ID:\n%s", h.log.String())
	}
}

// Connection refused and a GoFlow that never answers both show the
// retry message, keep the text, and log no request detail.
func TestGoFlowUnreachable(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + ln.Addr().String()
	ln.Close()

	stalled := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-stalled }))
	t.Cleanup(func() { close(stalled); slow.Close() })

	for name, base := range map[string]string{"refused": dead, "no answer": slow.URL} {
		t.Run(name, func(t *testing.T) {
			key := testKey()
			cfg, err := loadConfig(env(map[string]string{"CONTACT_GOFLOW_KEY": key, "CONTACT_GOFLOW_URL": base, "CONTACT_WEBHOOK_URL": testHook}))
			if err != nil {
				t.Fatal(err)
			}
			log := &lockedBuffer{}
			a, _ := newApp(cfg, slog.New(slog.NewTextHandler(log, nil)), &http.Client{Timeout: time.Hour})
			a.goflow.client.Timeout = 300 * time.Millisecond // newGoFlow sets 10 s; shortened for the test
			tok := a.tokens.issue()
			form := goodForm(tok)
			req := httptest.NewRequest(http.MethodPost, "/send", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			a.routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Test Person") {
				t.Errorf("status %d, want 502 with the text kept", rec.Code)
			}
			if !strings.Contains(log.String(), "goflow unreachable") || strings.Contains(log.String(), key) || strings.Contains(log.String(), base) {
				t.Errorf("log: %s", log.String())
			}
		})
	}
}

// A GoFlow answer that redirects elsewhere is not followed: the other
// server never sees the request or the key.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var hits int
	var mu sync.Mutex
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	t.Cleanup(elsewhere.Close)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusTemporaryRedirect)
	})
	h.post(goodForm(h.formToken()), "")
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Errorf("the redirect was followed: %d requests reached the other server", hits)
	}
}

// Bad settings are refused, and the error never repeats the value.
func TestSettingsAreChecked(t *testing.T) {
	key := testKey()
	base := map[string]string{"CONTACT_GOFLOW_KEY": key}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		return m
	}
	for name, m := range map[string]map[string]string{
		"no key":                 with("CONTACT_GOFLOW_KEY", ""),
		"short key":              with("CONTACT_GOFLOW_KEY", key[:20]),
		"session token":          with("CONTACT_GOFLOW_KEY", strings.Repeat("ab", 32)),
		"http remote GoFlow":     with("CONTACT_GOFLOW_URL", "http://goflow.example.com"),
		"GoFlow URL with user":   with("CONTACT_GOFLOW_URL", "https://u:"+key+"@goflow.example.com"),
		"GoFlow URL with query":  with("CONTACT_GOFLOW_URL", "https://goflow.example.com/?k=1"),
		"http webhook":           with("CONTACT_WEBHOOK_URL", "http://receiver.example.com/hook"),
		"webhook with user":      with("CONTACT_WEBHOOK_URL", "https://u:p@receiver.example.com/hook"),
		"public listen address":  with("CONTACT_ADDR", "0.0.0.0:8090"),
		"all interfaces":         with("CONTACT_ADDR", ":8090"),
		"LAN address":            with("CONTACT_ADDR", "192.168.1.5:8090"),
		"key pasted as the URL":  with("CONTACT_GOFLOW_URL", key),
		"key pasted as the hook": with("CONTACT_WEBHOOK_URL", key),
	} {
		_, err := loadConfig(env(m))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), key[4:]) {
			t.Errorf("%s: the error repeats the key: %v", name, err)
		}
	}
	cfg, err := loadConfig(env(base))
	if err != nil || cfg.GoFlowURL != defaultGoFlowURL || cfg.WebhookURL != defaultWebhookURL || cfg.Addr != defaultAddr {
		t.Errorf("defaults: %+v, %v", cfg, err)
	}
	for _, ok := range []string{"http://127.0.0.1:8080", "http://localhost:8080/", "https://goflow.example.com/"} {
		if _, err := loadConfig(env(with("CONTACT_GOFLOW_URL", ok))); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	if _, err := loadConfig(env(with("CONTACT_ADDR", "[::1]:8090"))); err != nil {
		t.Errorf("[::1] refused: %v", err)
	}
}

// Every timeout is set (requirement 4), and writing a response outlasts
// the GoFlow call it waits for.
func TestServerHasTimeouts(t *testing.T) {
	s := newServer(defaultAddr, http.NotFoundHandler())
	if s.ReadHeaderTimeout <= 0 || s.ReadTimeout <= 0 || s.WriteTimeout <= 0 || s.IdleTimeout <= 0 || s.MaxHeaderBytes <= 0 {
		t.Errorf("a timeout or limit is unset: %+v", s)
	}
	if s.WriteTimeout <= goflowTimeout {
		t.Errorf("WriteTimeout %v must outlast the GoFlow call (%v)", s.WriteTimeout, goflowTimeout)
	}
	if newGoFlow(config{}, nil).client.Timeout != goflowTimeout {
		t.Error("the GoFlow client has no timeout")
	}
}

// Requests the form doesn't make are refused before anything is sent.
func TestOnlyTheFormIsAccepted(t *testing.T) {
	h := newHarness(t, created)
	token := h.formToken()

	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/send", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, _ := h.do(req); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("JSON body: %d, want 415", resp.StatusCode)
	}
	form := goodForm(token)
	form.Set("message", strings.Repeat("a", maxFormBytes))
	if resp, _ := h.post(form, ""); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d, want 413", resp.StatusCode)
	}
	form = goodForm(token)
	form.Add("name", "Second Name")
	if resp, _ := h.post(form, ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a field sent twice: %d, want 400", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, h.server.URL+"/send?"+goodForm(token).Encode(), nil)
	if resp, _ := h.do(req); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /send: %d, want 405", resp.StatusCode)
	}
	if n := len(h.fake.requests()); n != 0 {
		t.Errorf("GoFlow got %d requests, want 0", n)
	}
}

// Every page carries the headers, and nothing in the pages can run script
// or load from elsewhere.
func TestPagesAreLockedDown(t *testing.T) {
	h := newHarness(t, created)
	for _, path := range []string{"/", "/thanks", "/style.css"} {
		req, _ := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
		resp, _ := h.do(req)
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "form-action 'self'") || strings.Contains(csp, "script-src") {
			t.Errorf("%s CSP %q", path, csp)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Referrer-Policy") != "no-referrer" || resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s headers: %v", path, resp.Header)
		}
	}
	for _, f := range []string{"form.html", "thanks.html"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := strings.ToLower(string(b))
		for _, banned := range []string{"<script", "javascript:", "style=", "http://", "https://"} {
			if strings.Contains(s, banned) {
				t.Errorf("%s contains %q", f, banned)
			}
		}
		if m := regexp.MustCompile(`\son[a-z]+\s*=`).FindString(s); m != "" {
			t.Errorf("%s has an event handler attribute: %q", f, m)
		}
	}
}

// The source holds the boundary: the job is built in one place, from the
// three fields, and the key is used in one line (the Authorization header).
func TestSourceRules(t *testing.T) {
	src := func(f string) string {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	all := src("main.go") + src("handler.go") + src("goflow.go") + src("token.go")
	if n := strings.Count(all, "g.key"); n != 1 || !strings.Contains(all, `req.Header.Set("Authorization", "Bearer "+g.key)`) {
		t.Errorf("the key is used in %d places; want only the Authorization header", n)
	}
	if strings.Count(all, "job.Webhook.URL = g.hook") != 1 || strings.Contains(src("handler.go"), "Webhook") {
		t.Error("the webhook URL must be set once, in goflow.go, from the settings")
	}
	for _, banned := range []string{"r.Form[", "r.FormValue(", "r.URL.Query()", "fmt.Print", "log.Print", "io.ReadAll(resp.Body)"} {
		if strings.Contains(all, banned) {
			t.Errorf("source contains %q", banned)
		}
	}
}
