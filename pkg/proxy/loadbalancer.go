// loadbalancer.go
package proxy

import (
	"errors"
	"sync"
)

type LoadBalancer struct {
	Strategy string
	mu       sync.RWMutex
	pool     *BackendPool
	current  int
}

func NewLoadBalancer(strategy string, pool *BackendPool) *LoadBalancer {
	return &LoadBalancer{Strategy: strategy, pool: pool}
}

func (lb *LoadBalancer) GetNextPeer() (*Backend, error) {
	switch lb.Strategy {
	case "least-connections":
		backend, err := lb.getNextLeastConnection()
		if err != nil {
			return nil, err
		}
		return backend, nil
	case "round-robin":
		backend := lb.getNextRoundRobin()
		if backend == nil {
			return nil, errors.New("no healthy backends")
		}
		return backend, nil
	default:
		backend := lb.getNextRoundRobin()
		if backend == nil {
			return nil, errors.New("no healthy backends")
		}
		return backend, nil
	}
}

func (lb *LoadBalancer) getNextRoundRobin() *Backend {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	start := lb.current
	backendCount := lb.pool.GetBackendCount()

	for i := 0; i < backendCount; i++ {
		idx := (start + i) % backendCount
		backend := lb.pool.GetBackendAtIndex(idx)
		if backend != nil && backend.IsHealthy() {
			lb.current = (idx + 1) % backendCount
			return backend
		}
	}
	return nil
}

func (lb *LoadBalancer) getNextLeastConnection() (*Backend, error) {
	// Get all backends (already has locking)
	backends := lb.pool.GetAllBackends()

	var leastConnected *Backend
	for _, backend := range backends {
		if !backend.IsHealthy() {
			continue
		}

		if leastConnected == nil {
			leastConnected = backend
			continue
		}

		if backend.GetActiveRequests() < leastConnected.GetActiveRequests() {
			leastConnected = backend
		}
	}

	if leastConnected == nil {
		return nil, errors.New("no healthy backends")
	}
	return leastConnected, nil
}
