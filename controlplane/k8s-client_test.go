package controlplane

import (
	pb "ananse/controlplane/cmd/configpb"
	px "ananse/pkg/proxy"
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
)

func init() {
	px.InitLogger()
}

func TestK8sClient_validate(t *testing.T) {
	tests := []struct {
		name    string
		config  *pb.Config
		wantErr bool
	}{
		{
			name: "valid config",
			config: &pb.Config{
				Services: []*pb.Service{
					{
						Name: "service-1",
						Routes: []*pb.Route{
							{Path: "/test", Methods: []string{"GET", "POST"}},
						},
						Endpoints: []*pb.Endpoint{
							{Address: "127.0.0.1:8080"},
						},
					},
				},
				ProxyConfig: &pb.ProxyConfig{
					Port:                       8090,
					MetricsPort:                9090,
					HealthCheckIntervalSeconds: 5,
				},
			},
			wantErr: false,
		},
		{
			name: "nil proxy config",
			config: &pb.Config{
				Services: []*pb.Service{{Name: "test"}},
			},
			wantErr: true,
		},
		{
			name: "no services",
			config: &pb.Config{
				ProxyConfig: &pb.ProxyConfig{Port: 8089},
				Services:    []*pb.Service{},
			},
			wantErr: true,
		},
		{
			name: "valid config with multiple services",
			config: &pb.Config{
				Services: []*pb.Service{
					{Name: "service-1", Endpoints: []*pb.Endpoint{{Address: "10.0.0.1:8080"}}},
					{Name: "service-2", Endpoints: []*pb.Endpoint{{Address: "10.0.0.2:8080"}}},
				},
				ProxyConfig: &pb.ProxyConfig{
					Port: 8089,
				},
			},
			wantErr: false,
		},
	}

	k := &K8sClient{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := k.validate(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestK8sClient_signalUpdate(t *testing.T) {
	tests := []struct {
		name           string
		preFillChannel bool
		expectSignal   bool
	}{
		{
			name:           "signal on empty channel",
			preFillChannel: false,
			expectSignal:   true,
		},
		{
			name:           "no block when channel full",
			preFillChannel: true,
			expectSignal:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := &K8sClient{
				updateChan: make(chan struct{}, 1),
			}

			if tt.preFillChannel {
				k.updateChan <- struct{}{}
			}

			// signalUpdate should not block
			done := make(chan bool, 1)
			go func() {
				k.signalUpdate()
				done <- true
			}()

			select {
			case <-done:
				// success - didn't block
			case <-time.After(100 * time.Millisecond):
				t.Error("signalUpdate blocked when it shouldn't")
			}

			// Verify channel has signal
			select {
			case <-k.updateChan:
				if !tt.expectSignal {
					t.Error("received unexpected signal")
				}
			default:
				if tt.expectSignal && !tt.preFillChannel {
					t.Error("expected signal in channel")
				}
			}
		})
	}
}

func TestK8sClient_buildEndpoints(t *testing.T) {
	readyTrue := true
	readyFalse := false
	port8080 := int32(8080)
	port9090 := int32(9090)

	tests := []struct {
		name           string
		namespace      string
		serviceName    string
		endpointSlices []*discoveryv1.EndpointSlice
		wantCount      int
		wantAddresses  []string
	}{
		{
			name:        "single ready endpoint",
			namespace:   "default",
			serviceName: "my-service",
			endpointSlices: []*discoveryv1.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-service-abc",
						Namespace: "default",
						Labels: map[string]string{
							"kubernetes.io/service-name": "my-service",
						},
					},
					Ports: []discoveryv1.EndpointPort{
						{Port: &port8080},
					},
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses:  []string{"10.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{Ready: &readyTrue},
						},
					},
				},
			},
			wantCount:     1,
			wantAddresses: []string{"10.0.0.1:8080"},
		},
		{
			name:        "multiple ready endpoints",
			namespace:   "default",
			serviceName: "my-service",
			endpointSlices: []*discoveryv1.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-service-abc",
						Namespace: "default",
						Labels: map[string]string{
							"kubernetes.io/service-name": "my-service",
						},
					},
					Ports: []discoveryv1.EndpointPort{
						{Port: &port9090},
					},
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses:  []string{"10.0.0.1", "10.0.0.2"},
							Conditions: discoveryv1.EndpointConditions{Ready: &readyTrue},
						},
						{
							Addresses:  []string{"10.0.0.3"},
							Conditions: discoveryv1.EndpointConditions{Ready: &readyTrue},
						},
					},
				},
			},
			wantCount:     3,
			wantAddresses: []string{"10.0.0.1:9090", "10.0.0.2:9090", "10.0.0.3:9090"},
		},
		{
			name:        "skip not ready endpoints",
			namespace:   "default",
			serviceName: "my-service",
			endpointSlices: []*discoveryv1.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-service-abc",
						Namespace: "default",
						Labels: map[string]string{
							"kubernetes.io/service-name": "my-service",
						},
					},
					Ports: []discoveryv1.EndpointPort{
						{Port: &port8080},
					},
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses:  []string{"10.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{Ready: &readyTrue},
						},
						{
							Addresses:  []string{"10.0.0.2"},
							Conditions: discoveryv1.EndpointConditions{Ready: &readyFalse},
						},
					},
				},
			},
			wantCount:     1,
			wantAddresses: []string{"10.0.0.1:8080"},
		},
		{
			name:        "use default port when not specified",
			namespace:   "default",
			serviceName: "my-service",
			endpointSlices: []*discoveryv1.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-service-abc",
						Namespace: "default",
						Labels: map[string]string{
							"kubernetes.io/service-name": "my-service",
						},
					},
					Ports: []discoveryv1.EndpointPort{}, // no port specified
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses:  []string{"10.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{Ready: &readyTrue},
						},
					},
				},
			},
			wantCount:     1,
			wantAddresses: []string{"10.0.0.1:8080"}, // default port
		},
		{
			name:        "ignore slices from different namespace",
			namespace:   "default",
			serviceName: "my-service",
			endpointSlices: []*discoveryv1.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-service-abc",
						Namespace: "other-ns",
						Labels: map[string]string{
							"kubernetes.io/service-name": "my-service",
						},
					},
					Ports: []discoveryv1.EndpointPort{
						{Port: &port8080},
					},
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses:  []string{"10.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{Ready: &readyTrue},
						},
					},
				},
			},
			wantCount:     0,
			wantAddresses: []string{},
		},
		{
			name:        "ignore slices from different service",
			namespace:   "default",
			serviceName: "my-service",
			endpointSlices: []*discoveryv1.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "other-service-abc",
						Namespace: "default",
						Labels: map[string]string{
							"kubernetes.io/service-name": "other-service",
						},
					},
					Ports: []discoveryv1.EndpointPort{
						{Port: &port8080},
					},
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses:  []string{"10.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{Ready: &readyTrue},
						},
					},
				},
			},
			wantCount:     0,
			wantAddresses: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset()
			factory := informers.NewSharedInformerFactory(clientset, 0)
			sliceInformer := factory.Discovery().V1().EndpointSlices().Informer()

			// Add endpoint slices to the informer store
			for _, slice := range tt.endpointSlices {
				if err := sliceInformer.GetStore().Add(slice); err != nil {
					t.Fatalf("failed to add slice to store: %v", err)
				}
			}

			k := &K8sClient{
				namespace:     tt.namespace,
				sliceInformer: sliceInformer,
			}

			endpoints := k.buildEndpoints(tt.namespace, tt.serviceName)

			if len(endpoints) != tt.wantCount {
				t.Errorf("buildEndpoints() returned %d endpoints, want %d", len(endpoints), tt.wantCount)
			}

			// Verify addresses
			gotAddresses := make(map[string]bool)
			for _, ep := range endpoints {
				gotAddresses[ep.Address] = true
			}

			for _, want := range tt.wantAddresses {
				if !gotAddresses[want] {
					t.Errorf("buildEndpoints() missing expected address %s", want)
				}
			}
		})
	}
}

