package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

var (
	isHealthy = true
	mu        sync.RWMutex
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// 1. Simulation: Latency
		sleep := r.URL.Query().Get("sleep")
		if sleep != "" {
			if sleepMs, err := strconv.Atoi(sleep); err == nil {
				fmt.Printf("Sleeping for %dms\n", sleepMs)
				time.Sleep(time.Duration(sleepMs) * time.Millisecond)
			}
		}

		// 2. Simulation: Forced Status Code
		if codeStr := r.URL.Query().Get("code"); codeStr != "" {
			if code, err := strconv.Atoi(codeStr); err == nil {
				w.WriteHeader(code)
				if code >= 400 {
					json.NewEncoder(w).Encode(map[string]string{
						"error": fmt.Sprintf("Forced error: %d", code),
					})
					return
				}
			}
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"service":          "payments",
			"balance":          1000,
			"received_headers": r.Header,
		})
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		if !isHealthy {
			http.Error(w, "Service Unhealthy", http.StatusInternalServerError)
			return
		}
		w.Write([]byte("Ok"))
	})

	http.HandleFunc("/health/toggle", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		mu.Lock()
		isHealthy = !isHealthy
		newStatus := isHealthy
		mu.Unlock()

		msg := fmt.Sprintf("Health toggled to: %v", newStatus)
		log.Println(msg)
		w.Write([]byte(msg))
	})

	log.Println("Payment service listening on :5003")
	log.Fatal(http.ListenAndServe(":5003", nil))
}
