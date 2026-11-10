// Package logging configures the Portal's slog JSON logger and the
// HTTP middleware that decorates every request with a request id,
// mirroring control-plane/internal/logging (PORTAL_PLAN 6/S0: "log
// JSON"). Secrets never pass through here: handlers log key ids and
// request ids only (PORTAL_PLAN 7).
package logging

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/oklog/ulid/v2"
)

// NewLogger returns a JSON slog.Logger writing to stderr. If
// PORTAL_DEBUG is set the level is debug; otherwise info.
func NewLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

type ctxKey int

const requestIDKey ctxKey = 1

// RequestID returns the request id stored in ctx by Middleware.
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// WithRequestID stores id under the request-id context key.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Middleware adds an X-Request-Id header to every response, threads
// the request id into the context and emits one JSON log line per
// request.
func Middleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := ulid.Make().String()
		w.Header().Set("X-Request-Id", reqID)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		ctx := WithRequestID(r.Context(), reqID)
		log.LogAttrs(ctx, slog.LevelInfo, "http request",
			slog.String("request_id", reqID),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("remote", r.RemoteAddr),
		)
		next.ServeHTTP(sw, r.WithContext(ctx))
		log.LogAttrs(ctx, slog.LevelInfo, "http response",
			slog.String("request_id", reqID),
			slog.Int("status", sw.status),
			slog.Duration("duration", time.Since(start)),
		)
	})
}