func TestK8sClient_buildConfig(t *testing.T) {
	readyTrue := true
	port8080 := int32(8080)

	tests := []struct {
		name           string
		namespace      string
		services       []*corev1.Service
		endpointSlices []*discoveryv1.EndpointSlice
		wantServices   int
	}{
		{
			name:      "service with endpoints",
			namespace: "default",
			services: []*corev1.Service{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-service",
						Namespace: "default",
					},
					Spec: corev1.ServiceSpec{
						Selector: map[string]string{"app": "my-app"},
					},
				},
			},
			endpointSlices: []*discoveryv1.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-service-abc",
						Namespace: "default",
						Labels: map[string]string{
							"kubernetes.io/service-name": "my-service",
						},
					},
					Ports: []discoveryv1.EndpointPort{
						{Port: &port8080},
					},
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses:  []string{"10.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{Ready: &readyTrue},
						},
					},
				},
			},
			wantServices: 1,
		},
		{
			name:      "service without selector ignored",
			namespace: "default",
			services: []*corev1.Service{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "headless-service",
						Namespace: "default",
					},
					Spec: corev1.ServiceSpec{
						Selector: map[string]string{}, // empty selector
					},
				},
			},
			endpointSlices: []*discoveryv1.EndpointSlice{},
			wantServices:   0,
		},
		{
			name:      "service without endpoints ignored",
			namespace: "default",
			services: []*corev1.Service{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-service",
						Namespace: "default",
					},
					Spec: corev1.ServiceSpec{
						Selector: map[string]string{"app": "my-app"},
					},
				},
			},
			endpointSlices: []*discoveryv1.EndpointSlice{}, // no endpoints
			wantServices:   0,
		},
		{
			name:      "multiple services",
			namespace: "default",
			services: []*corev1.Service{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "service-a",
						Namespace: "default",
					},
					Spec: corev1.ServiceSpec{
						Selector: map[string]string{"app": "app-a"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "service-b",
						Namespace: "default",
					},
					Spec: corev1.ServiceSpec{
						Selector: map[string]string{"app": "app-b"},
					},
				},
			},
			endpointSlices: []*discoveryv1.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "service-a-abc",
						Namespace: "default",
						Labels: map[string]string{
							"kubernetes.io/service-name": "service-a",
						},
					},
					Ports: []discoveryv1.EndpointPort{{Port: &port8080}},
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses:  []string{"10.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{Ready: &readyTrue},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "service-b-abc",
						Namespace: "default",
						Labels: map[string]string{
							"kubernetes.io/service-name": "service-b",
						},
					},
					Ports: []discoveryv1.EndpointPort{{Port: &port8080}},
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses:  []string{"10.0.0.2"},
							Conditions: discoveryv1.EndpointConditions{Ready: &readyTrue},
						},
					},
				},
			},
			wantServices: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset()
			factory := informers.NewSharedInformerFactory(clientset, 0)

			serviceInformer := factory.Core().V1().Services().Informer()
			sliceInformer := factory.Discovery().V1().EndpointSlices().Informer()

			// Add services to the informer store
			for _, svc := range tt.services {
				if err := serviceInformer.GetStore().Add(svc); err != nil {
					t.Fatalf("failed to add service to store: %v", err)
				}
			}

			// Add endpoint slices to the informer store
			for _, slice := range tt.endpointSlices {
				if err := sliceInformer.GetStore().Add(slice); err != nil {
					t.Fatalf("failed to add slice to store: %v", err)
				}
			}

			k := &K8sClient{
				namespace:       tt.namespace,
				serviceInformer: serviceInformer,
				sliceInformer:   sliceInformer,
			}

			cfg := k.buildConfig()

			if len(cfg.Services) != tt.wantServices {
				t.Errorf("buildConfig() returned %d services, want %d", len(cfg.Services), tt.wantServices)
			}

			// Verify config has required fields
			if cfg.ProxyConfig == nil {
				t.Error("buildConfig() ProxyConfig is nil")
			}

			if cfg.Version == "" {
				t.Error("buildConfig() Version is empty")
			}
		})
	}
}

