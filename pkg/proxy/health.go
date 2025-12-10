// health.go
package proxy

import (
	"net/http"
	"time"
)

type Health struct {
	pool          *BackendPool
	checkInterval time.Duration
}

func NewHealthCheck(pool *BackendPool, checkInterval time.Duration) *Health {
	return &Health{checkInterval: checkInterval, pool: pool}
}

func (h *Health) Check() {
	ticker := time.NewTicker(h.checkInterval)
	defer ticker.Stop()

	for range ticker.C {
		backends := h.pool.GetAllBackends()

		for _, backend := range backends {
			// Check circuit state and decide whether to check
			shouldCheck, _ := h.pool.GetCircuitState(backend.Name, h.checkInterval)

			if !shouldCheck {
				continue
			}

			go h.checkBackend(backend)
		}
	}
}

func (h *Health) checkBackend(backend *Backend) {
	healthURL := *backend.TargetUrl
	healthURL.Path = "/health"

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(healthURL.String())

	if err != nil {
		h.pool.UpdateBackendStatus(backend.Name, false, h.checkInterval)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		h.pool.UpdateBackendStatus(backend.Name, true, h.checkInterval)
	} else {
		h.pool.UpdateBackendStatus(backend.Name, false, h.checkInterval)
	}
}

func (h *Health) GetHealthCHeckInterval() time.Duration {
	return h.checkInterval
}
