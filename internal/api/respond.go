package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

const maxBodyBytes = 1 << 20 // 1 MiB

type errorResponse struct {
	Error string `json:"error"`
}

// writeJSON sends v as a JSON response with the given status code.
// Order matters: headers first, then WriteHeader, then the body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status is already sent, so an encode error can't change the response.
	// It almost always means the client disconnected, so it is not logged.
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends {"error": msg} with the given status code.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// requestError is a client mistake in the request: bad body, wrong headers.
type requestError struct {
	status int
	msg    string
}

func (e *requestError) Error() string { return e.msg }

// writeRequestError turns a decodeJSON error into a JSON error response.
func writeRequestError(w http.ResponseWriter, err error) {
	var re *requestError
	if errors.As(err, &re) {
		writeError(w, re.status, re.msg)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal error")
}

// decodeJSON reads exactly one JSON object from the request body into dst.
// Every failure is returned as a *requestError with the right status code.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &requestError{http.StatusUnsupportedMediaType, "Content-Type must be application/json"}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError

		switch {
		case errors.As(err, &maxErr):
			return &requestError{http.StatusRequestEntityTooLarge,
				fmt.Sprintf("body must not exceed %d bytes", maxErr.Limit)}
		case errors.Is(err, io.EOF):
			return &requestError{http.StatusBadRequest, "body must not be empty"}
		case errors.As(err, &syntaxErr), errors.Is(err, io.ErrUnexpectedEOF):
			return &requestError{http.StatusBadRequest, "body contains malformed JSON"}
		case errors.As(err, &typeErr):
			return &requestError{http.StatusBadRequest,
				fmt.Sprintf("field %q has the wrong type", typeErr.Field)}
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			// encoding/json has no typed error for this, so we match the text.
			return &requestError{http.StatusBadRequest, strings.TrimPrefix(err.Error(), "json: ")}
		default:
			return &requestError{http.StatusBadRequest, "invalid request body"}
		}
	}

	// A second Decode must hit EOF, or the body had more than one JSON value.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &requestError{http.StatusBadRequest, "body must contain a single JSON object"}
	}
	return nil
}
