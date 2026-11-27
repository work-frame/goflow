package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"
)

const (
	goflowTimeout = 10 * time.Second
	// maxResponseBytes bounds what is read of GoFlow's answer. Only the
	// job's id is needed; a created job with a 2,000-character message is
	// about 3 KiB.
	maxResponseBytes = 64 << 10
	jobTitle         = "contact form message" // titles show in lists and events: never personal data
	maxAttempts      = 5                      // the free receiver may be asleep on the first try
)

// message is what a visitor may choose: these three fields and nothing
// else. Everything else about the job is decided in goflow.send.
type message struct {
	Name, Email, Message string
}

// webhookBody is what the receiver gets. submission_id is the token's ID:
// the same on a resend of the same page, so the replay asks for the same
// work. (A send time here would differ on a resend, and GoFlow would
// answer 422; the job's created_at and the webhook-timestamp header say
// when instead.)
type webhookBody struct {
	SubmissionID string `json:"submission_id"`
	Name         string `json:"name"`
	Email        string `json:"email"`
	Message      string `json:"message"`
}

type jobRequest struct {
	Title   string `json:"title"`
	Type    string `json:"type"`
	Webhook struct {
		URL  string      `json:"url"`
		Body webhookBody `json:"body"`
	} `json:"webhook"`
	MaxAttempts int `json:"max_attempts"`
}

// goflow makes the one call this example makes: create a webhook job.
type goflow struct {
	base, key, hook string
	client          *http.Client
}

func newGoFlow(cfg config, client *http.Client) *goflow {
	if client == nil {
		client = &http.Client{}
	}
	c := *client
	c.Timeout = goflowTimeout
	// Follow no redirect: a redirect could take the request, key and all,
	// somewhere else. A 3xx is an unexpected answer like any other.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &goflow{base: cfg.GoFlowURL, key: cfg.Key, hook: cfg.WebhookURL, client: &c}
}

// result is what came back: the HTTP status (0 if there was none), the
// job's ID when GoFlow sent one, and whether it was a replay.
type result struct {
	Status   int
	JobID    string
	Replayed bool
}

var errUnreachable = errors.New("goflow unreachable")

// jobIDFormat is what may be logged as a job's ID: GoFlow's are 32 hex characters.
var jobIDFormat = regexp.MustCompile(`^[0-9a-f]{32}$`)

// send creates the job. id is the submission token's ID. The error is
// errUnreachable, never wrapped with the request: no header, body or key
// can reach a log or a page through it.
func (g *goflow) send(ctx context.Context, id string, m message) (result, error) {
	var job jobRequest
	job.Title = jobTitle
	job.Type = "webhook"
	job.Webhook.URL = g.hook
	job.Webhook.Body = webhookBody{SubmissionID: id, Name: m.Name, Email: m.Email, Message: m.Message}
	job.MaxAttempts = maxAttempts
	body, err := json.Marshal(job)
	if err != nil {
		return result{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.base+"/api/v1/jobs", bytes.NewReader(body))
	if err != nil {
		return result{}, errUnreachable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.key)
	req.Header.Set("Idempotency-Key", "contactform-"+id)

	resp, err := g.client.Do(req)
	if err != nil {
		return result{}, errUnreachable
	}
	defer resp.Body.Close()

	r := result{Status: resp.StatusCode, Replayed: resp.Header.Get("Idempotent-Replayed") == "true"}
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		var created struct {
			ID string `json:"id"`
		}
		// Too long or not JSON: the job was still accepted; only its ID is unknown.
		if json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&created) == nil && jobIDFormat.MatchString(created.ID) {
			r.JobID = created.ID
		}
	}
	// Anything else is not read at all: its text never reaches the visitor.
	return r, nil
}
