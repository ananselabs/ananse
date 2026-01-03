package config

import (
	px "ananse/pkg/proxy"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/go-playground/validator/v10"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

var (
	reloadTimer *time.Timer
	reloadMutex sync.Mutex
	Logger      *zap.Logger
)

type Route struct {
	Path    string   `mapstructure:"path" validate:"required"`
	Methods []string `mapstructure:"methods" validate:"required,dive"`
}

type Endpoint struct {
	Address string `mapstructure:"address" validate:"required,hostname_port"`
}

type Service struct {
	Name      string     `mapstructure:"name" validate:"required"`
	Endpoints []Endpoint `mapstructure:"endpoints" validate:"required,dive"`
	Routes    []Route    `mapstructure:"routes" validate:"required,dive"`
}

type Config struct {
	Services []Service   `mapstructure:"services" validate:"required,dive"`
	Server   ProxyConfig `mapstructure:"proxy"`
}

type ProxyConfig struct {
	Port                int    `mapstructure:"port"`
	MetricsPort         int    `mapstructure:"metrics_port"`
	HealthCheckInterval string `mapstructure:"health_check_interval"`
}

func InitLogger() {
	var err error
	Logger, err = zap.NewProduction()
	if err != nil {
		panic(err)
	}
}

func LoadConfig() (Config, error) {
	if Logger == nil {
		InitLogger()
	}
	defer func(Logger *zap.Logger) {
		err := Logger.Sync()
		if err != nil {

		}
	}(Logger)
	viper.SetConfigName("config")
	viper.AddConfigPath("./config/")
	if err := viper.ReadInConfig(); err != nil {
		return Config{}, fmt.Errorf("error reading config file: %w", err)
	}
	viper.SetEnvPrefix("ANANSE")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()
	viper.BindEnv("proxy.port")
	var config Config
	err := viper.Unmarshal(&config)
	if err != nil {
		return Config{}, fmt.Errorf("failed to parse config file: %w", err)
	}
	validate := validator.New(validator.WithRequiredStructEnabled())
	err = validate.Struct(&config)
	if err := validate.Struct(&config); err != nil {
		return Config{}, fmt.Errorf("config validation failed: %w", err)
	}
	return config, nil
}

func CreateBackends(config Config) []*px.Backend {

	var backends []*px.Backend

	for _, service := range config.Services {

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
	return backends
}
func InitWatcher() *fsnotify.Watcher {
	logger, err := zap.NewProduction()
	if err != nil {
		log.Fatal(err)
	}
	defer logger.Sync()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		Logger.Fatal("Failed to create watcher", zap.Error(err))
	}

	if err := watcher.Add("./config/"); err != nil {
		Logger.Fatal("Failed to add config path", zap.Error(err))
	}

	return watcher
}
func ConfigNotifier(bkPool *px.BackendPool, health *px.Health, watcher *fsnotify.Watcher) {
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				if event.Has(fsnotify.Write) {
					reloadMutex.Lock()
					if reloadTimer != nil {
						reloadTimer.Stop()
					}
					reloadTimer = time.AfterFunc(500*time.Millisecond, func() {
						Logger.Info("Config file changed, reloading...")

						config, err := LoadConfig()
						if err != nil {
							Logger.Error("Reload failed", zap.Error(err))
							return
						}
						backends := CreateBackends(config)

						if len(backends) == 0 {
							Logger.Warn("Config reload: no backends found, keeping old config")
							return
						}

						bkPool.UpdateBackend(backends)

						d, err := time.ParseDuration(config.Server.HealthCheckInterval)
						if err != nil {
							if secs, serr := strconv.Atoi(config.Server.HealthCheckInterval); serr == nil {
								d = time.Duration(secs) * time.Second
							} else {
								Logger.Warn("invalid health_check_interval",
									zap.String("input", config.Server.HealthCheckInterval),
									zap.Error(err),
								)
								d = 3 * time.Second
							}
						}

						health.Restart(d) // ← One line replaces three!
						Logger.Info("Config reloaded successfully")
					})
					reloadMutex.Unlock()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				Logger.Error("Watcher error", zap.Error(err))
			}
		}
	}()

}
