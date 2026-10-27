// Package client is a Go client for the GoFlow HTTP API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

var (
	// ErrUnauthenticated means the API refused the session token: it is missing,
	// wrong, expired, or was ended by logout.
	ErrUnauthenticated = errors.New("client: not signed in, or the session has expired")
	// ErrInvalidCredentials means login failed: wrong email or password.
	ErrInvalidCredentials = errors.New("client: invalid email or password")
)

// Client talks to a GoFlow API server. It is safe for concurrent use.
type Client struct {
	baseURL string
	token   string // session token; never logged or put in an error
	http    *http.Client
}

// New returns a Client for the API at baseURL, for example "http://localhost:8080".
// token is a session token from POST /api/v1/auth/login; "" sends none.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// A backstop only. Callers should also pass a context with their own deadline.
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// APIError is a response the API sent back with an unexpected status.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("API returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)
}

// Login exchanges an email and password for a session token. The password is
// only ever sent in the request body, never in the URL.
func (c *Client) Login(ctx context.Context, email, password string) (token string, expiresAt time.Time, err error) {
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("client: encode request: %w", err)
	}
	var resp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	err = c.do(ctx, http.MethodPost, "/api/v1/auth/login", body, http.StatusOK, &resp)
	if errors.Is(err, ErrUnauthenticated) {
		return "", time.Time{}, ErrInvalidCredentials
	}
	if err != nil {
		return "", time.Time{}, err
	}
	return resp.Token, resp.ExpiresAt, nil
}

// Logout ends the session for token on the server, whatever token the Client
// was created with. After this the token is useless, even if a copy leaked.
func (c *Client) Logout(ctx context.Context, token string) error {
	return New(c.baseURL, token).do(ctx, http.MethodPost, "/api/v1/auth/logout", nil, http.StatusNoContent, nil)
}

// Create submits a new job.
func (c *Client) Create(ctx context.Context, title string) (*job.Job, error) {
	body, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		return nil, fmt.Errorf("client: encode request: %w", err)
	}
	var j job.Job
	if err := c.do(ctx, http.MethodPost, "/api/v1/jobs", body, http.StatusCreated, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// Get returns one job, or job.ErrNotFound.
func (c *Client) Get(ctx context.Context, id string) (*job.Job, error) {
	var j job.Job
	if err := c.do(ctx, http.MethodGet, "/api/v1/jobs/"+url.PathEscape(id), nil, http.StatusOK, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// List returns one page of jobs and the cursor for the next page ("" on the last page).
func (c *Client) List(ctx context.Context, status job.Status, cursor string) ([]*job.Job, string, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(job.MaxPageSize))
	if status != "" {
		q.Set("status", string(status))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}

	var resp struct {
		Jobs       []*job.Job `json:"jobs"`
		NextCursor string     `json:"next_cursor"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/jobs?"+q.Encode(), nil, http.StatusOK, &resp); err != nil {
		return nil, "", err
	}
	return resp.Jobs, resp.NextCursor, nil
}

// Delete removes a job, or returns job.ErrNotFound.
func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/jobs/"+url.PathEscape(id), nil, http.StatusNoContent, nil)
}

// do sends one request, checks the status, and decodes the body into out if out is not nil.
func (c *Client) do(ctx context.Context, method, path string, body []byte, want int, out any) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return fmt.Errorf("client: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("client: cannot reach the GoFlow API at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != want {
		var e struct {
			Error string `json:"error"`
		}
		// Best effort: a proxy or a crashed server might not send GoFlow's JSON error shape.
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		if resp.StatusCode == http.StatusNotFound && e.Error == "job not found" {
			return job.ErrNotFound
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return ErrUnauthenticated
		}
		return &APIError{Status: resp.StatusCode, Message: e.Error}
	}

	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("client: decode %s %s response: %w", method, path, err)
	}
	return nil
}
