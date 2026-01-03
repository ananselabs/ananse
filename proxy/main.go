// proxy/main.go
package main

import (
	config "ananse/config"
	px "ananse/pkg/proxy"
	"fmt"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
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

	config.Logger.Info("Proxy server is started",
		zap.Int("port", notifier.Server.Port),
	)
	config.Logger.Fatal("Server failed",
		zap.Error(http.ListenAndServe(fmt.Sprintf(":%d", notifier.Server.Port), mux)),
	)
}
