package api

import (
	"encoding/json"
	"net/http"
)

// NewRouter returns the HTTP handler for the whole API.
func NewRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth)
	return mux
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeJSON sends v as a JSON response with the given status code.
// Order matters: headers first, then WriteHeader, then the body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Once WriteHeader has run, the status is already sent and can't change.
	// An encode error here usually means the client disconnected.
	// Task 8 adds logging for it.
	_ = json.NewEncoder(w).Encode(v)
}
