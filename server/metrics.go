package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/sethgrid/syl/internal/claude"
)

var (
	claudeCallsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "syl_claude_calls_total",
		Help: "Total Claude API calls by method, role, and status.",
	}, []string{"method", "role", "status"})

	claudeCallDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "syl_claude_call_duration_seconds",
		Help:    "Claude API call duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "role"})

	sseActiveConns = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "syl_sse_active_connections",
		Help: "Current number of active SSE connections.",
	})

	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "syl_http_requests_total",
		Help: "Total HTTP requests by method, path, and status.",
	}, []string{"method", "path", "status"})

	httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "syl_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})
)

// metricsClient wraps a claude.Client and records Prometheus metrics per call.
// role labels the purpose: "classifier", "response", etc.
type metricsClient struct {
	inner claude.Client
	role  string
}

func newMetricsClient(inner claude.Client, role string) *metricsClient {
	return &metricsClient{inner: inner, role: role}
}

func (m *metricsClient) Stream(ctx context.Context, systemPrompt string, messages []claude.Message, onToken func(string) error) (string, error) {
	start := time.Now()
	result, err := m.inner.Stream(ctx, systemPrompt, messages, onToken)
	status := "ok"
	if err != nil {
		status = "error"
	}
	claudeCallsTotal.WithLabelValues("stream", m.role, status).Inc()
	claudeCallDuration.WithLabelValues("stream", m.role).Observe(time.Since(start).Seconds())
	return result, err
}

func (m *metricsClient) Complete(ctx context.Context, systemPrompt string, messages []claude.Message) (string, error) {
	start := time.Now()
	result, err := m.inner.Complete(ctx, systemPrompt, messages)
	status := "ok"
	if err != nil {
		status = "error"
	}
	claudeCallsTotal.WithLabelValues("complete", m.role, status).Inc()
	claudeCallDuration.WithLabelValues("complete", m.role).Observe(time.Since(start).Seconds())
	return result, err
}

// httpMetricsMiddleware records request count and duration per chi route pattern and status.
// The route pattern (e.g. /agents/{id}/soul) is read after the handler returns,
// avoiding high-cardinality path values.
func httpMetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		path := chi.RouteContext(r.Context()).RoutePattern()
		if path == "" {
			path = "unknown"
		}
		status := strconv.Itoa(sw.status)
		httpRequestsTotal.WithLabelValues(r.Method, path, status).Inc()
		httpRequestDuration.WithLabelValues(r.Method, path).Observe(time.Since(start).Seconds())
	})
}

// statusWriter captures the HTTP status code written by a handler.
// It forwards Flush so SSE and other streaming handlers work correctly.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(status int) {
	sw.status = status
	sw.ResponseWriter.WriteHeader(status)
}

func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
