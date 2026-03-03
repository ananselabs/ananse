package proxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"golang.org/x/net/context"
)

func StartGateway(state *ProxyState, podName string, shutdownCh chan os.Signal, ctx context.Context, cancel context.CancelFunc) {
	// Get pod name from environment

	// Create reverse proxy
	proxy := &httputil.ReverseProxy{
		Director:       Director(),
		Transport:      Transport(),
		ErrorHandler:   ErrorHandler(state.BackendPool, state.Health),
		ModifyResponse: ModifyResponse(),
	}

	handler := NewProxyHandler(
		state.Router,
		state.LoadBalancer,
		state.BackendPool,
		state.Health,
		proxy,
	)

	// HTTP mux
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/health", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	mux.Handle("/", handler)

	// Metrics updater
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				state.UpdateMetrics()
			}
		}
	}()

	// Create server
	port := state.GetPort()
	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	Logger.Info("gateway proxy starting",
		zap.Int("port", port),
		zap.String("pod_name", podName),
		zap.String("config_version", state.GetVersion()))

	// Graceful shutdown handler
	go func() {
		sig := <-shutdownCh
		Logger.Info("shutdown signal received", zap.String("signal", sig.String()))

		ctx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()

		if err := server.Shutdown(ctx); err != nil {
			Logger.Error("shutdown error", zap.Error(err))
		}

		cancel() // Signal background goroutines
	}()

	// Start server
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		Logger.Fatal("server error", zap.Error(err))
	}

	Logger.Info("gateway proxy stopped")
}
