package controlplane

import (
	pb "ananse/controlplane/cmd/configpb"
	px "ananse/pkg/proxy"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

type FileClient struct {
	configPath string
	configName string
	configType string
	mu         sync.RWMutex
}

var (
	reloadTimer *time.Timer
)

func NewFileClient(configPath string, configName string, configType string) *FileClient {
	return &FileClient{configPath: configPath, configName: configName, configType: configType}
}
func (f *FileClient) LoadConfig() (*pb.Config, error) {
	if px.Logger == nil {
		px.InitLogger()
	}

	filepath := f.configPath

	viper.SetConfigName(f.configName)
	viper.SetConfigType(f.configType)
	//viper.AddConfigPath("./config/")
	viper.AddConfigPath(filepath)

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
	if err := f.validate(config); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return config, nil
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

func (f *FileClient) validate(config *pb.Config) error {
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

func (f *FileClient) Watch(ctx context.Context) <-chan *pb.Config {
	configChan := make(chan *pb.Config, 1)

	go func() {
		defer close(configChan)

		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			px.Logger.Error("failed to create watcher", zap.Error(err))
			return
		}
		defer watcher.Close()

		// Add file to watch
		configFile := filepath.Join(f.configPath, f.configName+"."+f.configType)
		if err := watcher.Add(configFile); err != nil {
			px.Logger.Error("failed to watch config file",
				zap.String("file", configFile),
				zap.Error(err))
			return
		}

		// Send initial config
		cfg, err := f.LoadConfig()
		if err != nil {
			px.Logger.Error("failed to load initial config", zap.Error(err))
			return
		}

		select {
		case configChan <- cfg:
			px.Logger.Info("Initial config loaded", zap.String("version", cfg.Version))
		case <-ctx.Done():
			return
		}

		// Watch for changes
		var reloadTimer *time.Timer
		for {
			select {
			case <-ctx.Done():
				px.Logger.Info("File watcher stopped")
				if reloadTimer != nil {
					reloadTimer.Stop()
				}
				return

			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				if event.Has(fsnotify.Write) {
					f.mu.Lock()
					if reloadTimer != nil {
						reloadTimer.Stop()
					}

					reloadTimer = time.AfterFunc(500*time.Millisecond, func() {
						cfg, err := f.LoadConfig()
						if err != nil {
							px.Logger.Error("Config reload failed", zap.Error(err))
							return
						}

						px.Logger.Info("Config reloaded",
							zap.String("file", event.Name),
							zap.String("version", cfg.Version))

						select {
						case configChan <- cfg:
						case <-ctx.Done():
						}
					})
					f.mu.Unlock()
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				px.Logger.Error("Watcher error", zap.Error(err))
			}
		}
	}()

	return configChan
}
