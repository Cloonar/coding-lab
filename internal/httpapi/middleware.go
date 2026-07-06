package httpapi

import (
	"net/http"
	"runtime/debug"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
)

// responseRecorder captures the status code while keeping Flush working
// (SSE) and exposing Unwrap for http.ResponseController.
type responseRecorder struct {
	http.ResponseWriter
	code  int
	wrote bool
}

func (r *responseRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.code = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// recoverMiddleware turns handler panics into logged 500s. ErrAbortHandler
// passes through (it is net/http's own abort mechanism).
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &responseRecorder{ResponseWriter: w, code: http.StatusOK}
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler { //nolint:errorlint // sentinel comparison per net/http docs
				panic(p)
			}
			s.log.Error("panic serving request",
				"component", "httpapi",
				"err", p,
				"method", r.Method,
				"path", r.URL.Path,
				"request_id", logx.RequestID(r.Context()),
				"stack", string(debug.Stack()))
			if !rec.wrote {
				writeError(rec, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// requestIDMiddleware assigns each request a fresh id, carried in the
// context and echoed as X-Request-Id.
func (s *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := logx.NewRequestID()
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(logx.WithRequestID(r.Context(), id)))
	})
}

// loggingMiddleware emits one slog line per request after it completes.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http request",
			"component", "httpapi",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.code,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
			"request_id", logx.RequestID(r.Context()))
	})
}
