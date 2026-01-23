package main

import (
	"ananse/pkg/proxy"
	"context"
	"encoding/json"
	"fmt"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

var (
	isHealthy = true
	mu        sync.RWMutex
)

func main() {
	shutdown := proxy.InitTracer()
	defer shutdown(context.Background())

	http.HandleFunc("/", handleAnalytics)
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

	port := os.Getenv("PORT")
	if port == "" {
		port = "5004"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: nil,
	}

	go func() {
		log.Printf("Analytics service listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Println("Server exiting")
}

func handleAnalytics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ctx := otel.GetTextMapPropagator().Extract(
		r.Context(),
		propagation.HeaderCarrier(r.Header),
	)
	ctx, span := otel.Tracer("analytics").Start(ctx, "analytics.handle_request",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("http.method", r.Method),
			attribute.String("http.url", r.URL.Path),
			attribute.String("http.host", r.Host),
		),
	)
	defer span.End()

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
		"service":          "analytics",
		"events":           []string{},
		"received_headers": r.Header,
	})
}
