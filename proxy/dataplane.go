// proxy/dataplane.go
package main

import (
	cf "ananse/config"
	pb "ananse/controlplane/cmd/configpb"
	px "ananse/pkg/proxy"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Internal endpoint representation
type Endpoint struct {
	Address string
	// later: health state, circuit breaker counters, etc.
}

// Internal route representation
type Route struct {
	Path    string
	Methods map[string]struct{} // fast membership check
}

// Per-service routing state
type ServiceRouting struct {
	Name      string
	Routes    []Route
	Endpoints []Endpoint
}

// Full routing table used by the proxy
type RoutingTable struct {
	Services map[string]*ServiceRouting // key: service name
}

type ConfigClient struct {
	client      pb.ControlPlaneClient
	proxyID     string
	lastVersion string
	onUpdate    func(config *pb.Config)
}

type ProxyState struct {
	mu                 sync.RWMutex
	lastAppliedConfig  *pb.Config
	lastAppliedVersion string
	routingTable       *RoutingTable
	backendPool        *px.BackendPool
	loadBalancer       *px.LoadBalancer
	health             *px.Health
}

func NewConfigClient(controlPlaneAddr string, proxyID string, onUpdate func(config *pb.Config)) (*ConfigClient, error) {
	conn, err := grpc.NewClient(controlPlaneAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}

	return &ConfigClient{
		client:   pb.NewControlPlaneClient(conn),
		proxyID:  proxyID,
		onUpdate: onUpdate,
	}, nil
}

// Subscribe starts receiving config updates
func (c *ConfigClient) Subscribe(ctx context.Context) error {
	req := &pb.SubscribeRequest{
		ProxyId:         c.proxyID,
		LastSeenVersion: c.lastVersion,
	}

	//call the subcribe RPC - returns a stream
	stream, err := c.client.Subscribe(ctx, req)
	if err != nil {
		return err
	}

	cf.Logger.Info("Subscribed to control plane as ", zap.String("proxy_id", c.proxyID))

	// Receive updates continuously
	for {
		config, err := stream.Recv()
		if err == io.EOF {
			cf.Logger.Info("Stream closed by server")
			return nil
		}
		if err != nil {
			log.Printf("Error receiving config: %v", err)
			return err
		}
		log.Printf("Received config update: version=%s, services=%d",
			config.Version, len(config.Services))

		c.lastVersion = config.Version

		//call the callbacl to handle the new config
		if c.onUpdate != nil {
			c.onUpdate(config)
		}
	}
}

// SubscribeWithRetry handles reconnection if connection drops
func (c *ConfigClient) SubscribeWithRetry(ctx context.Context) {
	for {
		err := c.Subscribe(ctx)
		if err != nil {
			log.Printf("Subscription error: %v. Retrying in 5s...", err)
			time.Sleep(5 * time.Second)
			continue
		}

		// If Subscribe returns normally (stream closed), wait and retry
		log.Println("Connection closed. Retrying in 5s...")
		time.Sleep(5 * time.Second)
	}
}

func (p *ProxyState) HandleConfig(cfg *pb.Config) {
	p.mu.Lock()
	defer p.mu.Unlock()

	rt, err := buildRoutingTable(cfg)
	if err != nil {
		cf.Logger.Error("Config apply failed",
			zap.String("version", cfg.Version),
			zap.Error(err),
		)
		// TODO: NACK could be sent here in the future
		return // keep lastAppliedConfig as is
	}

	// swap in the last known good
	p.lastAppliedConfig = cfg
	p.lastAppliedVersion = cfg.Version
	p.routingTable = rt
	cf.Logger.Info("Config applied",
		zap.String("version", cfg.Version),
	)

	cf.Logger.Info("last applied config is ",
		zap.String("version", p.lastAppliedVersion),
	)

	backends, err := p.CreateBackends(rt)
	p.backendPool.UpdateBackend(backends)

}

func buildRoutingTable(cfg *pb.Config) (*RoutingTable, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be found")
	}
	rt := &RoutingTable{
		Services: make(map[string]*ServiceRouting),
	}

	for i, svc := range cfg.Services {
		name := svc.GetName()
		if name == "" {
			return nil, fmt.Errorf("services[%d].name is required", i)
		}

		// Endpoints
		if len(svc.GetEndpoints()) == 0 {
			return nil, fmt.Errorf("services[%d] must have at least one endpoint", i)
		}

		endpointSeen := make(map[string]struct{})
		endpoints := make([]Endpoint, 0, len(svc.Endpoints))
		for j, ep := range svc.Endpoints {
			addr := ep.GetAddress()
			if addr == "" {
				return nil, fmt.Errorf("services[%d].endpoints[%d].address is required", i, j)

			}
			if _, ok := endpointSeen[addr]; ok {
				return nil, fmt.Errorf("duplicate endpoint %q in services[%d]", addr, i)
			}
			endpointSeen[addr] = struct{}{}
			endpoints = append(endpoints, Endpoint{Address: addr})
		}

		// Routes
		if len(svc.GetRoutes()) == 0 {
			return nil, fmt.Errorf("services[%d] must have at least one route", i)
		}
		routes := make([]Route, 0, len(svc.Routes))
		for j, r := range svc.Routes {
			path := r.GetPath()
			if path == "" {
				return nil, fmt.Errorf("services[%d].routes[%d].path is required", i, j)
			}
			methods := make(map[string]struct{})
			for _, m := range r.GetMethods() {
				if m == "" {
					continue
				}
				methods[strings.ToUpper(m)] = struct{}{}
			}
			if len(methods) == 0 {
				return nil, fmt.Errorf("services[%d].routes[%d] must have at least one method", i, j)
			}
			routes = append(routes, Route{
				Path:    path,
				Methods: methods,
			})
		}

		if _, exists := rt.Services[name]; exists {
			return nil, fmt.Errorf("duplicate service name %q", name)
		}

		rt.Services[name] = &ServiceRouting{
			Name:      name,
			Routes:    routes,
			Endpoints: endpoints,
		}

	}
	return rt, nil
}

func (p *ProxyState) CreateBackends(rt *RoutingTable) ([]*px.Backend, error) {
	if rt == nil {
		return nil, fmt.Errorf("routing table is empty")
	}
	var backends []*px.Backend
	for _, service := range rt.Services {

		// Create backend for EACH endpoint
		for i, endpoint := range service.Endpoints {
			bk := &px.Backend{
				Name:      fmt.Sprintf("%s-%d", service.Name, i),
				TargetUrl: px.MustParse(fmt.Sprintf("http://%s", endpoint.Address)),
				Healthy:   true,
			}

			backends = append(backends, bk)
		}
	}
	return backends, nil
}

func (p *ProxyState) CreateBackendPool(bks []*px.Backend) {
	bkPool := px.NewBackendPool(bks)
	loadbalancer := px.NewLoadBalancer("round-robin", bkPool)
	health := px.NewHealthCheck(bkPool, time.Duration(p.lastAppliedConfig.ProxyConfig.HealthCheckIntervalSeconds))

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
}
