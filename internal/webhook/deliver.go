package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/work-frame/goflow/internal/database"
	"github.com/work-frame/goflow/internal/egress"
	"github.com/work-frame/goflow/internal/job"
	"github.com/work-frame/goflow/internal/worker"
)

// maxResponseBytes is how much of a receiver's answer is read before the
// connection is closed. The body is never stored: only the status counts.
const maxResponseBytes = 64 << 10

// Secrets gives the signing secrets for a user's deliveries.
type Secrets interface {
	Signing(ctx context.Context, userID string) ([]string, error)
}

// Attempts records each attempt.
type Attempts interface {
	Record(ctx context.Context, jobID string, a job.Attempt) error
}

// Deliverer is the handler for webhook jobs.
type Deliverer struct {
	Client   *http.Client // egress.NewClient(); egress.NewTestClient in tests
	Secrets  Secrets
	Attempts Attempts
	Logger   *slog.Logger
	// Now is the clock for webhook-timestamp; nil means time.Now.
	Now func() time.Time
}

// Handle delivers one attempt. It returns nil on a 2xx answer, a
// worker.Permanent error when trying again can't help, and a plain error
// (retried with backoff) otherwise:
//
//	2xx                                   delivered
//	timeout, refused, reset, DNS failure  retry
//	408, 429, 5xx                         retry
//	1xx, 3xx, other 4xx                   permanent (redirects are never followed)
//	blocked address or port, bad cert     permanent
//	no signing secret                     permanent
//
// Errors never contain the URL: its path or query may hold a token, and
// the error is stored as last_error and logged.
func (d *Deliverer) Handle(ctx context.Context, run worker.Run) error {
	if run.Webhook == nil || run.Webhook.Body == nil {
		return worker.Permanent(errors.New("webhook: job has no payload"))
	}
	host := hostOf(run.Webhook.URL)

	secrets, err := d.Secrets.Signing(ctx, run.UserID)
	if errors.Is(err, database.ErrNoSecret) {
		return d.finish(ctx, run, host, time.Now(), 0, job.OutcomePermanent,
			errors.New("no signing secret: create one with POST /api/v1/webhook-secret"))
	}
	if err != nil {
		return err // the database: try again later, no attempt was made
	}

	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	ts := now().Unix()
	sig, err := SignatureHeader(secrets, run.JobID, ts, run.Webhook.Body)
	if err != nil {
		return worker.Permanent(err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, egress.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, run.Webhook.URL, bytes.NewReader(run.Webhook.Body))
	if err != nil {
		return d.finish(ctx, run, host, time.Now(), 0, job.OutcomePermanent, errors.New("webhook: the URL can't be used"))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GoFlow-Webhooks/1")
	req.Header.Set(HeaderID, run.JobID)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderSignature, sig)

	start := time.Now()
	resp, err := d.Client.Do(req)
	if err != nil {
		outcome := job.OutcomeRetry
		if permanentNetError(err) {
			outcome = job.OutcomePermanent
		}
		return d.finish(ctx, run, host, start, 0, outcome, withoutURL(err))
	}
	// Read what we'll take of the answer, inside the same deadline. A
	// failure here doesn't change the outcome: the status line already
	// said whether the receiver accepted the delivery.
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	resp.Body.Close()

	code := resp.StatusCode
	switch {
	case code >= 200 && code < 300:
		return d.finish(ctx, run, host, start, code, job.OutcomeDelivered, nil)
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		return d.finish(ctx, run, host, start, code, job.OutcomeRetry, fmt.Errorf("webhook: receiver answered %d", code))
	default:
		return d.finish(ctx, run, host, start, code, job.OutcomePermanent, fmt.Errorf("webhook: receiver answered %d", code))
	}
}

// finish records the attempt, logs it by host only, and returns what the
// worker pool should do with the job.
func (d *Deliverer) finish(ctx context.Context, run worker.Run, host string, start time.Time, code int, outcome string, cause error) error {
	a := job.Attempt{Attempt: run.Attempt, StartedAt: start.UTC(), DurationMS: int(time.Since(start).Milliseconds()),
		StatusCode: code, Outcome: outcome}
	if cause != nil {
		a.Error = cause.Error()
	}
	// The attempt's own context may be over (timeout, shutdown); recording
	// it still matters.
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if d.Attempts != nil {
		if err := d.Attempts.Record(recCtx, run.JobID, a); err != nil && d.Logger != nil {
			d.Logger.Warn("webhook: attempt not recorded", "job_id", run.JobID, "err", err)
		}
	}
	if d.Logger != nil {
		d.Logger.Info("webhook: attempt", "job_id", run.JobID, "attempt", run.Attempt, "host", host,
			"status", code, "outcome", outcome, "duration_ms", a.DurationMS)
	}
	switch outcome {
	case job.OutcomeDelivered:
		return nil
	case job.OutcomePermanent:
		return worker.Permanent(cause)
	default:
		if ctx.Err() != nil {
			return ctx.Err() // shutdown or the job's deadline: the pool decides
		}
		return cause
	}
}

// permanentNetError reports failures that another attempt can't fix: an
// address or port GoFlow refuses, and a certificate that doesn't check out.
func permanentNetError(err error) bool {
	var blocked *egress.BlockedError
	var unknownCA x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	return errors.As(err, &blocked) || errors.As(err, &unknownCA) || errors.As(err, &hostname) ||
		errors.As(err, &invalid) || errors.As(err, &verify)
}

// withoutURL drops the "Post \"https://...\": " that *url.Error puts in
// front of the cause.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return errors.New("webhook: no complete answer within 10s")
		}
		return fmt.Errorf("webhook: %w", ue.Err)
	}
	return err
}

// hostOf is the host name for logs: never the path or the query.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
