// pkg/proxy/handler.go
package proxy

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"strconv"
	"time"
)

// ProxyHandler handles the incoming requests and manages retries/load balancing
type ProxyHandler struct {
	lb             *LoadBalancer
	pool           *BackendPool
	health         *Health
	proxy          *httputil.ReverseProxy
	modifyResponse func(*http.Response) error
}

func NewProxyHandler(lb *LoadBalancer, pool *BackendPool, health *Health, proxy *httputil.ReverseProxy) *ProxyHandler {
	return &ProxyHandler{
		lb:             lb,
		pool:           pool,
		health:         health,
		proxy:          proxy,
		modifyResponse: ModifyResponse(), // Use the internal one or pass it in
	}
}

func (h *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	RecordRequestStart()
	defer RecordRequestEnd()
	startTime := time.Now()
	maxRetries := 3

	for attempt := 0; attempt < maxRetries; attempt++ {
		backend, err := h.lb.GetNextPeer()
		if err != nil {
			RecordRetryAttempt(backend.Name)
			log.Printf("Error getting backend: %v", err)
			break
		}
		if backend == nil {
			log.Printf("No backend available")
			break
		}
		// Track active requests
		backend.IncrementActiveRequests()

		// Setup context
		ctx := context.WithValue(r.Context(), "backend", backend)
		ctx = context.WithValue(ctx, "request-timer", time.Now().UTC())

		// Clone the request ensures we don't modify the original request for retries
		outReq := r.Clone(ctx)

		// Prepare request (Director's job)
		// Note: The Director is attached to h.proxy, so h.proxy.ServeHTTP would call it.
		// But here we are manually calling RoundTrip to handle retries.
		// We need to invoke the Director manually if we use Transport.RoundTrip directly.
		if h.proxy.Director != nil {
			h.proxy.Director(outReq)
		}

		// Try backend
		resp, err := h.proxy.Transport.RoundTrip(outReq)

		if err != nil {
			RecordBackendFailure(backend.Name)
			backend.DecrementActiveRequests()
			h.pool.UpdateBackendStatus(backend.Name, false, h.health.GetHealthCheckInterval())
			log.Printf("Backend %s failed: %v, retrying...", backend.Name, err)
			continue
		}

		// Success! Now modify response BEFORE writing to client
		defer resp.Body.Close()

		if h.proxy.ModifyResponse != nil {
			err = h.proxy.ModifyResponse(resp)
			if err != nil {
				backend.DecrementActiveRequests()
				log.Printf("ModifyResponse error: %v", err)
				continue // ← Retry with next backend
			}
		}

		// Copy headers
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}

		// Write status and body
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)

		backend.DecrementActiveRequests()
		duration := time.Since(startTime).Seconds()
		RecordRequest(backend.Name, r.Method, strconv.Itoa(resp.StatusCode), duration)
		log.Printf("Request succeeded via %s", backend.Name)
		return
	}

	// All retries failed
	log.Printf("All retries exhausted after %d attempts", maxRetries)
	http.Error(w, "All backends failed", http.StatusBadGateway)
}
