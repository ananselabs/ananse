// proxy/dataplane.go
package proxy

import (
	pb "ananse/controlplane/cmd/configpb"
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

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
	Services  map[string]*ServiceRouting
	pathIndex map[string]map[string]string
}

type ConfigClient struct {
	client      pb.ControlPlaneClient
	proxyID     string
	lastVersion string
	onUpdate    func(config *pb.Config)
}

type ProxyState struct {
	mu                 sync.RWMutex
	LastAppliedConfig  *pb.Config
	LastAppliedVersion string
	routingTable       *RoutingTable
	BackendPool        *BackendPool
	LoadBalancer       *LoadBalancer
	Health             *Health
	Router             *Router
}

func NewProxyState() *ProxyState {
	return &ProxyState{
		Router: &Router{
			routingTable: &RoutingTable{
				Services:  make(map[string]*ServiceRouting),
				pathIndex: make(map[string]map[string]string),
			},
		},
	}
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

	Logger.Info("Subscribed to control plane as ", zap.String("proxy_id", c.proxyID))

	// Receive updates continuously
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		config, err := stream.Recv()
		if err == io.EOF {
			Logger.Info("Stream closed by server")
			return nil
		}
		if err != nil {
			Logger.Error("Error receiving config: %v", zap.Error(err))
			return err
		}
		Logger.Info("Received config update",
			zap.String("version", config.Version),
			zap.Int("services", len(config.Services)),
		)

		//call the callbacl to handle the new config
		if c.onUpdate != nil {
			c.onUpdate(config)
		}
		c.lastVersion = config.Version
	}
}

// SubscribeWithRetry handles reconnection if connection drops
func (c *ConfigClient) SubscribeWithRetry(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			Logger.Info("Subscription cancelled")
			return
		default:
		}

		err := c.Subscribe(ctx)
		if err != nil {
			Logger.Warn("Subscription error, retrying", zap.Error(err))

			// Sleep with context awareness
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		Logger.Info("Stream closed, retrying")
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
			continue
		}
	}
}

func (p *ProxyState) HandleConfig(cfg *pb.Config) {
	rt, err := buildRoutingTable(cfg)
	if err != nil {
		Logger.Error("Config validation failed", zap.Error(err))
		return
	}

	backends, err := p.CreateBackends(rt)
	if err != nil {
		Logger.Error("Backend creation failed", zap.Error(err))
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Initialize Router if needed
	if p.Router == nil {
		p.Router = &Router{}
	}
	p.Router.UpdateRoutes(rt)

	// Initialize or update BackendPool
	if p.BackendPool == nil {
		p.BackendPool = NewBackendPool(backends)
	} else {
		p.BackendPool.UpdateBackends(backends)
	}

	// Initialize or update LoadBalancer
	if p.LoadBalancer == nil {
		p.LoadBalancer = NewLoadBalancer("round-robin", p.BackendPool)
	}
	// Note: LoadBalancer doesn't need updating - it queries BackendPool

	// Initialize or restart Health
	newInterval := time.Duration(cfg.ProxyConfig.HealthCheckIntervalSeconds) * time.Second
	if p.Health == nil {
		p.Health = NewHealthCheck(p.BackendPool, newInterval)
		go p.Health.Check()
	} else if newInterval != p.Health.GetHealthCheckInterval() {
		p.Health.Restart(newInterval)
	}

	// Update state
	p.LastAppliedConfig = cfg
	p.LastAppliedVersion = cfg.Version
	p.routingTable = rt

	Logger.Info("Config applied",
		zap.String("version", cfg.Version),
		zap.Int("services", len(rt.Services)),
		zap.Int("backends", countBackends(backends)),
	)
}

func countBackends(services map[string][]*Backend) int {
	count := 0
	for _, backends := range services {
		count += len(backends)
	}
	return count
}

func buildRoutingTable(cfg *pb.Config) (*RoutingTable, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be found")
	}
	rt := &RoutingTable{
		Services:  make(map[string]*ServiceRouting),
		pathIndex: make(map[string]map[string]string),
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
	rt.pathIndex = buildPathIndex(rt.Services)
	return rt, nil
}

func buildPathIndex(services map[string]*ServiceRouting) map[string]map[string]string {
	index := make(map[string]map[string]string)
	for serviceName, svc := range services {
		for _, route := range svc.Routes {
			if index[route.Path] == nil {
				index[route.Path] = make(map[string]string)
			}
			for method := range route.Methods {
				index[route.Path][method] = serviceName
			}
		}
	}
	return index
}

func (p *ProxyState) CreateBackends(rt *RoutingTable) (map[string][]*Backend, error) {
	if rt == nil {
		return nil, fmt.Errorf("routing table is nil")
	}

	services := make(map[string][]*Backend)

	for serviceName, svc := range rt.Services {
		if len(svc.Endpoints) == 0 {
			Logger.Warn("Service has no endpoints",
				zap.String("service", serviceName))
			continue
		}

		backends := make([]*Backend, 0, len(svc.Endpoints))

		for i, endpoint := range svc.Endpoints {
			if endpoint.Address == "" {
				Logger.Warn("Empty endpoint address",
					zap.String("service", serviceName),
					zap.Int("index", i))
				continue
			}
			addr := endpoint.Address
			if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
				addr = "http://" + addr
			}
			u, err := url.Parse(addr)
			if err != nil {
				return nil, fmt.Errorf("invalid endpoint %s for service %s: %w",
					endpoint.Address, serviceName, err)
			}

			bk := &Backend{
				Name:      fmt.Sprintf("%s-%d", serviceName, i),
				TargetUrl: u,
				Healthy:   true,
			}
			backends = append(backends, bk)
		}

		if len(backends) > 0 {
			services[serviceName] = backends
		}
	}

	if len(services) == 0 {
		return nil, fmt.Errorf("no valid services found in routing table")
	}

	return services, nil
}

func (p *ProxyState) GetPort() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.LastAppliedConfig == nil {
		return 8089 // Default
	}
	return int(p.LastAppliedConfig.ProxyConfig.Port)
}

func (p *ProxyState) IsReady() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.LastAppliedConfig != nil &&
		p.BackendPool != nil &&
		p.Health != nil &&
		p.Router != nil
}

func (p *ProxyState) UpdateMetrics() {
	p.mu.RLock()
	pool := p.BackendPool
	defer p.mu.RUnlock()

	if pool == nil {
		return
	}

	for _, backend := range pool.GetAllBackends() {
		UpdateBackendConnections(backend.Name, backend.GetActiveRequests())
	}
}

func (p *ProxyState) GetVersion() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.LastAppliedVersion
}
