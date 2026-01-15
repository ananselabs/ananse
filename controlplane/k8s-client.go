package controlplane

import (
	pb "ananse/controlplane/cmd/configpb"
	px "ananse/pkg/proxy"
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const resyncPeriod = 10 * time.Minute

type K8sClient struct {
	clientset  kubernetes.Interface
	factory    informers.SharedInformerFactory
	namespace  string
	configChan chan *pb.Config
	updateChan chan struct{}
	mu         sync.Mutex
}

func NewK8sClient(namespace string) (*K8sClient, error) {
	if px.Logger == nil {
		px.InitLogger()
	}

	// Load kubeconfig
	var kubeconfig string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = filepath.Join(home, ".kube", "config")
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	factory := informers.NewSharedInformerFactory(clientset, resyncPeriod)

	return &K8sClient{
		clientset:  clientset,
		factory:    factory,
		namespace:  namespace,
		configChan: make(chan *pb.Config, 10),
		updateChan: make(chan struct{}, 10),
	}, nil
}

func (k *K8sClient) Watch(ctx context.Context) <-chan *pb.Config {
	go func() {
		defer close(k.configChan)

		// Get informers
		serviceInformer := k.factory.Core().V1().Services().Informer()
		sliceInformer := k.factory.Discovery().V1().EndpointSlices().Informer()

		// Add event handlers AFTER we start factory
		stopCh := make(chan struct{})
		defer close(stopCh)

		// Start factory FIRST
		k.factory.Start(stopCh)
		go k.debouncer(ctx)
		// Wait for cache sync BEFORE adding handlers
		if !cache.WaitForCacheSync(stopCh, serviceInformer.HasSynced, sliceInformer.HasSynced) {
			px.Logger.Error("Failed to sync K8s cache")
			return
		}

		px.Logger.Info("K8s cache synced, watching for changes")

		// NOW add handlers (avoids initial flood)
		serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				k.onConfigChange("Service added")
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				k.onConfigChange("Service updated")
			},
			DeleteFunc: func(obj interface{}) {
				k.onConfigChange("Service deleted")
			},
		})

		sliceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				k.onConfigChange("EndpointSlice added")
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				k.onConfigChange("EndpointSlice updated")
			},
			DeleteFunc: func(obj interface{}) {
				k.onConfigChange("EndpointSlice deleted")
			},
		})

		// Build and send initial config
		cfg := k.buildConfigFromK8s()
		select {
		case k.configChan <- cfg:
			px.Logger.Info("Initial K8s config loaded",
				zap.String("version", cfg.Version),
				zap.Int("services", len(cfg.Services)))
		case <-ctx.Done():
			return
		}

		// Watch for changes
		<-ctx.Done()
		px.Logger.Info("K8s watcher stopped")
	}()

	return k.configChan
}

func (k *K8sClient) onConfigChange(reason string) {
	//cfg := k.buildConfigFromK8s()
	select {
	case k.updateChan <- struct{}{}:
		px.Logger.Info("Config updated", zap.String("reason", reason))
	default:
		// Channel full, drop update (last config still valid)
		px.Logger.Warn("Config update dropped (channel full)", zap.String("reason", reason))
	}
}

func (k *K8sClient) buildConfigFromK8s() *pb.Config {
	config := &pb.Config{
		Version: fmt.Sprintf("k8s-%d", time.Now().Unix()),
		ProxyConfig: &pb.ProxyConfig{
			Port:                       8080,
			MetricsPort:                9090,
			HealthCheckIntervalSeconds: 10,
		},
		Services: []*pb.Service{},
	}

	// Get services from informer
	serviceInformer := k.factory.Core().V1().Services().Informer()
	services := serviceInformer.GetStore().List()

	for _, obj := range services {
		svc := obj.(*corev1.Service)

		// Skip kube-system, kube-public, etc
		if svc.Namespace != k.namespace && k.namespace != "" {
			continue
		}

		// Skip services without selector
		if len(svc.Spec.Selector) == 0 {
			continue
		}

		// Build endpoints
		endpoints := k.getEndpointsForService(svc.Namespace, svc.Name)

		if len(endpoints) == 0 {
			continue
		}

		service := &pb.Service{
			Name:      svc.Name,
			Endpoints: endpoints,
			Routes: []*pb.Route{
				{
					Path:    fmt.Sprintf("/%s", svc.Name),
					Methods: []string{"GET", "POST", "PUT", "DELETE", "PATCH"},
				},
			},
		}

		config.Services = append(config.Services, service)
	}

	px.Logger.Info("Built config from K8s",
		zap.Int("services", len(config.Services)),
		zap.Int("total_store_items", len(services)))

	return config
}

func (k *K8sClient) getEndpointsForService(namespace, serviceName string) []*pb.Endpoint {
	var endpoints []*pb.Endpoint

	sliceInformer := k.factory.Discovery().V1().EndpointSlices().Informer()
	slices := sliceInformer.GetStore().List()

	for _, obj := range slices {
		slice := obj.(*discoveryv1.EndpointSlice)

		// Check if this EndpointSlice belongs to the service
		svcName, ok := slice.Labels["kubernetes.io/service-name"]
		if !ok || svcName != serviceName {
			continue
		}

		if slice.Namespace != namespace {
			continue
		}

		// Get port
		var port int32 = 8080 // Default
		if len(slice.Ports) > 0 && slice.Ports[0].Port != nil {
			port = *slice.Ports[0].Port
		}

		// Extract ready endpoints
		for _, endpoint := range slice.Endpoints {
			if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
				continue
			}

			for _, addr := range endpoint.Addresses {
				endpoints = append(endpoints, &pb.Endpoint{
					Address: fmt.Sprintf("%s:%d", addr, port),
				})
			}
		}
	}

	return endpoints
}

func (k *K8sClient) validate(config *pb.Config) error {
	if config.ProxyConfig == nil {
		return fmt.Errorf("proxy_config is required")
	}
	if len(config.Services) == 0 {
		return fmt.Errorf("at least one service is required")
	}
	return nil
}

func (k *K8sClient) debouncer(ctx context.Context) {
	ticker := time.NewTimer(500 * time.Millisecond) // Wait 500ms for updates to settle
	ticker.Stop()

	for {
		select {
		case <-k.updateChan:
			// Reset timer - wait for updates to stop
			ticker.Reset(500 * time.Millisecond)
		case <-ticker.C:
			// 500ms passed without new updates - build and send config
			cfg := k.buildConfigFromK8s()
			select {
			case k.configChan <- cfg:
				px.Logger.Info("Debounced config update", zap.Int("services", len(cfg.Services)))
			default:
				px.Logger.Warn("Config channel full, dropping update")
			}
		case <-ctx.Done():
			return
		}
	}
}
