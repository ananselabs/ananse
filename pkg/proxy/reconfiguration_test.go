package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"sync"
	"testing"
	"time"
)

func TestBackendDraining(t *testing.T) {
	// 1. Setup Backend A (Old)
	// This backend simulates a long running request
	var requestStarted sync.WaitGroup
	requestStarted.Add(1)

	b1 := createMockBackend("b1", func(w http.ResponseWriter, r *http.Request) {
		requestStarted.Done()       // Signal that request has reached backend
		time.Sleep(2 * time.Second) // Simulate processing time
		w.WriteHeader(http.StatusOK)
	})
	// We don't defer b1.Close() immediately because the pool should close it.
	// But we should defer it just in case the test fails to avoid leaks.
	defer b1.Close()

	target1 := MustParse(b1.URL)
	backend1 := &Backend{Name: "b1", TargetUrl: target1, Healthy: true}

	// 2. Setup Backend B (New)
	b2 := createMockBackend("b2", nil)
	defer b2.Close()
	target2 := MustParse(b2.URL)
	backend2 := &Backend{Name: "b2", TargetUrl: target2, Healthy: true}

	// 3. Initialize Pool with Backend A
	pool := NewBackendPool([]*Backend{backend1})
	lb := NewLoadBalancer("round-robin", pool)

	// We need the health check or at least a dummy one?
	// The handler uses it.
	health := NewHealthCheck(pool, 100*time.Millisecond)
	go health.Check()

	proxy := &httputil.ReverseProxy{
		Director:       Director(),
		Transport:      Transport(),
		ModifyResponse: ModifyResponse(),
	}

	handler := NewProxyHandler(lb, pool, health, proxy)
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	// 4. Start a long running request to Backend A
	go func() {
		client := proxyServer.Client()
		resp, err := client.Get(proxyServer.URL)
		if err != nil {
			// This might fail if the server is closed abruptly, which is what we want to avoid
			t.Logf("Long request finished: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Long request failed with status %d", resp.StatusCode)
		}
	}()

	// Wait for request to be in-flight
	requestStarted.Wait()

	// Give it a tiny bit more time to ensure IncrementActiveRequests was called
	// (Actually createMockBackend is called AFTER the director/transport, so ActiveRequests is already incremented)
	time.Sleep(50 * time.Millisecond)

	// Verify ActiveRequests is 1
	// We need to find backend1 in the pool (it's at index 0)
	if pool.GetBkActiveRequests(0) != 1 {
		t.Errorf("Expected 1 active request, got %d", pool.GetBkActiveRequests(0))
	}

	// 5. Trigger UpdateBackend to swap to Backend B
	t.Log("Updating backend pool...")
	pool.UpdateBackend([]*Backend{backend2})

	// 6. Verify new requests go to Backend B immediately
	client := proxyServer.Client()
	resp, err := client.Get(proxyServer.URL)
	if err != nil {
		t.Fatalf("New request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Backend-Name") != "b2" {
		t.Errorf("Expected response from b2, got %s", resp.Header.Get("X-Backend-Name"))
	}

	// 7. Verify Backend A is NOT yet closed (it has 1 active request)
	backend1.mu.RLock()
	if backend1.closed {
		t.Error("Backend 1 should NOT be closed yet (request still active)")
	}
	backend1.mu.RUnlock()

	// 8. Wait for the long request to finish (approx 2s total, we already waited a bit)
	t.Log("Waiting for long request to finish...")
	time.Sleep(2500 * time.Millisecond)

	// 9. Verify Backend A IS now closed
	backend1.mu.RLock()
	if !backend1.closed {
		t.Error("Backend 1 SHOULD be closed now (request finished)")
	}
	backend1.mu.RUnlock()
}
