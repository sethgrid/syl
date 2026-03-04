package logger

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"os"
)

type contextKey string

var (
	logKey contextKey = "logger"
	ridKey contextKey = "rid"
)

func New() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, nil))
}

func FromRequest(r *http.Request) *slog.Logger {
	if l, ok := r.Context().Value(logKey).(*slog.Logger); ok {
		return l
	}
	return New()
}

func Middleware(parent *slog.Logger, debug bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid := newRID()
			l := parent.With("rid", rid, "method", r.Method, "path", r.URL.Path)
			ctx := context.WithValue(r.Context(), logKey, l)
			ctx = context.WithValue(ctx, ridKey, rid)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func newRID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
