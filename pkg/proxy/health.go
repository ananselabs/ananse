package proxy

import (
	"net/http"
	"sync"
	"time"
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

	for {
		select {
		case <-ticker.C:
			backends := h.pool.GetAllBackends()
			for _, backend := range backends {
				shouldCheck, _ := h.pool.GetCircuitState(backend.Name, h.getInterval())
				if !shouldCheck {
					continue
				}
				go h.checkBackend(backend)
			}
		case <-h.stopCh:
			return
		}
	}
}

func (h *Health) checkBackend(backend *Backend) {
	healthURL := *backend.TargetUrl
	healthURL.Path = "/health"

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(healthURL.String())

	if err != nil {
		h.pool.UpdateBackendStatus(backend.Name, false, h.getInterval())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		h.pool.UpdateBackendStatus(backend.Name, true, h.getInterval())
	} else {
		h.pool.UpdateBackendStatus(backend.Name, false, h.getInterval())
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
