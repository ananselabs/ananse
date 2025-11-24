package main

import (
	"log"
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
