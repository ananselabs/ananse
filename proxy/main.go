package main

import (
	pb "ananse/controlplane/cmd/configpb"
	px "ananse/pkg/proxy"
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

func main() {
	// Initialize logger
	if px.Logger == nil {
		px.InitLogger()
	}
	defer px.Logger.Sync()

	cleanup := px.InitTracer()
	defer cleanup(context.Background())

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
	state := px.NewProxyState()

	// Config callback
	onConfig := func(cfg *pb.Config) {
		state.HandleConfig(cfg)
		firstConfigOnce.Do(func() {
			close(firstConfigReady)
		})
	}

	// Connect to control plane
	client, err := px.NewConfigClient(controlPlaneEndpoint, podName, onConfig)
	if err != nil {
		px.Logger.Fatal("failed to create config client", zap.Error(err))
	}

	// Start subscription
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.SubscribeWithRetry(ctx)

	// Wait for initial config
	px.Logger.Info("waiting for initial configuration", zap.String("endpoint", controlPlaneEndpoint))
	select {
	case <-firstConfigReady:
		px.Logger.Info("initial configuration received")
	case <-time.After(30 * time.Second):
		px.Logger.Fatal("timeout waiting for initial configuration")
	case sig := <-shutdownCh:
		px.Logger.Info("shutdown signal during startup", zap.String("signal", sig.String()))
		return
	}

	// Detect mode (default: gateway)
	mode := os.Getenv("ANANSE_MODE")
	if mode == "" {
		mode = "gateway"
	}

	px.Logger.Info("proxy starting", zap.String("mode", mode))

	// Sidecar mode: simple transparent proxy
	if mode == "sidecar" {
		px.StartSidecarProxy(state, shutdownCh, ctx, cancel)
		return
	}

	// Gateway mode: full reverse proxy with control plane
	px.StartGateway(state, podName, shutdownCh, ctx, cancel)
}
