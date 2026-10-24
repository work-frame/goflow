package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// ctxKey is unexported, so no other package can create a key that collides with ours.
type ctxKey int

const requestIDKey ctxKey = 0

// RequestID returns the request ID stored in ctx, or "" if there is none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// withRequestID reuses a safe incoming X-Request-ID or generates a new one,
// echoes it in the response, and stores it in the request context.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)

		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// validRequestID accepts short IDs made of letters, digits, '-' and '_'.
// Anything else is replaced, so clients can't inject newlines or huge values into logs.
func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

func newRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// statusRecorder wraps a ResponseWriter to remember the status code and body size.
// Embedding http.ResponseWriter means every method we don't override passes straight through.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the original writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// logRequests writes one structured log line per request after it completes.
// It logs the path but never the query string or body, which can hold secrets.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		status := rec.status
		if status == 0 {
			status = http.StatusOK // handler wrote nothing; net/http sends 200
		}
		level := slog.LevelInfo
		if status >= 500 {
			level = slog.LevelError
		}

		logger.LogAttrs(r.Context(), level, "request",
			slog.String("request_id", RequestID(r.Context())),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Int("bytes", rec.bytes),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

// recoverPanics turns a panic in a handler into a logged 500 response.
// It only catches panics on the request's own goroutine.
func recoverPanics(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}

		defer func() {
			v := recover()
			if v == nil {
				return
			}
			// net/http uses this panic on purpose to abort a response. Let it through.
			if v == http.ErrAbortHandler {
				panic(v)
			}
			logger.Error("panic recovered",
				"request_id", RequestID(r.Context()),
				"panic", v,
				"stack", string(debug.Stack()),
			)
			if !rec.wroteHeader {
				writeError(rec, http.StatusInternalServerError, "internal error")
			}
		}()

		next.ServeHTTP(rec, r)
	})
}

// muxErrorWriter swaps the mux's plain-text 404 and 405 responses for JSON ones.
// Our own handlers send JSON already, so their 404s pass through untouched.
type muxErrorWriter struct {
	http.ResponseWriter
	intercepted bool
}

func (w *muxErrorWriter) WriteHeader(code int) {
	plain := strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain")
	if plain && (code == http.StatusNotFound || code == http.StatusMethodNotAllowed) {
		w.intercepted = true
		msg := "not found"
		if code == http.StatusMethodNotAllowed {
			msg = "method not allowed"
		}
		writeError(w.ResponseWriter, code, msg)
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *muxErrorWriter) Write(b []byte) (int, error) {
	if w.intercepted {
		return len(b), nil // drop the mux's plain-text body
	}
	return w.ResponseWriter.Write(b)
}

func (w *muxErrorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func jsonMuxErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&muxErrorWriter{ResponseWriter: w}, r)
	})
}
