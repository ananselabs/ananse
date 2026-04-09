package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"ananse/pkg/proxy"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

func main() {
	if proxy.Logger == nil {
		proxy.InitLogger()
	}
	defer proxy.Logger.Sync()

	shutdown := proxy.InitTracer()
	defer shutdown(context.Background())

	mux := http.NewServeMux()

	mux.HandleFunc("/echo", echoHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/", echoHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "4199"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	go func() {
		proxy.Logger.Info("service started", zap.String("service", "echo"), zap.String("port", port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			proxy.Logger.Fatal("listen failed", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	proxy.Logger.Info("shutting down", zap.String("service", "echo"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		proxy.Logger.Fatal("forced shutdown", zap.Error(err))
	}

	proxy.Logger.Info("service stopped", zap.String("service", "echo"))
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	response := map[string]string{
		"status":    "unhealthy",
		"timestamp": time.Now().Format(time.RFC3339),
	}

	json.NewEncoder(w).Encode(response)
}

func echoHandler(w http.ResponseWriter, r *http.Request) {
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

	_, span := otel.Tracer("echo").Start(ctx, "echo.handle_request",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("http.method", r.Method),
			attribute.String("http.url", r.URL.Path),
			attribute.String("http.host", r.Host),
		),
	)
	defer span.End()

	traceID := span.SpanContext().TraceID().String()

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sleep := r.URL.Query().Get("sleep")
	var sleepMs int
	if sleep != "" {
		var err error
		sleepMs, err = strconv.Atoi(sleep)
		if err != nil {
			http.Error(w, "Invalid sleep parameter", http.StatusBadRequest)
			return
		}
		proxy.Logger.Info("simulating latency",
			zap.String("trace_id", traceID),
			zap.Int("sleep_ms", sleepMs),
		)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	data := map[string]interface{}{
		"method":      r.Method,
		"path":        r.URL,
		"query":       r.URL.Query(),
		"headers":     r.Header,
		"remote_addr": r.RemoteAddr,
		"sleep":       sleep,
	}

	time.Sleep(time.Duration(sleepMs) * time.Millisecond)

	proxy.Logger.Info("request completed",
		zap.String("trace_id", traceID),
		zap.String("service", "echo"),
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
	)

	json.NewEncoder(w).Encode(data)
}
