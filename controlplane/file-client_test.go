package controlplane

import (
	pb "ananse/controlplane/cmd/configpb"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileClient_validateConfig(t *testing.T) {
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
			name:    "nil config",
			config:  nil,
			wantErr: true,
		},
		{
			name: "nil proxy config",
			config: &pb.Config{
				Services: []*pb.Service{{Name: "test"}},
			},
			wantErr: true,
		},
		{
			name: "zero port",
			config: &pb.Config{
				ProxyConfig: &pb.ProxyConfig{Port: 0},
				Services:    []*pb.Service{{Name: "test"}},
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
	}

	fileClient := &FileClient{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fileClient.validate(tt.config)

			if (err != nil) != tt.wantErr {
				t.Errorf("validateConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestFileClient_Watch_ContextCancellation(t *testing.T) {
	// Create valid config file
	tmpFile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}

	t.Cleanup(func() {
		os.Remove(tmpFile.Name())
	})

	yamlContent := `
proxy:
  port: 8089
  metrics_port: 9090
  health_check_interval: 3
services:
  - name: test
    endpoints:
      - address: localhost:5001
    routes:
      - path: /test
        methods: [GET]
`
	if err := os.WriteFile(tmpFile.Name(), []byte(yamlContent), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	dir := filepath.Dir(tmpFile.Name())
	filename := filepath.Base(tmpFile.Name())
	name := strings.TrimSuffix(filename, filepath.Ext(filename))

	client := NewFileClient(dir, name, "yaml")

	// Test context cancellation
	ctx, cancel := context.WithCancel(context.Background())
	configChan := client.Watch(ctx)

	// Receive initial config
	select {
	case cfg := <-configChan:
		if cfg == nil {
			t.Error("expected initial config, got nil")
		}
	case <-time.After(1 * time.Second):
		t.Error("timeout waiting for initial config")
	}

	// Cancel context
	cancel()

	// Channel should close
	select {
	case _, ok := <-configChan:
		if ok {
			t.Error("expected channel to close after context cancellation")
		}
	case <-time.After(1 * time.Second):
		t.Error("channel didn't close after context cancellation")
	}
}
