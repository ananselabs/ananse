// cmd/controlplane.go
package main

import (
	pb "ananse/controlplane/cmd/configpb"
	px "ananse/pkg/proxy"
	"fmt"
	"net"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Server struct {
	pb.ControlPlaneServer
	mu            sync.Mutex
	currentConfig *pb.Config
	subscribers   map[string]chan *pb.Config
	version       uint64
}

func NewServer(config *pb.Config) (*Server, error) {
	if config == nil {
		return nil, fmt.Errorf("config is required")
	}
	s := &Server{
		currentConfig: &pb.Config{
			Services:    config.Services,
			ProxyConfig: config.ProxyConfig,
			LastUpdated: timestamppb.Now(),
			Version:     "0",
		},
		// Set initial version
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
	configChan := make(chan *pb.Config, 10)

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
	px.Logger.Info("Updating config")

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

func (s *Server) Start(addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	grpcServer := grpc.NewServer()
	pb.RegisterControlPlaneServer(grpcServer, s)

	px.Logger.Info("Control plane started", zap.String("addr", addr))
	return grpcServer.Serve(lis)
}

func (s *Server) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Close all subscriber channels
	for proxyID, ch := range s.subscribers {
		close(ch)
		px.Logger.Info("Closed subscriber", zap.String("proxy_id", proxyID))
	}

	s.subscribers = nil
}
