// controlplane/k8s-client.go
package controlplane

import (
	pb "ananse/controlplane/cmd/configpb"
	"context"
	"fmt"
)

type K8sClient struct {
	// Will add: clientset, informer factory, etc.
}

func NewK8sClient() (*K8sClient, error) {
	// TODO: Implement K8s client initialization
	return nil, fmt.Errorf("K8s client not yet implemented")
}

func (k *K8sClient) Watch(ctx context.Context) <-chan *pb.Config {
	configChan := make(chan *pb.Config)

	go func() {
		defer close(configChan)
		// TODO: Implement K8s informer watching and conversion to pb.Config
		<-ctx.Done()
	}()

	return configChan
}

func (k *K8sClient) validate(config *pb.Config) error {
	return nil
}
