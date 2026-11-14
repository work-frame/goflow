// Command receiver is a webhook receiver for GoFlow, and an example of
// one: it checks each delivery the way the Standard Webhooks spec says a
// receiver should, then "processes" it by logging that it arrived.
// Phase 15, Task 67: it runs on Render as goflow-receiver, so a delivery
// from the hosted API crosses the real internet.
//
// For each POST /hook, in order:
//
//  1. the signature, with RECEIVER_SECRET (the owner's whsec_ secret): 401
//  2. the timestamp, within 5 minutes of this clock: 400 (a replay)
//  3. the webhook-id: one already processed answers 200 and does nothing
//  4. ?fail=N: the first N attempts of each webhook-id answer 503, to show
//     GoFlow retrying (a demonstration, not something to copy)
//  5. process: log the id and the body's size, never the body; answer 200
//
// The processed IDs live in memory: a restart, or a free instance going to
// sleep, forgets them, and a repeat sent after that is processed again. A
// real receiver keeps them in its database, in the same transaction as the
// work.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/work-frame/goflow/internal/job"
	"github.com/work-frame/goflow/internal/webhook"
)

// tolerance is how far webhook-timestamp may be from this clock, the value
// Standard Webhooks libraries use.
const tolerance = 5 * time.Minute

// maxRemembered bounds the in-memory sets: past it they start over, so a
// flood of IDs can't grow the process without limit.
const maxRemembered = 10000

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	secret := os.Getenv("RECEIVER_SECRET")
	if err := checkSecret(secret); err != nil {
		logger.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           newReceiver(secret, logger, time.Now).routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	logger.Info("receiver listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("receiver stopped", "err", err)
		os.Exit(1)
	}
}

// checkSecret accepts a Standard Webhooks secret. The error never repeats it.
func checkSecret(s string) error {
	if s == "" {
		return errors.New("RECEIVER_SECRET is required: the output of goflow webhook-secret")
	}
	if !strings.HasPrefix(s, "whsec_") {
		return errors.New("RECEIVER_SECRET must start with whsec_")
	}
	if _, err := webhook.Sign(s, "check", 0, nil); err != nil {
		return errors.New("RECEIVER_SECRET must be whsec_ and base64")
	}
	return nil
}

type receiver struct {
	secret string
	log    *slog.Logger
	now    func() time.Time

	mu        sync.Mutex
	processed map[string]bool // webhook-ids handled
	failed    map[string]int  // 503s given per webhook-id (?fail=N)
}

func newReceiver(secret string, log *slog.Logger, now func() time.Time) *receiver {
	return &receiver{secret: secret, log: log, now: now, processed: map[string]bool{}, failed: map[string]int{}}
}

func (rc *receiver) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /hook", rc.hook)
	return mux
}

func (rc *receiver) hook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, job.MaxWebhookBodyBytes+1024))
	if err != nil {
		rc.refuse(w, http.StatusRequestEntityTooLarge, "body too large", "")
		return
	}
	id := r.Header.Get(webhook.HeaderID)
	ts, tsErr := strconv.ParseInt(r.Header.Get(webhook.HeaderTimestamp), 10, 64)

	// 1. Who sent it. Checked before anything else is believed.
	if id == "" || tsErr != nil || !webhook.Verify(rc.secret, id, ts, body, r.Header.Get(webhook.HeaderSignature)) {
		rc.refuse(w, http.StatusUnauthorized, "signature doesn't verify", id)
		return
	}
	// 2. When. A valid signature on an old request is a replay.
	age := rc.now().Sub(time.Unix(ts, 0))
	if age > tolerance || age < -tolerance {
		rc.refuse(w, http.StatusBadRequest, fmt.Sprintf("timestamp is %s away from this clock", age.Round(time.Second)), id)
		return
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()
	// 3. Once. A repeat of a processed delivery succeeds without redoing it.
	if rc.processed[id] {
		rc.log.Info("webhook duplicate, already processed", "webhook_id", id)
		w.WriteHeader(http.StatusOK)
		return
	}
	// 4. The demonstration's planned failures.
	if n, _ := strconv.Atoi(r.URL.Query().Get("fail")); rc.failed[id] < n {
		rc.failed[id]++
		rc.log.Info("webhook refused on purpose (?fail)", "webhook_id", id, "failure", rc.failed[id], "of", n)
		http.Error(w, "failing on purpose", http.StatusServiceUnavailable)
		return
	}
	// 5. Process.
	if len(rc.processed) >= maxRemembered {
		rc.processed, rc.failed = map[string]bool{}, map[string]int{}
	}
	rc.processed[id] = true
	rc.log.Info("webhook processed", "webhook_id", id, "body_bytes", len(body), "age", age.Round(time.Millisecond))
	w.WriteHeader(http.StatusOK)
}

// refuse logs why, by id only, and answers code.
func (rc *receiver) refuse(w http.ResponseWriter, code int, why, id string) {
	rc.log.Warn("webhook refused", "status", code, "reason", why, "webhook_id", id)
	http.Error(w, why, code)
}
