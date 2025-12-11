package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"sync"
	"testing"
	"time"
)

// Helper to create a mock backend
func createMockBackend(name string, behavior func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	mux := http.NewServeMux()

	// If custom behavior is provided, let it handle all paths including /health
	if behavior != nil {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Backend-Name", name)
			behavior(w, r)
		})
	} else {
		// Default handlers when no custom behavior
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Backend-Name", name)
			if r.URL.Query().Get("fail_health") == "true" {
				http.Error(w, "Health Check Failed", http.StatusInternalServerError)
			} else {
				w.WriteHeader(http.StatusOK)
				io.WriteString(w, "OK")
			}
		})
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Backend-Name", name)
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "Hello from "+name)
		})
	}
	return httptest.NewServer(mux)
}

func TestProxyIntegration(t *testing.T) {

	t.Run("LoadBalancing", func(t *testing.T) {
		// Setup Mock Backends
		b1 := createMockBackend("b1", nil) // Healthy
		defer b1.Close()
		b2 := createMockBackend("b2", nil) // Healthy
		defer b2.Close()

		// Setup Proxy Components for this subtest
		target1 := MustParse(b1.URL)
		target2 := MustParse(b2.URL)

		backends := []*Backend{
			{Name: "b1", TargetUrl: target1, Healthy: true},
			{Name: "b2", TargetUrl: target2, Healthy: true},
		}

		pool := NewBackendPool(backends)
		lb := NewLoadBalancer("round-robin", pool)
		health := NewHealthCheck(pool, 100*time.Millisecond) // Fast check for testing
		go health.Check()

		// Allow some time for initial health checks to run
		time.Sleep(200 * time.Millisecond)

		proxy := &httputil.ReverseProxy{
			Director:  Director(),
			Transport: Transport(),
			// ErrorHandler is handled by NewProxyHandler's loop
			ModifyResponse: ModifyResponse(),
		}

		handler := NewProxyHandler(lb, pool, health, proxy)
		proxyServer := httptest.NewServer(handler)
		defer proxyServer.Close()

		client := proxyServer.Client()

		counts := make(map[string]int)
		expectedRequests := 10
		for i := 0; i < expectedRequests; i++ {
			resp, err := client.Get(proxyServer.URL)
			if err != nil {
				t.Errorf("Request %d failed: %v", i, err)
				continue
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("Request %d: Expected status 200, got %d", i, resp.StatusCode)
				continue
			}

			bkName := resp.Header.Get("X-Backend-Name")
			counts[bkName]++
		}

		if counts["b1"] == 0 || counts["b2"] == 0 {
			t.Errorf("Load balancing failed, expected distribution across b1 and b2, got: %v", counts)
		}
		if counts["b1"]+counts["b2"] != expectedRequests {
			t.Errorf("Total requests mismatch, expected %d, got %d", expectedRequests, counts["b1"]+counts["b2"])
		}
	})

	t.Run("RetryLogic_TransportError", func(t *testing.T) {
		// Setup backends for this subtest
		// b_dead will simulate connection refused by being closed immediately
		b_dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// This handler won't even be reached if connection is refused
			panic("should not be reached")
		}))
		b_dead.Close() // Simulate connection refused

		b_ok := createMockBackend("b_ok", nil)
		defer b_ok.Close()

		target_dead := MustParse(b_dead.URL)
		target_ok := MustParse(b_ok.URL)

		backendsRetry := []*Backend{
			{Name: "b_dead", TargetUrl: target_dead, Healthy: true},
			{Name: "b_ok", TargetUrl: target_ok, Healthy: true},
		}

		poolRetry := NewBackendPool(backendsRetry)
		lbRetry := NewLoadBalancer("round-robin", poolRetry)
		healthRetry := NewHealthCheck(poolRetry, 100*time.Millisecond)
		go healthRetry.Check()

		// Allow some time for initial health checks to run.
		// Health check on b_dead will fail, but b_ok should become healthy.
		time.Sleep(200 * time.Millisecond)

		proxyRetry := &httputil.ReverseProxy{
			Director:  Director(),
			Transport: Transport(),
			// ErrorHandler is handled by NewProxyHandler's loop
			ModifyResponse: ModifyResponse(),
		}

		handlerRetry := NewProxyHandler(lbRetry, poolRetry, healthRetry, proxyRetry)
		serverRetry := httptest.NewServer(handlerRetry)
		defer serverRetry.Close()

		// We expect the request to succeed because b_dead fails initially,
		// and the retry logic should pick b_ok.
		resp, err := serverRetry.Client().Get(serverRetry.URL)
		if err != nil {
			t.Fatalf("Expected successful request after retry, got error: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status 200 OK, got %d", resp.StatusCode)
		}

		if name := resp.Header.Get("X-Backend-Name"); name != "b_ok" {
			t.Errorf("Expected response from b_ok, got from %s", name)
		}
	})

	t.Run("CircuitBreaker", func(t *testing.T) {
		// Backend that consistently fails (returns 500)
		b_failing := createMockBackend("b_failing", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				http.Error(w, "Health Check Failed", http.StatusInternalServerError)
			} else {
				http.Error(w, "Always Fails", http.StatusInternalServerError)
			}
		})
		defer b_failing.Close()

		target_failing := MustParse(b_failing.URL)

		backendFailing := &Backend{Name: "b_failing", TargetUrl: target_failing, Healthy: true, state: Closed}
		poolCB := NewBackendPool([]*Backend{backendFailing})

		// Use a fast health check interval
		healthCB := NewHealthCheck(poolCB, 50*time.Millisecond)
		go healthCB.Check()

		// Allow time for health checks to run and mark the backend unhealthy
		time.Sleep(5 * healthCB.checkInterval * 6) // Wait for more than 5 failures to open circuit

		// The health check marks backendFailing as unhealthy (Healthy = false).
		// The circuit breaker in UpdateStatus will transition it to Open state.

		if backendFailing.IsHealthy() {
			t.Errorf("Backend should have been marked unhealthy by health check. Current state: %v, Healthy: %t", backendFailing.state, backendFailing.IsHealthy())
		}

		lbCB := NewLoadBalancer("round-robin", poolCB)

		// This should now attempt to get a peer. Since b_failing is the only one and unhealthy,
		// GetNextPeer should return an error.
		b, err := lbCB.GetNextPeer()
		if err == nil {
			if b != nil {
				t.Errorf("Expected GetNextPeer to return an error as no healthy backends are available, got: %v", b.Name)
			} else {
				t.Errorf("Expected GetNextPeer to return an error as no healthy backends are available")
			}
		}
		if b != nil {
			t.Errorf("Expected GetNextPeer to return nil backend when no healthy backends, got: %v", b.Name)
		}
	})

	t.Run("ConcurrencyStress", func(t *testing.T) {
		// Setup Mock Backends
		b1 := createMockBackend("b1_stress", nil)
		defer b1.Close()
		b2 := createMockBackend("b2_stress", func(w http.ResponseWriter, r *http.Request) {
			// Health checks should always succeed for this test
			if r.URL.Path == "/health" {
				w.WriteHeader(http.StatusOK)
				io.WriteString(w, "OK")
				return
			}
			// Simulate a flaky backend with some delays for regular requests
			if time.Now().Unix()%2 == 0 {
				time.Sleep(50 * time.Millisecond)
				http.Error(w, "Flaky Error", http.StatusInternalServerError)
			} else {
				io.WriteString(w, "Hello from b2_stress")
			}
		})
		defer b2.Close()

		target1 := MustParse(b1.URL)
		target2 := MustParse(b2.URL)

		backends := []*Backend{
			{Name: "b1_stress", TargetUrl: target1, Healthy: true},
			{Name: "b2_stress", TargetUrl: target2, Healthy: true},
		}

		pool := NewBackendPool(backends)
		lb := NewLoadBalancer("round-robin", pool)
		health := NewHealthCheck(pool, 50*time.Millisecond) // Fast check for testing
		go health.Check()

		time.Sleep(200 * time.Millisecond) // Allow health checks to stabilize

		proxy := &httputil.ReverseProxy{
			Director:  Director(),
			Transport: Transport(),
			// ErrorHandler is handled by NewProxyHandler's loop
			ModifyResponse: ModifyResponse(),
		}

		handler := NewProxyHandler(lb, pool, health, proxy)
		proxyServer := httptest.NewServer(handler)
		defer proxyServer.Close()

		var wg sync.WaitGroup
		concurrentRequests := 100

		for i := 0; i < concurrentRequests; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond) // Increased timeout
				defer cancel()

				req, _ := http.NewRequestWithContext(ctx, "GET", proxyServer.URL, nil)
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					resp.Body.Close()
				}
			}()
		}
		wg.Wait()
		// Just checking for race conditions and panics during concurrent operations
		// and ensuring the overall system remains stable.
	})
}
