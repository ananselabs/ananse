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
	Backends map[string][]*Backend
	mu       sync.RWMutex
}

func NewBackendPool(backends map[string][]*Backend) *BackendPool {
	bp := &BackendPool{
		Backends: backends,
	}
	return bp
}

func (bp *BackendPool) UpdateBackends(newBackends map[string][]*Backend) {
	bp.mu.Lock()
	oldBackends := bp.Backends
	bp.Backends = newBackends
	bp.mu.Unlock()

	go bp.drainRemovedBackends(oldBackends, newBackends)
}

func (bp *BackendPool) drainRemovedBackends(oldMap, newMap map[string][]*Backend) {
	removed := bp.findRemovedBackends(oldMap, newMap)

	for _, backend := range removed {
		b := backend // capture

		go func() {
			timeout := time.After(30 * time.Second)
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()

			for {
				select {
				case <-timeout:
					fmt.Printf(
						"Backend %s still has %d active requests after 30s, force closing\n",
						b.Name, b.GetActiveRequests(),
					)
					b.Close()
					return

				case <-ticker.C:
					if b.GetActiveRequests() == 0 {
						fmt.Printf("Backend %s drained, closing\n", b.Name)
						b.Close()
						return
					}
				}
			}
		}()
	}
}

func (bp *BackendPool) findRemovedBackends(oldMap, newMap map[string][]*Backend) []*Backend {

	var removed []*Backend

	for service, oldBackends := range oldMap {
		newBackends := make(map[string]struct{})

		for _, b := range newMap[service] {
			newBackends[b.Name] = struct{}{}
		}

		for _, b := range oldBackends {
			if _, exists := newBackends[b.Name]; !exists {
				removed = append(removed, b)
			}
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

func (bp *BackendPool) GetBackendsForService(service string) ([]*Backend, bool) {
	bp.mu.RLock()
	defer bp.mu.RUnlock()

	backends, ok := bp.Backends[service]
	if !ok {
		return nil, false
	}

	out := make([]*Backend, len(backends))
	copy(out, backends)
	return out, true
}

func (bp *BackendPool) GetServiceBackendCount(service string) (int, bool) {
	bp.mu.RLock()
	defer bp.mu.RUnlock()

	backends, ok := bp.Backends[service]
	if !ok {
		return 0, false
	}
	return len(backends), true
}

// GetTotalBackendCount returns number of backends
func (bp *BackendPool) GetTotalBackendCount() int {
	bp.mu.RLock()
	defer bp.mu.RUnlock()

	count := 0
	for _, backends := range bp.Backends {
		count += len(backends)
	}
	return count
}

func (bp *BackendPool) GetAllBackends() []*Backend {
	bp.mu.RLock()
	defer bp.mu.RUnlock()

	var result []*Backend
	for _, backends := range bp.Backends {
		result = append(result, backends...)
	}
	return result
}

func (bp *BackendPool) GetAllServices() []string {
	bp.mu.RLock()
	defer bp.mu.RUnlock()
	var services []string
	for service, _ := range bp.Backends {
		services = append(services, service)
	}
	return services
}

func (bp *BackendPool) GetCircuitState(service string, backendName string, checkInterval time.Duration) (bool, State) {

	backends, ok := bp.GetBackendsForService(service)
	if !ok {
		return false, Closed
	}

	for _, b := range backends {
		if b.Name == backendName {
			return b.GetCircuitState(checkInterval)
		}
	}
	return false, Closed
}

func (bp *BackendPool) UpdateBackendStatus(service string, backendName string, healthy bool, checkInterval time.Duration) {
	bp.mu.RLock()
	backends, ok := bp.Backends[service]
	bp.mu.RUnlock()

	if !ok {
		return
	}

	for _, b := range backends {
		if b.Name == backendName {
			b.UpdateStatus(healthy, checkInterval)
			return
		}
	}
}
