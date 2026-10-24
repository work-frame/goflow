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

// Pinger reports whether a dependency is reachable. *sql.DB satisfies it.
// Defined here, where it is used, so tests can pass a fake without a real database.
type Pinger interface {
	PingContext(ctx context.Context) error
}

// NewRouter returns the HTTP handler for the whole API, wrapped in middleware.
//
// Request flow, outermost first:
// withRequestID → logRequests → recoverPanics → jsonMuxErrors → mux → handler
func NewRouter(store *job.MemoryStore, db Pinger, logger *slog.Logger) http.Handler {
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
	store  *job.MemoryStore
	db     Pinger
	logger *slog.Logger
}

// internalError logs the real cause server-side and sends the client a generic 500.
func (h *handler) internalError(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.Error("internal error", "request_id", RequestID(r.Context()), "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// handleHealth is the liveness check: the process is up and serving HTTP.
// It checks no dependencies, so a database outage never makes it fail.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ready is the readiness check: can this instance serve real traffic right now?
func (h *handler) ready(w http.ResponseWriter, r *http.Request) {
	// Derived from the request context: if the client gives up, the ping is cancelled too.
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

	if err := h.store.Add(j); err != nil {
		h.internalError(w, r, err)
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

	all := h.store.List()
	jobs := make([]*job.Job, 0, len(all))
	for _, j := range all {
		if status == "" || j.Status == status {
			jobs = append(jobs, j)
		}
	}
	writeJSON(w, http.StatusOK, listJobsResponse{Jobs: jobs})
}

func (h *handler) getJob(w http.ResponseWriter, r *http.Request) {
	j, err := h.store.Get(r.PathValue("id"))
	if errors.Is(err, job.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (h *handler) deleteJob(w http.ResponseWriter, r *http.Request) {
	err := h.store.Delete(r.PathValue("id"))
	if errors.Is(err, job.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
