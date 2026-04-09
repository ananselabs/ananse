package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"ananse/pkg/proxy"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

var (
	isHealthy = true
	mu        sync.RWMutex
)

func handleUsers(w http.ResponseWriter, r *http.Request) {
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

	_, span := otel.Tracer("users").Start(ctx, "users.handle_request",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("http.method", r.Method),
			attribute.String("http.url", r.URL.Path),
			attribute.String("http.host", r.Host),
		),
	)
	defer span.End()

	traceID := span.SpanContext().TraceID().String()
	w.Header().Set("Content-Type", "application/json")

	// 1. Simulation: Latency
	sleep := r.URL.Query().Get("sleep")
	if sleep != "" {
		if sleepMs, err := strconv.Atoi(sleep); err == nil {
			proxy.Logger.Info("simulating latency",
				zap.String("trace_id", traceID),
				zap.Int("sleep_ms", sleepMs),
			)
			time.Sleep(time.Duration(sleepMs) * time.Millisecond)
		}
	}

	// 2. Simulation: Forced Status Code
	if codeStr := r.URL.Query().Get("code"); codeStr != "" {
		if code, err := strconv.Atoi(codeStr); err == nil {
			w.WriteHeader(code)
			if code >= 400 {
				proxy.Logger.Warn("forced error",
					zap.String("trace_id", traceID),
					zap.Int("status", code),
				)
				json.NewEncoder(w).Encode(map[string]string{
					"error": "Forced error",
				})
				return
			}
		}
	}

	proxy.Logger.Info("request completed",
		zap.String("trace_id", traceID),
		zap.String("service", "users"),
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
	)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"service":          "users",
		"data":             []string{},
		"received_headers": r.Header,
	})
}

func main() {
	if proxy.Logger == nil {
		proxy.InitLogger()
	}
	defer proxy.Logger.Sync()

	shutdown := proxy.InitTracer()
	defer shutdown(context.Background())

	http.HandleFunc("/", handleUsers)

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		if !isHealthy {
			http.Error(w, "Service Unhealthy", http.StatusInternalServerError)
			return
		}
		w.Write([]byte("OK"))
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

		proxy.Logger.Info("health toggled", zap.Bool("healthy", newStatus))
		w.Write([]byte("Health toggled"))
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "5002"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: nil,
	}

	go func() {
		proxy.Logger.Info("service started", zap.String("service", "users"), zap.String("port", port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			proxy.Logger.Fatal("listen failed", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	proxy.Logger.Info("shutting down", zap.String("service", "users"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		proxy.Logger.Fatal("forced shutdown", zap.Error(err))
	}

	proxy.Logger.Info("service stopped", zap.String("service", "users"))
}