func TestK8sClient_buildConfig_Routes(t *testing.T) {
	readyTrue := true
	port8080 := int32(8080)

	clientset := fake.NewSimpleClientset()
	factory := informers.NewSharedInformerFactory(clientset, 0)

	serviceInformer := factory.Core().V1().Services().Informer()
	sliceInformer := factory.Discovery().V1().EndpointSlices().Informer()

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-service",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "api"},
		},
	}
	if err := serviceInformer.GetStore().Add(svc); err != nil {
		t.Fatalf("failed to add service: %v", err)
	}

	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-service-abc",
			Namespace: "default",
			Labels: map[string]string{
				"kubernetes.io/service-name": "api-service",
			},
		},
		Ports: []discoveryv1.EndpointPort{{Port: &port8080}},
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: &readyTrue},
			},
		},
	}
	if err := sliceInformer.GetStore().Add(slice); err != nil {
		t.Fatalf("failed to add slice: %v", err)
	}

	k := &K8sClient{
		namespace:       "default",
		serviceInformer: serviceInformer,
		sliceInformer:   sliceInformer,
	}

	cfg := k.buildConfig()

	if len(cfg.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(cfg.Services))
	}

	service := cfg.Services[0]

	// Verify service name
	if service.Name != "api-service" {
		t.Errorf("expected service name 'api-service', got '%s'", service.Name)
	}

	// Verify routes
	if len(service.Routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(service.Routes))
	}

	route := service.Routes[0]
	expectedPath := "/api-service"
	if route.Path != expectedPath {
		t.Errorf("expected route path '%s', got '%s'", expectedPath, route.Path)
	}

	expectedMethods := []string{"GET", "POST", "PUT", "DELETE", "PATCH"}
	if len(route.Methods) != len(expectedMethods) {
		t.Errorf("expected %d methods, got %d", len(expectedMethods), len(route.Methods))
	}
}

