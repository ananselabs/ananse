package main

import (
	"ananse/config"
	pb "ananse/controlplane/cmd/configpb"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

func main() {
	// Initialize logger
	if config.Logger == nil {
		config.InitLogger()
	}
	defer config.Logger.Sync()

	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		config.Logger.Fatal("failed to listen", zap.Error(err))
	}

	grpcServer := grpc.NewServer()
	controlPlaneServer, err := NewServer()
	if err != nil {
		config.Logger.Fatal("failed to init control plane", zap.Error(err))
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		config.Logger.Fatal("failed to create watcher", zap.Error(err))
	}
	defer watcher.Close()

	// Watch config file
	if err := watcher.Add("config/config.yml"); err != nil {
		config.Logger.Fatal("failed to watch config file", zap.Error(err))
	}

	// Start config change notifier
	controlPlaneServer.ConfigNotifier(watcher)

	// Register your service implementation
	pb.RegisterControlPlaneServer(grpcServer, controlPlaneServer)

	config.Logger.Info("Control plane listening", zap.String("address", ":50051"))

	// Graceful shutdown
	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			config.Logger.Fatal("failed to serve", zap.Error(err))
		}
	}()

	// Wait for interrupt signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	config.Logger.Info("Shutting down gracefully...")
	grpcServer.GracefulStop()
	config.Logger.Info("Server stopped")
}
