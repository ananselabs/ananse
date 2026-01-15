package controlplane

import (
	pb "ananse/controlplane/cmd/configpb"
	"context"
)

type ConfigWatcher interface {
	Watch(ctx context.Context) <-chan *pb.Config
	validate(config *pb.Config) error
}
