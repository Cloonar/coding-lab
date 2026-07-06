// Package logx owns lab's logging conventions: slog JSON to a writer,
// request-id generation, and the context carrier for both. Canonical keys
// (design §2): component, repo, session, run, err — and never any secret.
package logx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
)

// New returns a JSON slog logger writing to w at info level.
func New(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// NewRequestID returns a fresh 16-char lowercase-hex request id.
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("logx: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

type ctxKey int

const (
	ctxKeyLogger ctxKey = iota
	ctxKeyRequestID
)

// WithLogger returns ctx carrying l.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, l)
}

// FromContext returns the logger carried by ctx, or slog.Default() when none
// is set. It never returns nil.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// WithRequestID returns ctx carrying the request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// RequestID returns the request id carried by ctx, or "" when none is set.
func RequestID(ctx context.Context) string {
	if id, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return id
	}
	return ""
}
