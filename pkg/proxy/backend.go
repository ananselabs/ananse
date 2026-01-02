// backend.go
package proxy

import (
	"fmt"
	"log"
	"net/url"
	"sync"
	"sync/atomic"
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
	mu             sync.RWMutex
	closed         bool
}

func (b *Backend) IsHealthy() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.Healthy
}

func (b *Backend) GetActiveRequests() int32 {
	return atomic.LoadInt32(&b.ActiveRequest)
}

func (b *Backend) IncrementActiveRequests() {
	atomic.AddInt32(&b.ActiveRequest, 1)
}

func (b *Backend) DecrementActiveRequests() {
	atomic.AddInt32(&b.ActiveRequest, -1)
}

func (b *Backend) UpdateStatus(healthy bool, checkInterval time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.Healthy = healthy
	UpdateBackendHealth(b.Name, healthy)
	if !healthy {
		b.FailureCount++
		RecordBackendFailure(b.Name)
		log.Printf("Backend %s marked unhealthy (failures: %d)", b.Name, b.FailureCount)
	} else {
		b.FailureCount = 0
		b.backofDuration = 0
		b.state = Closed
		b.resetTimeOut = 0
		log.Printf("Backend %s marked healthy", b.Name)
	}

	// if failure count is greater than 5 open the circuit set the next check time to 3 sec * the time remaining
	if b.FailureCount >= 5 {
		if b.state == HalfOpen {
			b.resetTimeOut = b.backofDuration + 3*time.Second + checkInterval*2
			b.state = Open
		} else {
			b.state = Open
			b.resetTimeOut = b.resetTimeOut + 3*time.Second + checkInterval
		}
		if b.backofDuration != 0 {
			b.backofDuration = b.backofDuration * 2
		} else {
			b.backofDuration = 3 * time.Second
		}
		UpdateCircuitBreakerState(b.Name, b.state)
	}
}

func (b *Backend) GetCircuitState(checkInterval time.Duration) (shouldCheck bool, state State) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == Open {
		if b.resetTimeOut >= checkInterval {
			b.resetTimeOut -= checkInterval
			log.Printf("%s next check time is %s\n", b.Name, b.resetTimeOut)
			return false, Open
		} else {
			b.state = HalfOpen
			b.FailureCount--
			return true, HalfOpen
		}
	}

	if b.state == HalfOpen {
		b.FailureCount--
	}

	return true, b.state
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

func (bp *BackendPool) UpdateBackend(newBackends []*Backend) {
	bp.mu.Lock()
	oldBackends := bp.Backends
	bp.Backends = newBackends
	bp.mu.Unlock()

	// Drain removed backends asynchronously
	go bp.drainRemovedBackends(oldBackends, newBackends)
}

func (bp *BackendPool) drainRemovedBackends(old, new []*Backend) {
	removed := bp.findRemovedBackends(old, new)

	for _, backend := range removed {
		// Wait for active requests to reach 0
		timeout := time.After(30 * time.Second)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-timeout:
				fmt.Printf("Backend %s still has %d active requests after 30s, force closing",
					backend.Name, backend.GetActiveRequests())
				backend.Close()
				return

			case <-ticker.C:
				if backend.GetActiveRequests() == 0 {
					fmt.Printf("Backend %s drained, closing connections", backend.Name)
					backend.Close()
					return
				}
			}
		}
	}
}

func (bp *BackendPool) findRemovedBackends(old, new []*Backend) []*Backend {
	newMap := make(map[string]bool)
	for _, b := range new {
		newMap[b.Name] = true
	}

	var removed []*Backend
	for _, b := range old {
		if !newMap[b.Name] {
			removed = append(removed, b)
		}
	}
	return removed
}

func (b *Backend) Close() {
	// Close any connection pools
	// Mark as closed to prevent new connections
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
}

func (bp *BackendPool) GetBackendByName(name string) *Backend {
	// Assuming Backends list doesn't change after init for now,
	// or caller holds lock if it does.
	// Ideally this should lock, but internal usage often holds lock.
	// Let's make it safe.
	bp.mu.RLock()
	defer bp.mu.RUnlock()
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
	// No need to lock pool to check backend health if backend has its own lock
	// But we need to safely get the backend
	b := bp.GetBackendAtIndex(idx)
	if b == nil {
		return false
	}
	return b.IsHealthy()
}

func (bp *BackendPool) GetPool() []*Backend {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return bp.Backends
}

func (bp *BackendPool) GetBkActiveRequests(index int) int32 {
	b := bp.GetBackendAtIndex(index)
	if b == nil {
		return 0
	}
	return b.GetActiveRequests()
}

func (bp *BackendPool) GetAllBackends() []*Backend {
	bp.mu.RLock()
	defer bp.mu.RUnlock()

	backends := make([]*Backend, len(bp.Backends))
	copy(backends, bp.Backends)
	return backends
}

func (bp *BackendPool) GetCircuitState(name string, checkinterval time.Duration) (shouldCheck bool, state State) {
	// Lock pool to find backend? GetBackendByName locks.
	backend := bp.GetBackendByName(name)
	if backend == nil {
		return false, Closed
	}
	return backend.GetCircuitState(checkinterval)
}

func (bp *BackendPool) UpdateBackendStatus(name string, healthy bool, checkInterval time.Duration) {
	backend := bp.GetBackendByName(name)
	if backend == nil {
		return
	}
	backend.UpdateStatus(healthy, checkInterval)
}
