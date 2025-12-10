// backend.go
package proxy

import (
	"errors"
	"log"
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
	Backends []*Backend
	current  int
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

func (bp *BackendPool) Current() int {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return bp.current
}
func (bp *BackendPool) UpdateCurrent(index int) {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	bp.current = index
}

func (bp *BackendPool) IsHealthy(index int) bool {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return bp.Backends[index].Healthy
}

func (bp *BackendPool) GetBackend(index int) *Backend {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return bp.Backends[index]
}

func (bp *BackendPool) PoolSize() int {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return len(bp.Backends)
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

func (bp *BackendPool) GetNextRoundRobin() *Backend {
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

func (bp *BackendPool) GetNextLeastConnection() (*Backend, error) {
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

	if leastConnected == nil {
		return nil, errors.New("no healthy backends")
	}
	return leastConnected, nil
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
