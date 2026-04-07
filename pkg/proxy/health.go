package proxy

import (
	"context"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type Health struct {
	pool          *BackendPool
	checkInterval time.Duration
	stopCh        chan struct{}
	mu            sync.RWMutex // ← Add this
}

func NewHealthCheck(pool *BackendPool, checkInterval time.Duration) *Health {
	return &Health{
		checkInterval: checkInterval,
		pool:          pool,
		stopCh:        make(chan struct{}),
	}
}

func (h *Health) Check() {
	ticker := time.NewTicker(h.getInterval()) // ← Thread-safe read
	defer ticker.Stop()
	var healthSem = make(chan struct{}, 20)
	for {
		select {
		case <-ticker.C:
			services := h.pool.GetAllServices()
			for _, service := range services {
				backends, exist := h.pool.GetBackendsForService(service)
				if exist {
					for _, backend := range backends {
						shouldCheck, _ := h.pool.GetCircuitState(service, backend.Name, h.getInterval())
						if !shouldCheck {
							continue
						}
						go func(s string, b *Backend) {
							healthSem <- struct{}{}
							defer func() { <-healthSem }()
							h.checkBackend(s, b)
						}(service, backend)
					}
				}
			}
		case <-h.stopCh:
			return
		}
	}
}

func (h *Health) checkBackend(service string, backend *Backend) {
	healthURL := *backend.TargetUrl
	healthURL.Path = "/health"

	// Create span for health check
	ctx, span := otel.Tracer("ananse").Start(context.Background(), "health.check",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("health.service", service),
			attribute.String("health.backend", backend.Name),
			attribute.String("health.url", healthURL.String()),
		),
	)
	defer span.End()

	// Create request with context
	req, err := http.NewRequestWithContext(ctx, "GET", healthURL.String(), nil)
	if err != nil {
		span.SetAttributes(attribute.String("error", err.Error()))
		h.pool.UpdateBackendStatus(service, backend.Name, false, h.getInterval())
		return
	}

	// Inject trace context into headers
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	// TODO: refactor to make the timeout configurable
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)

	if err != nil {
		span.SetAttributes(attribute.String("error", err.Error()))
		h.pool.UpdateBackendStatus(service, backend.Name, false, h.getInterval())
		return
	}
	defer resp.Body.Close()

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		h.pool.UpdateBackendStatus(service, backend.Name, true, h.getInterval())
	} else {
		h.pool.UpdateBackendStatus(service, backend.Name, false, h.getInterval())
	}
}

// Thread-safe getter
func (h *Health) getInterval() time.Duration {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.checkInterval
}

func (h *Health) GetHealthCheckInterval() time.Duration {
	return h.getInterval()
}

// Restart with new interval
func (h *Health) Restart(newInterval time.Duration) {
	h.mu.Lock()

	// Signal stop to old goroutine (safe - only closes once)
	select {
	case <-h.stopCh:
		// Already closed, create new channel
		h.stopCh = make(chan struct{})
	default:
		// Not closed yet, close it
		close(h.stopCh)
		h.stopCh = make(chan struct{})
	}

	// Update interval
	h.checkInterval = newInterval

	h.mu.Unlock()
	go h.Check()
}
