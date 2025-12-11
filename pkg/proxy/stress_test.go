package proxy

import (
	"net/url"
	"sync"
	"testing"
	"time"
)

func TestConcurrencyStress(t *testing.T) {
	// 1. Setup
	backends := []*Backend{
		{Name: "b1", TargetUrl: &url.URL{}, Healthy: true},
		{Name: "b2", TargetUrl: &url.URL{}, Healthy: true},
		{Name: "b3", TargetUrl: &url.URL{}, Healthy: true},
	}
	pool := NewBackendPool(backends)
	lb := NewLoadBalancer("least-connections", pool)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 2. Simulate Health Checks / Updates (The Writer)
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(1 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				// Toggle health status
				name := backends[i%len(backends)].Name
				pool.UpdateBackendStatus(name, i%2 == 0, 1*time.Second)
				i++
			}
		}
	}()

	// 3. Simulate Request Load (The Readers/Writers of ActiveRequest)
	// Create 50 concurrent request simulators
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					// This calls getNextLeastConnection
					b, err := lb.GetNextPeer()
					if err == nil && b != nil {
						// Simulate processing using new thread-safe methods
						b.IncrementActiveRequests()
						time.Sleep(time.Microsecond * 10)
						b.DecrementActiveRequests()
					}
				}
			}
		}()
	}

	// Run for a short duration
	time.Sleep(2 * time.Second)
	close(stop)
	wg.Wait()
}
