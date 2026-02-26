package proxy

import (
	pb "ananse/controlplane/cmd/configpb"
	"fmt"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"golang.org/x/net/context"
)

func StartGateway() {
	// Get pod name from environment
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		podName = fmt.Sprintf("proxy-%s", uuid.New().String()[:8])
	}

	// Get control plane endpoint
	controlPlaneEndpoint := os.Getenv("CONTROL_PLANE_ENDPOINT")
	if controlPlaneEndpoint == "" {
		controlPlaneEndpoint = "localhost:50051"
	}

	// Signal channels
	firstConfigReady := make(chan struct{})
	var firstConfigOnce sync.Once
	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, os.Interrupt, syscall.SIGTERM)

	// Create proxy state
	state := NewProxyState()

	// Config callback
	onConfig := func(cfg *pb.Config) {
		state.HandleConfig(cfg)
		firstConfigOnce.Do(func() {
			close(firstConfigReady)
		})
	}

	// Connect to control plane
	client, err := NewConfigClient(controlPlaneEndpoint, podName, onConfig)
	if err != nil {
		Logger.Fatal("failed to create config client", zap.Error(err))
	}

	// Start subscription
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.SubscribeWithRetry(ctx)

	// Wait for initial config
	Logger.Info("waiting for initial configuration", zap.String("endpoint", controlPlaneEndpoint))
	select {
	case <-firstConfigReady:
		Logger.Info("initial configuration received")
	case <-time.After(30 * time.Second):
		Logger.Fatal("timeout waiting for initial configuration")
	case sig := <-shutdownCh:
		Logger.Info("shutdown signal during startup", zap.String("signal", sig.String()))
		return
	}

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
