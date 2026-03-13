// loadbalancer.go
package proxy

import (
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
)

type LoadBalancer struct {
	Strategy string
	mu       sync.RWMutex
	pool     *BackendPool
	current  int
	// Track round-robin index per service
	rrIndices map[string]int
}

func NewLoadBalancer(strategy string, pool *BackendPool) *LoadBalancer {
	return &LoadBalancer{Strategy: strategy, pool: pool, rrIndices: make(map[string]int)}
}

func (lb *LoadBalancer) GetNextPeer(service string) (*Backend, error) {
	switch lb.Strategy {
	case "least-connections":
		backend, err := lb.getNextLeastConnection(service)
		if err != nil {
			return nil, err
		}
		return backend, nil
	case "round-robin":
		backend := lb.getNextRoundRobin(service)
		if backend == nil {
			return nil, errors.New("no healthy backends")
		}
		return backend, nil
	default:
		backend := lb.getNextRoundRobin(service)
		if backend == nil {
			return nil, errors.New("no healthy backends")
		}
		return backend, nil
	}
}

func (lb *LoadBalancer) getNextRoundRobin(service string) *Backend {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	backends, ok := lb.pool.GetBackendsForService(service)
	if !ok || len(backends) == 0 {
		return nil
	}

	if _, exists := lb.rrIndices[service]; !exists {
		lb.rrIndices[service] = 0
	}

	start := lb.rrIndices[service]
	for i := 0; i < len(backends); i++ {
		idx := (start + i) % len(backends)
		b := backends[idx]

		shouldCheck, state := lb.pool.GetCircuitState(service, b.Name, 10*time.Second)

		if !shouldCheck {
			Logger.Info("skipping backend (circuit open)",
				zap.String("backend", b.Name),
				zap.String("state", string(state)))
			continue
		}

		if b.IsHealthy() && shouldCheck {
			return b
		}
	}

	return nil
}

func (lb *LoadBalancer) getNextLeastConnection(service string) (*Backend, error) {
	backends, ok := lb.pool.GetBackendsForService(service)
	if !ok || len(backends) == 0 {
		return nil, errors.New("service not found or no backends")
	}

	var least *Backend
	for _, b := range backends {
		// ✅ Check BOTH health AND circuit state
		shouldCheck, _ := lb.pool.GetCircuitState(service, b.Name, 10*time.Second)

		if !b.IsHealthy() || !shouldCheck {
			continue
		}

		if least == nil || b.GetActiveRequests() < least.GetActiveRequests() {
			least = b
		}
	}

	if least == nil {
		return nil, errors.New("no healthy backends")
	}
	return least, nil
}
