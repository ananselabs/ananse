package main

import (
	pb "ananse/controlplane/cmd/configpb"
	"ananse/pkg/proxy"
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
	if proxy.Logger == nil {
		proxy.InitLogger()
	}
	defer proxy.Logger.Sync()

	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		proxy.Logger.Fatal("failed to listen", zap.Error(err))
	}

	grpcServer := grpc.NewServer()
	controlPlaneServer, err := NewServer()
	if err != nil {
		proxy.Logger.Fatal("failed to init control plane", zap.Error(err))
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		proxy.Logger.Fatal("failed to create watcher", zap.Error(err))
	}
	defer watcher.Close()

	// Watch config file
	if err := watcher.Add("config/config.yml"); err != nil {
		proxy.Logger.Fatal("failed to watch config file", zap.Error(err))
	}

	// Start config change notifier
	controlPlaneServer.ConfigNotifier(watcher)

	// Register your service implementation
	pb.RegisterControlPlaneServer(grpcServer, controlPlaneServer)

	proxy.Logger.Info("Control plane listening", zap.String("address", ":50051"))

	// Graceful shutdown
	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			proxy.Logger.Fatal("failed to serve", zap.Error(err))
		}
	}()

	// Wait for interrupt signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	proxy.Logger.Info("Shutting down gracefully...")
	grpcServer.GracefulStop()
	proxy.Logger.Info("Server stopped")
}
