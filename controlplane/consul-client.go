package controlplane

import (
	pb "ananse/controlplane/cmd/configpb"
	px "ananse/pkg/proxy"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/consul/api"
	"go.uber.org/zap"
)

const (
	consulDebounceWindow = 500 * time.Millisecond
	consulBlockTimeout   = 5 * time.Minute
)

type ConsulClient struct {
	client *api.Client
	config ConsulConfig

	configChan chan *pb.Config
	updateChan chan struct{}

	mu            sync.RWMutex
	lastIndex     uint64
	knownServices map[string]uint64 // service name -> last modify index
}

type ConsulConfig struct {
	Address    string
	Datacenter string
	Token      string // ACL token if needed
	TagFilter  string // only discover services with this tag (e.g., "ananse")
}

func NewConsulClient(cfg ConsulConfig) (*ConsulClient, error) {
	if px.Logger == nil {
		px.InitLogger()
	}

	consulCfg := api.DefaultConfig()
	consulCfg.Address = cfg.Address
	if cfg.Datacenter != "" {
		consulCfg.Datacenter = cfg.Datacenter
	}
	if cfg.Token != "" {
		consulCfg.Token = cfg.Token
	}

	client, err := api.NewClient(consulCfg)
	if err != nil {
		return nil, fmt.Errorf("create consul client: %w", err)
	}

	// Verify connection
	_, err = client.Agent().Self()
	if err != nil {
		return nil, fmt.Errorf("connect to consul: %w", err)
	}

	px.Logger.Info("Connected to Consul",
		zap.String("address", cfg.Address))

	return &ConsulClient{
		client:        client,
		config:        cfg,
		configChan:    make(chan *pb.Config, 5),
		updateChan:    make(chan struct{}, 1),
		knownServices: make(map[string]uint64),
	}, nil
}

func (c *ConsulClient) Watch(ctx context.Context) <-chan *pb.Config {
	// Start the catalog watcher
	go c.watchCatalog(ctx)

	// Start the debouncer
	go c.runDebouncer(ctx)

	// Emit initial config
	if cfg := c.buildConfig(); c.validate(cfg) == nil {
		c.configChan <- cfg
	}

	return c.configChan
}

func (c *ConsulClient) watchCatalog(ctx context.Context) {
	var waitIndex uint64

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Blocking query for service list changes
		services, meta, err := c.client.Catalog().Services(&api.QueryOptions{
			WaitIndex: waitIndex,
			WaitTime:  consulBlockTimeout,
		})
		if err != nil {
			px.Logger.Warn("Consul catalog query failed", zap.Error(err))
			time.Sleep(5 * time.Second)
			continue
		}

		// Check if anything changed
		if meta.LastIndex > waitIndex {
			waitIndex = meta.LastIndex
			c.signalUpdate()
			px.Logger.Debug("Consul catalog changed",
				zap.Uint64("index", waitIndex),
				zap.Int("services", len(services)))
		}
	}
}

func (c *ConsulClient) signalUpdate() {
	select {
	case c.updateChan <- struct{}{}:
	default:
	}
}

func (c *ConsulClient) runDebouncer(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()

	for {
		select {
		case <-c.updateChan:
			timer.Reset(consulDebounceWindow)

		case <-timer.C:
			cfg := c.buildConfig()
			if err := c.validate(cfg); err != nil {
				px.Logger.Warn("Invalid config from Consul", zap.Error(err))
				continue
			}
			select {
			case c.configChan <- cfg:
				px.Logger.Info("Config updated from Consul",
					zap.Int("services", len(cfg.Services)))
			default:
				px.Logger.Warn("Config channel full")
			}

		case <-ctx.Done():
			close(c.configChan)
			return
		}
	}
}

func (c *ConsulClient) buildConfig() *pb.Config {
	cfg := &pb.Config{
		Version: fmt.Sprintf("consul-%d", time.Now().Unix()),
		ProxyConfig: &pb.ProxyConfig{
			Port:                       8089,
			MetricsPort:                9090,
			HealthCheckIntervalSeconds: 10,
		},
	}

	// Get all services from catalog
	services, _, err := c.client.Catalog().Services(nil)
	if err != nil {
		px.Logger.Error("Failed to list services", zap.Error(err))
		return cfg
	}

	for serviceName, tags := range services {
		// Skip consul's internal service
		if serviceName == "consul" {
			continue
		}

		// Filter by tag if configured
		if c.config.TagFilter != "" && !containsTag(tags, c.config.TagFilter) {
			continue
		}

		// Get healthy service instances
		entries, _, err := c.client.Health().Service(serviceName, "", true, nil)
		if err != nil {
			px.Logger.Warn("Failed to get service health",
				zap.String("service", serviceName),
				zap.Error(err))
			continue
		}

		if len(entries) == 0 {
			continue
		}

		endpoints := make([]*pb.Endpoint, 0, len(entries))
		var servicePort int32

		for _, entry := range entries {
			// Use service port, fallback to node port
			port := entry.Service.Port
			if port == 0 {
				continue
			}
			servicePort = int32(port)

			// Use service address, fallback to node address
			addr := entry.Service.Address
			if addr == "" {
				addr = entry.Node.Address
			}

			endpoints = append(endpoints, &pb.Endpoint{
				Address: fmt.Sprintf("%s:%d", addr, port),
			})
		}

		if len(endpoints) == 0 {
			continue
		}

		cfg.Services = append(cfg.Services, &pb.Service{
			Name:      serviceName,
			Port:      servicePort,
			Endpoints: endpoints,
			Routes: []*pb.Route{
				{
					Path:    "/" + serviceName,
					Methods: []string{"GET", "POST", "PUT", "DELETE", "PATCH"},
				},
			},
		})
	}

	return cfg
}

func (c *ConsulClient) validate(cfg *pb.Config) error {
	if cfg.ProxyConfig == nil {
		return fmt.Errorf("proxy_config is required")
	}
	if len(cfg.Services) == 0 {
		return fmt.Errorf("no services discovered from Consul")
	}
	return nil
}

func containsTag(tags []string, target string) bool {
	for _, t := range tags {
		if t == target {
			return true
		}
	}
	return false
}
