package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/database"
	"github.com/work-frame/goflow/internal/egress"
	"github.com/work-frame/goflow/internal/job"
	"github.com/work-frame/goflow/internal/worker"
)

// The example from the Standard Webhooks specification: a receiver using
// any library that follows it will accept GoFlow's signatures.
func TestSignMatchesTheStandardWebhooksExample(t *testing.T) {
	got, err := Sign("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", "msg_p5jXN8AQM9LWM0D4loKWxJek", 1614265330, []byte(`{"test": 2432232314}`))
	if err != nil || got != "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=" {
		t.Errorf("Sign = %q, %v; want the spec's v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=", got, err)
	}
}

func TestSignatureHeaderCarriesEverySecret(t *testing.T) {
	body := []byte(`{"order":1042}`)
	h, err := SignatureHeader([]string{"whsec_bmV3LXNlY3JldA==", "whsec_b2xkLXNlY3JldA=="}, "job1", 100, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(h)) != 2 {
		t.Fatalf("header %q, want two signatures", h)
	}
	for _, s := range []string{"whsec_bmV3LXNlY3JldA==", "whsec_b2xkLXNlY3JldA=="} {
		if !Verify(s, "job1", 100, body, h) {
			t.Errorf("a receiver with %s rejects the header", s)
		}
	}
	if Verify("whsec_b3RoZXI=", "job1", 100, body, h) || Verify("whsec_bmV3LXNlY3JldA==", "job1", 101, body, h) {
		t.Error("a wrong secret or timestamp verifies")
	}
}

type fakeSecrets struct {
	secrets []string
	err     error
}

func (f fakeSecrets) Signing(context.Context, string) ([]string, error) { return f.secrets, f.err }

type recorded struct {
	mu       sync.Mutex
	attempts []job.Attempt
}

func (r *recorded) Record(_ context.Context, _ string, a job.Attempt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts = append(r.attempts, a)
	return nil
}

func (r *recorded) last(t *testing.T) job.Attempt {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.attempts) == 0 {
		t.Fatal("no attempt recorded")
	}
	return r.attempts[len(r.attempts)-1]
}

const secret = "whsec_dGVzdC1zaWduaW5nLXNlY3JldC0xMjM0NTY3OA=="

// receiver runs h as https://example.com on this machine and returns a
// Deliverer that can reach it.
func receiver(t *testing.T, h http.Handler, timeout time.Duration) (*Deliverer, *recorded) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	ap := netip.MustParseAddrPort(srv.Listener.Addr().String())
	c := egress.NewTestClient(t, egress.TestOptions{
		Allow:   []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
		Hosts:   map[string]netip.AddrPort{"example.com": ap},
		RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
		Timeout: timeout,
	})
	rec := &recorded{}
	return &Deliverer{Client: c, Secrets: fakeSecrets{secrets: []string{secret}}, Attempts: rec}, rec
}

func run(url string) worker.Run {
	return worker.Run{JobID: "0123456789abcdef0123456789abcdef", Type: job.TypeWebhook, Attempt: 1, UserID: "u1",
		Webhook: &job.Webhook{URL: url, Body: json.RawMessage(`{"order":1042}`)}}
}

func TestDeliverySendsASignedPost(t *testing.T) {
	var got *http.Request
	var body []byte
	d, rec := receiver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}), 0)

	if err := d.Handle(context.Background(), run("https://example.com/hooks/goflow?token=t0k3n")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	ts, _ := strconv.ParseInt(got.Header.Get(HeaderTimestamp), 10, 64)
	switch {
	case got.Method != http.MethodPost || got.URL.Path != "/hooks/goflow" || got.URL.Query().Get("token") != "t0k3n":
		t.Errorf("request %s %s", got.Method, got.URL)
	case got.Header.Get("Content-Type") != "application/json" || string(body) != `{"order":1042}`:
		t.Errorf("Content-Type %q, body %s", got.Header.Get("Content-Type"), body)
	case got.Header.Get(HeaderID) != "0123456789abcdef0123456789abcdef":
		t.Errorf("webhook-id %q, want the job ID", got.Header.Get(HeaderID))
	case time.Since(time.Unix(ts, 0)).Abs() > time.Minute:
		t.Errorf("webhook-timestamp %d isn't now", ts)
	case !Verify(secret, got.Header.Get(HeaderID), ts, body, got.Header.Get(HeaderSignature)):
		t.Errorf("signature %q doesn't verify", got.Header.Get(HeaderSignature))
	}
	if a := rec.last(t); a.Outcome != job.OutcomeDelivered || a.StatusCode != 204 || a.Attempt != 1 || a.Error != "" {
		t.Errorf("recorded %+v, want delivered 204", a)
	}
}

