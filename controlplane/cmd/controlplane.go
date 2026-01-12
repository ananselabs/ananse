// cmd/controlplane.go
package main

import (
	pb "ananse/controlplane/cmd/configpb"
	px "ananse/pkg/proxy"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	reloadTimer *time.Timer
)

type Server struct {
	pb.ControlPlaneServer
	mu            sync.Mutex
	currentConfig *pb.Config
	subscribers   map[string]chan *pb.Config
	version       uint64
}

func NewServer() (*Server, error) {
	loadConfig, err := LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load initial config: %w", err)
	}

	s := &Server{
		currentConfig: &pb.Config{
			Services:    loadConfig.Services,
			ProxyConfig: loadConfig.ProxyConfig,
			LastUpdated: timestamppb.Now(),
			Version:     "v0",
		},
		subscribers: make(map[string]chan *pb.Config),
	}
	return s, nil
}

func (s *Server) Subscribe(req *pb.SubscribeRequest, stream pb.ControlPlane_SubscribeServer) error {
	//currently LastSeenVersion is ignored, used only for debugging
	px.Logger.Info("New subscriber",
		zap.String("proxy_id", req.ProxyId),
		zap.String("last_seen_version", req.LastSeenVersion))

	//Create a channel for this subscriber
	configChan := make(chan *pb.Config, 1)

	// Register the subscriber
	s.mu.Lock()
	s.subscribers[req.ProxyId] = configChan
	current := s.currentConfig
	s.mu.Unlock()

	//Cleanup on disconnect
	defer func() {
		s.mu.Lock()
		delete(s.subscribers, req.ProxyId)
		close(configChan)
		s.mu.Unlock()

		px.Logger.Info("Subscriber disconnected", zap.String("proxy_id", req.ProxyId))
	}()

	// always send latest snapshot once
	if err := stream.Send(current); err != nil {
		return err
	}

	//keep connection open and send updates when they arrive
	for {
		select {
		case cfg := <-configChan:
			if cfg == nil {
				return nil
			}
			if err := stream.Send(cfg); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return stream.Context().Err()

		}
	}
}

// UpdateConfig pushes new config to all subscribers
func (s *Server) UpdateConfig(cfg *pb.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.version++
	cfg.Version = fmt.Sprintf("v%d", s.version)
	cfg.LastUpdated = timestamppb.Now()
	s.currentConfig = cfg

	for proxyID, ch := range s.subscribers {
		select {
		case ch <- cfg:
			px.Logger.Info("Sent config update",
				zap.String("proxy_id", proxyID),
				zap.String("version", cfg.Version),
			)
		default:
			// latest-only: drop old value and push new snapshot
			select {
			case <-ch:
			default:
			}
			ch <- cfg
			px.Logger.Warn("Subscriber was slow; overwrote pending config",
				zap.String("proxy_id", proxyID),
				zap.String("version", cfg.Version),
			)
		}
	}
}

func LoadConfig() (*pb.Config, error) {
	if px.Logger == nil {
		px.InitLogger()
	}

	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("./config/")

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	// Environment variable support
	viper.SetEnvPrefix("ANANSE")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()
	viper.BindEnv("proxy.port")
	viper.BindEnv("proxy.metrics_port")
	viper.BindEnv("proxy.health_check_interval")

	// Parse into protobuf struct
	config := &pb.Config{
		ProxyConfig: &pb.ProxyConfig{
			Port:                       int32(viper.GetInt("proxy.port")),
			MetricsPort:                int32(viper.GetInt("proxy.metrics_port")),
			HealthCheckIntervalSeconds: int32(viper.GetInt("proxy.health_check_interval")),
		},
		Services: parseServices(viper.Get("services")),
	}

	// Validate
	if err := validateConfig(config); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return config, nil
}

func parseServices(raw interface{}) []*pb.Service {
	services := []*pb.Service{}

	servicesSlice, ok := raw.([]interface{})
	if !ok {
		return services
	}

	for _, item := range servicesSlice {
		svcMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		service := &pb.Service{
			Name:      getString(svcMap, "name"),
			Endpoints: parseEndpoints(svcMap["endpoints"]),
			Routes:    parseRoutes(svcMap["routes"]),
		}
		services = append(services, service)
	}

	return services
}

func parseEndpoints(raw interface{}) []*pb.Endpoint {
	endpoints := []*pb.Endpoint{}

	epSlice, ok := raw.([]interface{})
	if !ok {
		return endpoints
	}

	for _, item := range epSlice {
		epMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		endpoints = append(endpoints, &pb.Endpoint{
			Address: getString(epMap, "address"),
		})
	}

	return endpoints
}

func parseRoutes(raw interface{}) []*pb.Route {
	routes := []*pb.Route{}

	routeSlice, ok := raw.([]interface{})
	if !ok {
		return routes
	}

	for _, item := range routeSlice {
		routeMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		route := &pb.Route{
			Path:    getString(routeMap, "path"),
			Methods: getStringSlice(routeMap, "methods"),
		}
		routes = append(routes, route)
	}

	return routes
}

func getString(m map[string]interface{}, key string) string {
	if val, ok := m[key]; ok {
		if str, ok := val.(string); ok {
			return str
		}
	}
	return ""
}

func getStringSlice(m map[string]interface{}, key string) []string {
	result := []string{}
	if val, ok := m[key]; ok {
		if slice, ok := val.([]interface{}); ok {
			for _, item := range slice {
				if str, ok := item.(string); ok {
					result = append(result, str)
				}
			}
		}
	}
	return result
}

func validateConfig(config *pb.Config) error {
	if config.ProxyConfig == nil {
		return errors.New("proxy_config is required")
	}
	if config.ProxyConfig.Port == 0 {
		return errors.New("proxy_config.port is required")
	}
	if len(config.Services) == 0 {
		return errors.New("at least one service is required")
	}

	for i, svc := range config.Services {
		if svc.Name == "" {
			return fmt.Errorf("services[%d].name is required", i)
		}
		if len(svc.Endpoints) == 0 {
			return fmt.Errorf("services[%d] must have at least one endpoint", i)
		}
		seen := map[string]struct{}{}
		for j, ep := range svc.Endpoints {
			if ep.Address == "" {
				return fmt.Errorf("services[%d].endpoints[%d].address is required", i, j)
			}
			if _, ok := seen[ep.Address]; ok {
				return fmt.Errorf("duplicate endpoint %q in services[%d]", ep.Address, i)
			}
			seen[ep.Address] = struct{}{}
		}
		if len(svc.Routes) == 0 {
			return fmt.Errorf("services[%d] must have at least one route", i)
		}
		for j, r := range svc.Routes {
			if r.Path == "" {
				return fmt.Errorf("services[%d].routes[%d].path is required", i, j)
			}
		}
	}

	return nil
}

func (s *Server) ConfigNotifier(watcher *fsnotify.Watcher) {
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Write) {
					s.mu.Lock()
					if reloadTimer != nil {
						reloadTimer.Stop()
					}
					reloadTimer = time.AfterFunc(500*time.Millisecond, func() {
						px.Logger.Info("Config changed, reloading",
							zap.String("file", event.Name),
							zap.String("current_version", s.currentConfig.Version))
						cfg, err := LoadConfig()
						if err != nil {
							px.Logger.Error("Reload failed", zap.Error(err))
							return
						}
						s.UpdateConfig(cfg)
					})
					s.mu.Unlock()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				px.Logger.Error("Watcher error", zap.Error(err))
			}
		}
	}()
}
