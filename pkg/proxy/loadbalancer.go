// loadbalancer.go
package proxy

import (
	"errors"
	"sync"
)

type LoadBalancer struct {
	Strategy []string
	Current  string
	mu       sync.RWMutex
	pool     *BackendPool
	current  int
}

func NewLoadBalancer(current string, pool *BackendPool) *LoadBalancer {
	return &LoadBalancer{Current: current, pool: pool}
}

func (lb *LoadBalancer) GetNextPeer() (*Backend, error) {
	switch lb.Current {
	case "least-connections":
		backend, err := lb.getNextLeastConnection()
		if err != nil {
			return nil, err
		}
		return backend, nil
	case "round-robin":
		return lb.getNextRoundRobin(), nil
	default:
		return lb.getNextRoundRobin(), nil
	}
}

func (lb *LoadBalancer) getNextRoundRobin() *Backend {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	start := lb.current
	backendCount := lb.pool.GetBackendCount()

	for i := 0; i < backendCount; i++ {
		idx := (start + i) % backendCount
		if lb.pool.IsBackendHealthy(idx) {
			lb.current = (idx + 1) % backendCount
			return lb.pool.GetBackendAtIndex(idx)
		}
	}
	return nil
}

func (lb *LoadBalancer) getNextLeastConnection() (*Backend, error) {
	// Get all backends (already has locking)
	backends := lb.pool.GetAllBackends()

	var leastConnected *Backend
	for _, backend := range backends {
		if !backend.Healthy {
			continue
		}

		if leastConnected == nil {
			leastConnected = backend
			continue
		}

		if backend.ActiveRequest < leastConnected.ActiveRequest {
			leastConnected = backend
		}
	}

	if leastConnected == nil {
		return nil, errors.New("no healthy backends")
	}
	return leastConnected, nil
}
