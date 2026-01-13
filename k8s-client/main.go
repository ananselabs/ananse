package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

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

	//create clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	watcher, err := clientset.CoreV1().ConfigMaps("").Watch(context.TODO(), metav1.ListOptions{})
	if err != nil {
		panic(err)
	}

	for event := range watcher.ResultChan() {
		service, ok := event.Object.(*v1.ConfigMap)
		if !ok {
			fmt.Printf("unexpected type %T\n", event.Object)
			continue
		}

		switch event.Type {
		case watch.Added:
			fmt.Printf("Service ADDED: %s\n", service.Name)
		case watch.Modified:
			fmt.Printf("Service MODIFIED: %s\n", service.Name)
		case watch.Deleted:
			fmt.Printf("Service DELETED: %s\n", service.Name)
		}
	}

}
