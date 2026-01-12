// proxy/main.go
package main

import (
	pb "ananse/controlplane/cmd/configpb"
	px "ananse/pkg/proxy"
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

func main() {
	// Initialize logger
	if px.Logger == nil {
		px.InitLogger()
	}
	defer px.Logger.Sync()

	// Create channel to signal when first config is received
	firstConfigReady := make(chan struct{})
	var firstConfigOnce sync.Once

	state := px.NewProxyState() // Proper initialization

	// Wrap handler to signal on first config
	onConfig := func(cfg *pb.Config) {
		state.HandleConfig(cfg)
		firstConfigOnce.Do(func() {
			close(firstConfigReady) // Signal: ready to start server
		})
	}

	// Connect to control plane
	client, err := px.NewConfigClient("localhost:50051", "test-proxy-1", onConfig)
	if err != nil {
		px.Logger.Fatal("Failed to create config client", zap.Error(err))
	}

	// Start subscription in background
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.SubscribeWithRetry(ctx)

	// BLOCK until first config received
	px.Logger.Info("Waiting for initial configuration...")
	select {
	case <-firstConfigReady:
		px.Logger.Info("Initial configuration received")
	case <-time.After(30 * time.Second):
		px.Logger.Fatal("Timeout waiting for initial configuration")
	}

	// NOW safe to access state
	proxy := &httputil.ReverseProxy{
		Director:       px.Director(),
		Transport:      px.Transport(),
		ErrorHandler:   px.ErrorHandler(state.BackendPool, state.Health),
		ModifyResponse: px.ModifyResponse(),
	}

	handler := px.NewProxyHandler(
		state.Router,
		state.LoadBalancer,
		state.BackendPool,
		state.Health,
		proxy,
	)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
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

	port := state.GetPort()

	px.Logger.Info("Proxy server starting",
		zap.Int("port", port),
		zap.String("config_version", state.LastAppliedVersion),
	)

	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), mux); err != nil {
		px.Logger.Fatal("Server failed", zap.Error(err))
	}
}
