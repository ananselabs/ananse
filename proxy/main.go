// proxy/main.go
package main

import (
	config "ananse/config"
	px "ananse/pkg/proxy"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	config.InitLogger()
	defer config.Logger.Sync()
	watcher := config.InitWatcher()
	defer watcher.Close()
	notifier, err := config.LoadConfig()
	if err != nil {
		config.Logger.Fatal("Failed to load initial config", zap.Error(err))
	}
	backends := config.CreateBackends(notifier)
	// create a reverse proxy
	bkPool := px.NewBackendPool(backends)
	loadbalancer := px.NewLoadBalancer("round-robin", bkPool)
	health := px.NewHealthCheck(bkPool, 3*time.Second)

	go health.Check()
	proxy := &httputil.ReverseProxy{
		Director:       px.Director(),
		Transport:      px.Transport(),
		ErrorHandler:   px.ErrorHandler(bkPool, health),
		ModifyResponse: px.ModifyResponse(),
	}

	handler := px.NewProxyHandler(loadbalancer, bkPool, health, proxy)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/", handler)

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			for _, backend := range bkPool.GetAllBackends() {
				px.UpdateBackendConnections(backend.Name, backend.GetActiveRequests())
			}
		}
	}()

	config.ConfigNotifier(bkPool, health, watcher)

	conn, err := grpc.NewClient("localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	state := &ProxyState{}
	con, err := NewConfigClient("localhost:50051", "test-proxy-1", state.HandleConfig)
	if err != nil {
		config.Logger.Error("this is the error that occured", zap.Error(err))
	}
	// Run subscription in background
	go con.SubscribeWithRetry(context.Background())

	config.Logger.Info("this is the lastapplied config", zap.Any("last applied config", state.lastAppliedConfig))
	config.Logger.Info("Proxy server is started",
		zap.Int("port", notifier.Server.Port),
	)
	config.Logger.Fatal("Server failed",
		zap.Error(http.ListenAndServe(fmt.Sprintf(":%d", notifier.Server.Port), mux)),
	)
}
