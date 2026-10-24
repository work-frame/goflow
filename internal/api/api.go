package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/work-frame/goflow/internal/job"
)

// NewRouter returns the HTTP handler for the whole API, wrapped in middleware.
//
// Request flow, outermost first:
// withRequestID → logRequests → recoverPanics → jsonMuxErrors → mux → handler
func NewRouter(store *job.MemoryStore, logger *slog.Logger) http.Handler {
	h := &handler{store: store, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth)
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

// handler holds what the job endpoints depend on.
type handler struct {
	store  *job.MemoryStore
	logger *slog.Logger
}

// internalError logs the real cause server-side and sends the client a generic 500.
func (h *handler) internalError(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.Error("internal error", "request_id", RequestID(r.Context()), "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
