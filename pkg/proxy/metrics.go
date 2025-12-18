package proxy

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// HTTP Request metrics
	httpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ananse_http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"backend", "method", "status"},
	)

	httpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ananse_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"backend", "method"},
	)

	httpRequestsInFlight = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "ananse_http_requests_in_flight",
			Help: "Current number of HTTP requests being served",
		},
	)

	// Backend metrics
	backendHealthStatus = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "ananse_backend_health_status",
			Help: "Backend health status (1=healthy, 0=unhealthy)",
		},
		[]string{"backend"},
	)

	backendActiveConnections = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "ananse_backend_active_connections",
			Help: "Number of active connections to backend",
		},
		[]string{"backend"},
	)

	// Circuit breaker metrics
	circuitBreakerState = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "ananse_circuit_breaker_state",
			Help: "Circuit breaker state (0=closed, 1=half-open, 2=open)",
		},
		[]string{"backend"},
	)

	circuitBreakerFailures = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ananse_circuit_breaker_failures_total",
			Help: "Total number of backend failures",
		},
		[]string{"backend"},
	)

	// Retry metrics
	retryAttemptsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ananse_retry_attempts_total",
			Help: "Total number of retry attempts",
		},
		[]string{"backend"},
	)
)

// Helper functions to record metrics
func RecordRequest(backend, method, status string, duration float64) {
	httpRequestsTotal.WithLabelValues(backend, method, status).Inc()
	httpRequestDuration.WithLabelValues(backend, method).Observe(duration)
}

func RecordRequestStart() {
	httpRequestsInFlight.Inc()
}

func RecordRequestEnd() {
	httpRequestsInFlight.Dec()
}

func UpdateBackendHealth(backend string, healthy bool) {
	if healthy {
		backendHealthStatus.WithLabelValues(backend).Set(1)
	} else {
		backendHealthStatus.WithLabelValues(backend).Set(0)
	}
}

func UpdateBackendConnections(backend string, count int32) {
	backendActiveConnections.WithLabelValues(backend).Set(float64(count))
}

func UpdateCircuitBreakerState(backend string, state State) {
	circuitBreakerState.WithLabelValues(backend).Set(float64(state))
}

func RecordBackendFailure(backend string) {
	circuitBreakerFailures.WithLabelValues(backend).Inc()
}

func RecordRetryAttempt(backend string) {
	retryAttemptsTotal.WithLabelValues(backend).Inc()
}
