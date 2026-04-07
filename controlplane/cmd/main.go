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
	useConsul := flag.Bool("consul", false, "Use Consul service discovery")
	consulAddr := flag.String("consul-addr", "localhost:8500", "Consul agent address")
	consulTag := flag.String("consul-tag", "", "Only discover services with this tag")
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
	switch {
	case *useK8s:
		px.Logger.Info("Starting with Kubernetes service discovery")
		k8sClient, err := cp.NewK8sClient(*ananseNamespace)
		if err != nil {
			px.Logger.Fatal("Failed to create K8s client", zap.Error(err))
		}
		watcher = k8sClient

	case *useConsul:
		px.Logger.Info("Starting with Consul service discovery",
			zap.String("consul_addr", *consulAddr))
		consulClient, err := cp.NewConsulClient(cp.ConsulConfig{
			Address:   *consulAddr,
			TagFilter: *consulTag,
		})
		if err != nil {
			px.Logger.Fatal("Failed to create Consul client", zap.Error(err))
		}
		watcher = consulClient

	default:
		px.Logger.Info("Starting with file-based configuration",
			zap.String("config_path", *configPath),
			zap.String("config_name", *configName))
		watcher = cp.NewFileClient(*configPath, *configName, *configType)
	}

	go injector.StartWebhookServer(":8443")

	// Create control plane server with empty config (sidecars can connect immediately)
	server, err := NewServer(nil)
	if err != nil {
		px.Logger.Fatal("Failed to create server", zap.Error(err))
	}

	// Start gRPC server immediately
	go func() {
		if err := server.Start(*grpcAddr); err != nil {
			px.Logger.Fatal("gRPC server failed", zap.Error(err))
		}
	}()

	px.Logger.Info("gRPC server started, waiting for service discovery...")

	// Start watching for configs
	configChan := watcher.Watch(ctx)

	// Watch for config updates and push to subscribers
	go func() {
		for {
			select {
			case cfg, ok := <-configChan:
				if !ok {
					px.Logger.Info("Config watcher stopped")
					return
				}
				if cfg != nil {
					px.Logger.Info("Configuration received",
						zap.String("version", cfg.Version),
						zap.Int("services", len(cfg.Services)))
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