func TestDeliveryOutcomes(t *testing.T) {
	for _, c := range []struct {
		status    int
		outcome   string
		permanent bool
	}{
		{200, job.OutcomeDelivered, false},
		{408, job.OutcomeRetry, false},
		{429, job.OutcomeRetry, false},
		{500, job.OutcomeRetry, false},
		{503, job.OutcomeRetry, false},
		{400, job.OutcomePermanent, true},
		{401, job.OutcomePermanent, true},
		{404, job.OutcomePermanent, true},
		{410, job.OutcomePermanent, true},
		{422, job.OutcomePermanent, true},
	} {
		d, rec := receiver(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(c.status) }), 0)
		err := d.Handle(context.Background(), run("https://example.com/hook"))
		if (err == nil) != (c.outcome == job.OutcomeDelivered) || worker.IsPermanent(err) != c.permanent {
			t.Errorf("%d: err %v (permanent %v), want outcome %s", c.status, err, worker.IsPermanent(err), c.outcome)
		}
		if a := rec.last(t); a.Outcome != c.outcome || a.StatusCode != c.status {
			t.Errorf("%d: recorded %+v, want %s", c.status, a, c.outcome)
		}
	}
}

// A redirect is the answer, permanent, and its target is never asked.
func TestDeliveryNeverFollowsARedirect(t *testing.T) {
	var followed int
	mux := http.NewServeMux()
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/other", http.StatusFound) })
	mux.HandleFunc("/other", func(http.ResponseWriter, *http.Request) { followed++ })
	d, rec := receiver(t, mux, 0)
	err := d.Handle(context.Background(), run("https://example.com/hook"))
	if !worker.IsPermanent(err) || followed != 0 || rec.last(t).StatusCode != 302 {
		t.Errorf("err %v, followed %d, recorded %+v; want a permanent 302 and no second request", err, followed, rec.last(t))
	}
}

func TestDeliveryThatTimesOutIsRetried(t *testing.T) {
	release := make(chan struct{})
	d, rec := receiver(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }), 300*time.Millisecond)
	t.Cleanup(func() { close(release) })
	err := d.Handle(context.Background(), run("https://example.com/hook"))
	if err == nil || worker.IsPermanent(err) || rec.last(t).Outcome != job.OutcomeRetry {
		t.Errorf("err %v, recorded %+v; want a retry", err, rec.last(t))
	}
}

func TestNoSigningSecretIsPermanent(t *testing.T) {
	d := &Deliverer{Client: egress.NewClient(), Secrets: fakeSecrets{err: database.ErrNoSecret}, Attempts: &recorded{}}
	err := d.Handle(context.Background(), run("https://example.com/hook"))
	if !worker.IsPermanent(err) || !strings.Contains(err.Error(), "POST /api/v1/webhook-secret") {
		t.Errorf("err %v, want a permanent error pointing at POST /api/v1/webhook-secret", err)
	}
}

// The URL's path and query may hold a token: last_error, the recorded
// attempt and the logs only ever see the reason.
func TestErrorsNeverContainTheURL(t *testing.T) {
	rec := &recorded{}
	d := &Deliverer{Client: egress.NewClient(), Secrets: fakeSecrets{secrets: []string{secret}}, Attempts: rec}
	err := d.Handle(context.Background(), run("https://127.0.0.1/hooks?token=s3cr3t"))
	if !worker.IsPermanent(err) {
		t.Errorf("a blocked address: err %v, want permanent", err)
	}
	for _, s := range []string{err.Error(), rec.last(t).Error} {
		if strings.Contains(s, "s3cr3t") || strings.Contains(s, "/hooks") {
			t.Errorf("error %q contains part of the URL", s)
		}
	}
}

// A certificate that doesn't check out won't start checking out.
func TestUntrustedCertificateIsPermanent(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	ap := netip.MustParseAddrPort(srv.Listener.Addr().String())
	c := egress.NewTestClient(t, egress.TestOptions{
		Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
		Hosts: map[string]netip.AddrPort{"example.com": ap},
	})
	d := &Deliverer{Client: c, Secrets: fakeSecrets{secrets: []string{secret}}, Attempts: &recorded{}}
	if err := d.Handle(context.Background(), run("https://example.com/hook")); !worker.IsPermanent(err) {
		t.Errorf("err %v, want permanent", err)
	}
}
