package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const resyncPeriod = 10 * time.Minute

func main() {
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	factory := informers.NewSharedInformerFactory(clientset, resyncPeriod)

	// Traffic
	serviceInformer := factory.Core().V1().Services().Informer()
	sliceInformer := factory.Discovery().V1().EndpointSlices().Informer()

	// Identity
	podInformer := factory.Core().V1().Pods().Informer()

	// Config
	namespaceInformer := factory.Core().V1().Namespaces().Informer()

	// Service handlers
	serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svc := obj.(*corev1.Service)
			handleServiceChange(svc, "ADDED")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			svc := newObj.(*corev1.Service)
			handleServiceChange(svc, "MODIFIED")
		},
		DeleteFunc: func(obj interface{}) {
			svc := obj.(*corev1.Service)
			handleServiceChange(svc, "DELETED")
		},
	})

	// EndpointSlice handlers
	sliceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			slice := obj.(*discoveryv1.EndpointSlice)
			handleEndpointChange(slice, "ADDED")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			slice := newObj.(*discoveryv1.EndpointSlice)
			handleEndpointChange(slice, "MODIFIED")
		},
		DeleteFunc: func(obj interface{}) {
			slice := obj.(*discoveryv1.EndpointSlice)
			handleEndpointChange(slice, "DELETED")
		},
	})

	// Pod handlers
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			handlePodChange(pod, "ADDED")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			pod := newObj.(*corev1.Pod)
			handlePodChange(pod, "MODIFIED")
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			handlePodChange(pod, "DELETED")
		},
	})

	// Namespace handlers
	namespaceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ns := obj.(*corev1.Namespace)
			handleNamespaceChange(ns, "ADDED")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			ns := newObj.(*corev1.Namespace)
			handleNamespaceChange(ns, "MODIFIED")
		},
		DeleteFunc: func(obj interface{}) {
			ns := obj.(*corev1.Namespace)
			handleNamespaceChange(ns, "DELETED")
		},
	})

	stopCh := make(chan struct{})
	defer close(stopCh)

	fmt.Println("Starting Ananse Discovery Engine")
	factory.Start(stopCh)

	factory.WaitForCacheSync(stopCh)
	fmt.Println("Initial Cache Synced. Watching for changes...")

	<-stopCh
}

func handleServiceChange(svc *corev1.Service, eventType string) {
	fmt.Printf("[%s] Service: %s/%s\n", eventType, svc.Namespace, svc.Name)

	if len(svc.Spec.Selector) == 0 {
		fmt.Printf("  -> 🌎 Service WITHOUT selector (external endpoints)\n")
	} else {
		fmt.Printf("  -> 📦 Service WITH selector: %v\n", svc.Spec.Selector)
	}

	for _, port := range svc.Spec.Ports {
		fmt.Printf("  -> Port: %s %d/%s\n", port.Name, port.Port, port.Protocol)
	}
}

func handleEndpointChange(slice *discoveryv1.EndpointSlice, eventType string) {
	serviceName := slice.Labels["kubernetes.io/service-name"]
	if serviceName == "" {
		serviceName = slice.Name
	}

	fmt.Printf("[%s] EndpointSlice: %s/%s (Service: %s)\n",
		eventType, slice.Namespace, slice.Name, serviceName)

	for _, port := range slice.Ports {
		portNum := int32(0)
		if port.Port != nil {
			portNum = *port.Port
		}
		portName := ""
		if port.Name != nil {
			portName = *port.Name
		}
		protocol := corev1.ProtocolTCP
		if port.Protocol != nil {
			protocol = *port.Protocol
		}

		fmt.Printf("  -> Port: %s %d/%s\n", portName, portNum, protocol)
	}

	for _, endpoint := range slice.Endpoints {
		ready := true
		if endpoint.Conditions.Ready != nil {
			ready = *endpoint.Conditions.Ready
		}

		if !ready {
			continue
		}

		zone := "unknown"
		if endpoint.Zone != nil {
			zone = *endpoint.Zone
		}

		for _, addr := range endpoint.Addresses {
			if endpoint.TargetRef == nil {
				fmt.Printf("    -> 🌎 EXTERNAL IP: %s (zone: %s)\n", addr, zone)
			} else {
				fmt.Printf("    -> 📦 POD IP: %s (pod: %s, zone: %s)\n",
					addr, endpoint.TargetRef.Name, zone)
			}
		}
	}
}

func handlePodChange(pod *corev1.Pod, eventType string) {
	fmt.Printf("[%s] Pod: %s/%s (IP: %s, Phase: %s)\n",
		eventType, pod.Namespace, pod.Name, pod.Status.PodIP, pod.Status.Phase)
}

func handleNamespaceChange(ns *corev1.Namespace, eventType string) {
	fmt.Printf("[%s] Namespace: %s (Phase: %s)\n",
		eventType, ns.Name, ns.Status.Phase)
}
