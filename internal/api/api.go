package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/work-frame/goflow/internal/job"
)

// NewRouter returns the HTTP handler for the whole API.
func NewRouter(store *job.MemoryStore) http.Handler {
	h := &handler{store: store}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth)
	mux.HandleFunc("POST /api/v1/jobs", h.createJob)
	mux.HandleFunc("GET /api/v1/jobs", h.listJobs)
	mux.HandleFunc("GET /api/v1/jobs/{id}", h.getJob)
	mux.HandleFunc("DELETE /api/v1/jobs/{id}", h.deleteJob)
	return mux
}

// handler holds what the job endpoints depend on.
type handler struct {
	store *job.MemoryStore
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
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := h.store.Add(j); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
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
		writeError(w, http.StatusInternalServerError, "internal error")
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
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
