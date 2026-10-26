package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

const (
	// dbTimeout caps every storage call made while handling a request.
	dbTimeout = 3 * time.Second

	// statusClientClosedRequest is nginx's convention for "the client hung up first".
	// It isn't in the HTTP standard, but it keeps these out of the 5xx error count.
	statusClientClosedRequest = 499
)

// JobStore is what the API needs from job storage.
// database.JobStore satisfies it in production. Tests use small in-memory fakes.
type JobStore interface {
	Add(ctx context.Context, j *job.Job) error
	Get(ctx context.Context, id string) (*job.Job, error)
	List(ctx context.Context, status job.Status) ([]*job.Job, error)
	Delete(ctx context.Context, id string) error
}

// Pinger reports whether a dependency is reachable. *sql.DB satisfies it.
type Pinger interface {
	PingContext(ctx context.Context) error
}

// NewRouter returns the HTTP handler for the whole API, wrapped in middleware.
//
// Request flow, outermost first:
// withRequestID → logRequests → recoverPanics → jsonMuxErrors → mux → handler
func NewRouter(store JobStore, db Pinger, logger *slog.Logger) http.Handler {
	h := &handler{store: store, db: db, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth)
	mux.HandleFunc("GET /readyz", h.ready)
	mux.HandleFunc("POST /api/v1/jobs", h.createJob)
	mux.HandleFunc("GET /api/v1/jobs", h.listJobs)
	mux.HandleFunc("GET /api/v1/jobs/{id}", h.getJob)
	mux.HandleFunc("DELETE /api/v1/jobs/{id}", h.deleteJob)

	var handler http.Handler = mux
	handler = jsonMuxErrors(handler)
	handler = recoverPanics(logger, handler)
	handler = logRequests(logger, handler)
	handler = withRequestID(handler)
	return handler
}

// handler holds what the endpoints depend on.
type handler struct {
	store  JobStore
	db     Pinger
	logger *slog.Logger
}

// dbContext derives a storage context from the request: it ends when the client
// disconnects, the server shuts down the request, or dbTimeout passes.
func dbContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), dbTimeout)
}

// internalError logs the real cause server-side and sends the client a generic 500.
func (h *handler) internalError(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.Error("internal error", "request_id", RequestID(r.Context()), "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// storeError turns an unexpected storage error into the right response.
func (h *handler) storeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, context.Canceled):
		// The client went away. Nobody will read the body, but the status keeps the log honest.
		h.logger.Info("request cancelled by client", "request_id", RequestID(r.Context()))
		w.WriteHeader(statusClientClosedRequest)
	case errors.Is(err, context.DeadlineExceeded):
		h.logger.Warn("storage timeout", "request_id", RequestID(r.Context()), "err", err)
		writeError(w, http.StatusServiceUnavailable, "service temporarily unavailable")
	default:
		h.internalError(w, r, err)
	}
}

// handleHealth is the liveness check: the process is up and serving HTTP.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ready is the readiness check: can this instance serve real traffic right now?
func (h *handler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := h.db.PingContext(ctx); err != nil {
		h.logger.Warn("readiness check failed", "request_id", RequestID(r.Context()), "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type createJobRequest struct {
	Title string `json:"title"`
}

func (h *handler) createJob(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeRequestError(w, err)
		return
	}

	j, err := job.New(req.Title)
	switch {
	case errors.Is(err, job.ErrEmptyTitle):
		writeError(w, http.StatusBadRequest, "title is required")
		return
	case errors.Is(err, job.ErrTitleTooLong):
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("title must be at most %d characters", job.MaxTitleLength))
		return
	case err != nil:
		h.internalError(w, r, err)
		return
	}

	ctx, cancel := dbContext(r)
	defer cancel()

	if err := h.store.Add(ctx, j); err != nil {
		h.storeError(w, r, err)
		return
	}

	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	writeJSON(w, http.StatusCreated, j)
}

type listJobsResponse struct {
	Jobs []*job.Job `json:"jobs"`
}

func (h *handler) listJobs(w http.ResponseWriter, r *http.Request) {
	status := job.Status(r.URL.Query().Get("status"))
	if status != "" && !status.Valid() {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid status %q", status))
		return
	}

	ctx, cancel := dbContext(r)
	defer cancel()

	// Filtering happens in the store, so Postgres can use the (status, created_at) index.
	jobs, err := h.store.List(ctx, status)
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listJobsResponse{Jobs: jobs})
}

func (h *handler) getJob(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := dbContext(r)
	defer cancel()

	j, err := h.store.Get(ctx, r.PathValue("id"))
	if errors.Is(err, job.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (h *handler) deleteJob(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := dbContext(r)
	defer cancel()

	err := h.store.Delete(ctx, r.PathValue("id"))
	if errors.Is(err, job.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
