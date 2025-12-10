// loadbalancer.go
package proxy

import (
	"sync"
)

type LoadBalancer struct {
	Strategy []string
	Current  string
	mu       sync.RWMutex
	pool     *BackendPool
}

func NewLoadBalancer(current string, pool *BackendPool) *LoadBalancer {
	return &LoadBalancer{Current: current, pool: pool}
}

func (lb *LoadBalancer) GetNextPeer() (*Backend, error) {
	switch lb.Current {
	case "least-connections":
		backend, err := lb.pool.GetNextLeastConnection()
		if err != nil {
			return nil, err
		}
		return backend, nil
	case "round-robin":
		return lb.pool.GetNextRoundRobin(), nil
	default:
		return lb.pool.GetNextRoundRobin(), nil
	}
}