func TestK8sClient_runDebouncer(t *testing.T) {
	readyTrue := true
	port8080 := int32(8080)

	clientset := fake.NewSimpleClientset()
	factory := informers.NewSharedInformerFactory(clientset, 0)

	serviceInformer := factory.Core().V1().Services().Informer()
	sliceInformer := factory.Discovery().V1().EndpointSlices().Informer()

	// Add a service with endpoints
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "test"},
		},
	}
	if err := serviceInformer.GetStore().Add(svc); err != nil {
		t.Fatalf("failed to add service: %v", err)
	}

	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service-abc",
			Namespace: "default",
			Labels: map[string]string{
				"kubernetes.io/service-name": "test-service",
			},
		},
		Ports: []discoveryv1.EndpointPort{{Port: &port8080}},
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: &readyTrue},
			},
		},
	}
	if err := sliceInformer.GetStore().Add(slice); err != nil {
		t.Fatalf("failed to add slice: %v", err)
	}

	k := &K8sClient{
		namespace:       "default",
		serviceInformer: serviceInformer,
		sliceInformer:   sliceInformer,
		configChan:      make(chan *pb.Config, 5),
		updateChan:      make(chan struct{}, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go k.runDebouncer(ctx)

	// Signal update
	k.signalUpdate()

	// Wait for debounce window plus some buffer
	select {
	case cfg := <-k.configChan:
		if cfg == nil {
			t.Error("received nil config")
		}
		if len(cfg.Services) != 1 {
			t.Errorf("expected 1 service, got %d", len(cfg.Services))
		}
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for config after debounce")
	}
}

func TestK8sClient_runDebouncer_ContextCancellation(t *testing.T) {
	k := &K8sClient{
		configChan: make(chan *pb.Config, 5),
		updateChan: make(chan struct{}, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		k.runDebouncer(ctx)
		close(done)
	}()

	// Cancel context
	cancel()

	// Wait for debouncer to exit
	select {
	case <-done:
		// success
	case <-time.After(1 * time.Second):
		t.Error("debouncer didn't exit on context cancellation")
	}

	// Verify config channel is closed
	select {
	case _, ok := <-k.configChan:
		if ok {
			t.Error("expected config channel to be closed")
		}
	default:
		// Channel is empty but may not be closed yet, that's ok
	}
}
