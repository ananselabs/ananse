package proxy

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type State int

const (
	Closed State = iota
	HalfOpen
	Open
)

type Backend struct {
	Name           string
	TargetUrl      *url.URL
	Healthy        bool
	ActiveRequest  int32
	MaxRequest     int
	FailureCount   int
	resetTimeOut   time.Duration
	backofDuration time.Duration
	state          State
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
		backend.backofDuration = 0
		backend.state = Closed
		backend.resetTimeOut = 0
		log.Printf("Backend %s marked healthy", backend.Name)
	}

	// if failure count is greater than 5 open the circuit set the next check time to 3 sec * the time remaining
	if backend.FailureCount == 5 {
		if backend.state == HalfOpen {
			backend.resetTimeOut = backend.backofDuration + 3*time.Second + bp.HealthCheckInterval*2
			backend.state = Open
		} else {
			backend.state = Open
			backend.resetTimeOut = backend.resetTimeOut + 3*time.Second + bp.HealthCheckInterval
		}
		if backend.backofDuration != 0 {
			backend.backofDuration = backend.backofDuration * 2
		} else {
			backend.backofDuration = 3 * time.Second
		}
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
			bp.mu.Lock()

			shouldCheck := true

			if backend.state == Open {
				if backend.resetTimeOut >= bp.HealthCheckInterval {
					backend.resetTimeOut -= bp.HealthCheckInterval
					fmt.Printf("%s next check time is %s\n", backend.Name, backend.resetTimeOut)
					shouldCheck = false
				} else {
					backend.state = HalfOpen
				}
			}

			if backend.state == HalfOpen {
				backend.FailureCount -= 1
			}

			bp.mu.Unlock()

			if shouldCheck {
				go bp.checkBackend(backend)
			}
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
