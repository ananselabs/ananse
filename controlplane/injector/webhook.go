package injector

import (
	px "ananse/pkg/proxy"
	"context"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"
)

// StartWebhookServer starts the mutating webhook server
func StartWebhookServer(port string) {
	if port == "" {
		port = ":8443"
	}

	// Start the injector (handles /mutate endpoint)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		px.Logger.Info("starting injector server", zap.String("port", port))
		if err := StartInjection(port); err != nil {
			px.Logger.Error("server failed", zap.Error(err))
			cancel() // kill the app if the server fails
		}
	}()

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		px.Logger.Info("webhook server shutting down", zap.String("signal", sig.String()))
	case <-ctx.Done():
		px.Logger.Info("context cancelled, exiting")
	}
}
