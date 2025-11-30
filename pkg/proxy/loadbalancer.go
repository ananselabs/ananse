package proxy

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type Backend struct {
	Name          string
	TargetUrl     *url.URL
	Healthy       bool
	ActiveRequest int32
	MaxRequest    int
	FailureCount  int
}

type BackendPool struct {
	Backends            []*Backend
	Strategy            string
	HealthCheckInterval time.Duration
	mu                  sync.RWMutex // Round-robin index
	current             int
}

func NewBackendPool(backends []*Backend, strategy string, healthCheckInterval time.Duration) *BackendPool {
	bp := &BackendPool{
		Backends:            backends,
		Strategy:            strategy,
		HealthCheckInterval: healthCheckInterval,
	}
	// Start a background goroutine for health checks
	go bp.HealthCheck()
	return bp
}

func (bp *BackendPool) UpdateBackendStatus(backend *Backend, healthy bool) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	backend.Healthy = healthy
	if !healthy {
		backend.FailureCount++
		log.Printf("Backend %s marked unhealthy (failures: %d)", backend.Name, backend.FailureCount)
	} else {
		backend.FailureCount = 0
		log.Printf("Backend %s marked healthy", backend.Name)
	}

}

func (bp *BackendPool) GetNextPeer() *Backend {
	switch bp.Strategy {
	case "least-connections":
		return bp.getNextLeastConnection()
	case "round-robin":
		return bp.getNextRoundRobin()
	default:
		return bp.getNextRoundRobin()
	}
}

func (bp *BackendPool) getNextRoundRobin() *Backend {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	start := bp.current

	for i := 0; i < len(bp.Backends); i++ {
		idx := (start + i) % len(bp.Backends)
		if bp.Backends[idx].Healthy {
			bp.current = (idx + 1) % len(bp.Backends)
			return bp.Backends[idx]
		}
	}
	return nil
}

func (bp *BackendPool) HealthCheck() {
	ticker := time.NewTicker(bp.HealthCheckInterval)
	defer ticker.Stop()

	for range ticker.C {
		for _, backend := range bp.Backends {
			go bp.checkBackend(backend)
		}
	}
}

func (bp *BackendPool) checkBackend(backend *Backend) {
	// Build health check URL
	healthURL := *backend.TargetUrl
	healthURL.Path = "/health"

	// Make request with timeout
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(healthURL.String())

	if err != nil {
		bp.UpdateBackendStatus(backend, false)
		return
	}
	defer resp.Body.Close()

	// Consider 200-299 as healthy
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		bp.UpdateBackendStatus(backend, true)
	} else {
		bp.UpdateBackendStatus(backend, false)
	}
}

func (bp *BackendPool) getNextLeastConnection() *Backend {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	var leastConnected *Backend
	for i := 0; i < len(bp.Backends); i++ {
		if !bp.Backends[i].Healthy {
			continue
		}

		if leastConnected == nil {
			leastConnected = bp.Backends[i]
			continue
		}

		if bp.Backends[i].ActiveRequest < leastConnected.ActiveRequest {
			leastConnected = bp.Backends[i]
		}
	}
	fmt.Printf("%s has the least connection of %d active connections\n ", leastConnected.Name, leastConnected.ActiveRequest)
	return leastConnected
}

func (bp *BackendPool) RemoveBackend(backend *Backend) {
}

func (bp *BackendPool) AddBackend(backend *Backend) {
}

func (bp *BackendPool) GetNumberOfAliveBackends() int {
	return 0
}

func (bp *BackendPool) GetBackendByName(name string) *Backend {
	for _, b := range bp.Backends {
		if b.Name == name {
			return b
		}
	}
	return nil
}
