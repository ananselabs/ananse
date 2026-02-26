// cmd/controlplane/main.go
package main

import (
	cp "ananse/controlplane"
	"ananse/controlplane/injector"
	px "ananse/pkg/proxy"
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"
)

func main() {
	// Initialize logger first
	if px.Logger == nil {
		px.InitLogger()
	}
	defer px.Logger.Sync()

	// Command-line flags
	useK8s := flag.Bool("k8s", false, "Use Kubernetes service discovery")
	configPath := flag.String("config-path", "./config", "Path to config directory")
	configName := flag.String("config-name", "config", "Config file name (without extension)")
	configType := flag.String("config-type", "yaml", "Config file type (yaml, json)")
	grpcAddr := flag.String("addr", ":50051", "gRPC server address")
	ananseNamespace := flag.String("ananse-namespace", "ananse", "Ananse namespace")
	flag.Parse()

	// Setup context with cancellation
	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Create appropriate config watcher
	var watcher cp.ConfigWatcher
	if *useK8s {
		px.Logger.Info("Starting with Kubernetes service discovery")
		k8sClient, err := cp.NewK8sClient(*ananseNamespace)
		if err != nil {
			px.Logger.Fatal("Failed to create K8s client", zap.Error(err))
		}
		watcher = k8sClient
	} else {
		px.Logger.Info("Starting with file-based configuration",
			zap.String("config_path", *configPath),
			zap.String("config_name", *configName))
		watcher = cp.NewFileClient(*configPath, *configName, *configType)
	}

	go injector.StartWebhookServer(":8443")

	// Start watching for configs
	configChan := watcher.Watch(ctx)

	// Wait for initial config
	px.Logger.Info("Waiting for initial configuration...")
	initialConfig, ok := <-configChan
	if !ok {
		px.Logger.Fatal("Config channel closed before receiving initial config")
	}
	if initialConfig == nil {
		px.Logger.Fatal("Received nil initial config")
	}

	px.Logger.Info("Initial configuration loaded",
		zap.String("version", initialConfig.Version),
		zap.Int("services", len(initialConfig.Services)))

	// Create control plane server
	server, err := NewServer(initialConfig)
	if err != nil {
		px.Logger.Fatal("Failed to create server", zap.Error(err))
	}

	// Start gRPC server in background
	go func() {
		if err := server.Start(*grpcAddr); err != nil {
			px.Logger.Fatal("gRPC server failed", zap.Error(err))
		}
	}()

	// Watch for config updates
	go func() {
		for {
			select {
			case cfg, ok := <-configChan:
				if !ok {
					px.Logger.Info("Config watcher stopped")
					return
				}
				if cfg != nil {
					server.UpdateConfig(cfg)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	px.Logger.Info("Control plane running. Press Ctrl+C to stop.")

	// Wait for shutdown signal
	<-ctx.Done()
	px.Logger.Info("Shutting down control plane...")

	// Graceful shutdown
	server.Shutdown()
	px.Logger.Info("Control plane stopped")
}
