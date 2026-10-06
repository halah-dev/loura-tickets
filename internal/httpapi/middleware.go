package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

// ctxKey is unexported so nothing outside this package can collide with it.
type ctxKey int

const requestIDKey ctxKey = iota

// RequestIDHeader is read as well as written: reusing an id the caller already
// has is what makes a trace span both services.
const RequestIDHeader = "X-Request-Id"

// RequestIDFrom returns the id assigned to this request, or "" outside one.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// newRequestID returns 16 hex characters. Not a UUID: it only has to be unique
// enough to find one log line.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Does not happen in practice, but a timestamp still correlates better
		// than nothing.
		return hex.EncodeToString([]byte(time.Now().UTC().Format("150405.000000")))
	}
	return hex.EncodeToString(b[:])
}

// statusRecorder captures the status for the access log. WriteHeader may never
// be called, so it starts at 200.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// withRequestID assigns an id, echoes it back, and logs one line per request,
// so an error in the logs matches the one the caller saw.
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" {
			id = newRequestID()
		}

		w.Header().Set(RequestIDHeader, id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r.WithContext(ctx))

		s.log.Info("request",
			"request_id", id,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// logger carries the request id, so handlers do not have to attach it.
func (s *Server) logger(ctx context.Context) *slog.Logger {
	if id := RequestIDFrom(ctx); id != "" {
		return s.log.With("request_id", id)
	}
	return s.log
}
