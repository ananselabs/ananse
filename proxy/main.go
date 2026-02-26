package main

import (
	px "ananse/pkg/proxy"
	"context"
	"os"

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

	// Detect mode (default: gateway)
	mode := os.Getenv("ANANSE_MODE")
	if mode == "" {
		mode = "gateway"
	}

	px.Logger.Info("proxy starting", zap.String("mode", mode))

	// Sidecar mode: simple transparent proxy
	if mode == "sidecar" {
		px.StartSidecarProxy()
		return
	}

	// Gateway mode: full reverse proxy with control plane
	px.StartGateway()
}
