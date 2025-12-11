// proxy/main.go
package main

import (
	px "ananse/pkg/proxy"
	"log"
	"net/http"
	"net/http/httputil"
	"time"
)

func main() {

	backends := []*px.Backend{
		{Name: "backend1", TargetUrl: px.MustParse("http://localhost:5004"), Healthy: true},
		{Name: "backend2", TargetUrl: px.MustParse("http://localhost:5001"), Healthy: true},
		{Name: "backend3", TargetUrl: px.MustParse("http://localhost:5003"), Healthy: true},
		{Name: "backend4", TargetUrl: px.MustParse("http://localhost:5002"), Healthy: true},
	}
	// create a reverse proxy
	bkPool := px.NewBackendPool(backends)
	loadbalancer := px.NewLoadBalancer("round-robin", bkPool)
	health := px.NewHealthCheck(bkPool, 10*time.Second)
	go health.Check()
	proxy := &httputil.ReverseProxy{
		Director:       px.Director(),
		Transport:      px.Transport(),
		ErrorHandler:   px.ErrorHandler(bkPool, health),
		ModifyResponse: px.ModifyResponse(),
	}

	handler := px.NewProxyHandler(loadbalancer, bkPool, health, proxy)

	log.Println("Proxy server started on :8089")
	log.Fatal(http.ListenAndServe(":8089", handler))
}
