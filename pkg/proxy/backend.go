// backend.go
package proxy

import (
	"log"
	"net/url"
	"sync"
	"time"
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
	Backends []*Backend
	mu       sync.RWMutex
}

func NewBackendPool(backends []*Backend) *BackendPool {
	bp := &BackendPool{
		Backends: backends,
	}
	return bp
}

func (bp *BackendPool) GetBackendByName(name string) *Backend {
	for _, b := range bp.Backends {
		if b.Name == name {
			return b
		}
	}
	return nil
}

// GetBackendCount returns number of backends
func (bp *BackendPool) GetBackendCount() int {
	bp.mu.RLock()
	defer bp.mu.RUnlock()
	return len(bp.Backends)
}

// GetBackendAtIndex returns backend at given index (thread-safe)
func (bp *BackendPool) GetBackendAtIndex(idx int) *Backend {
	bp.mu.RLock()
	defer bp.mu.RUnlock()
	if idx < 0 || idx >= len(bp.Backends) {
		return nil
	}
	return bp.Backends[idx]
}

// IsBackendHealthy checks if backend at index is healthy
func (bp *BackendPool) IsBackendHealthy(idx int) bool {
	bp.mu.RLock()
	defer bp.mu.RUnlock()
	if idx < 0 || idx >= len(bp.Backends) {
		return false
	}
	return bp.Backends[idx].Healthy
}

func (bp *BackendPool) GetPool() []*Backend {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return bp.Backends
}

func (bp *BackendPool) GetBkActiveRequests(index int) int32 {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return bp.Backends[index].ActiveRequest
}

func (bp *BackendPool) GetAllBackends() []*Backend {
	bp.mu.RLock()
	defer bp.mu.RUnlock()

	backends := make([]*Backend, len(bp.Backends))
	copy(backends, bp.Backends)
	return backends
}

func (bp *BackendPool) GetCircuitState(name string, checkinterval time.Duration) (shouldCheck bool, state State) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	backend := bp.GetBackendByName(name)
	if backend == nil {
		return false, Closed
	}

	// Handle circuit state transitions
	if backend.state == Open {
		if backend.resetTimeOut >= checkinterval {
			backend.resetTimeOut -= checkinterval
			log.Printf("%s next check time is %s\n", backend.Name, backend.resetTimeOut)
			return false, Open
		} else {
			backend.state = HalfOpen
			backend.FailureCount--
			return true, HalfOpen
		}
	}

	if backend.state == HalfOpen {
		backend.FailureCount--
	}

	return true, backend.state
}

func (bp *BackendPool) UpdateBackendStatus(name string, healthy bool, checkInterval time.Duration) {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	backend := bp.GetBackendByName(name)
	if backend == nil {
		return
	}
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
	if backend.FailureCount >= 5 {
		if backend.state == HalfOpen {
			backend.resetTimeOut = backend.backofDuration + 3*time.Second + checkInterval*2
			backend.state = Open
		} else {
			backend.state = Open
			backend.resetTimeOut = backend.resetTimeOut + 3*time.Second + checkInterval
		}
		if backend.backofDuration != 0 {
			backend.backofDuration = backend.backofDuration * 2
		} else {
			backend.backofDuration = 3 * time.Second
		}
	}

}
