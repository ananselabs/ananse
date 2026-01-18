package controlplane

import (
	pb "ananse/controlplane/cmd/configpb"
	"testing"
)

func TestFileClient_LoadConfig(t *testing.T) {
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
							{
								Path:    "/test",
								Methods: []string{"GET", "POST"},
							},
						},
						Endpoints: nil,
					},
				},
				ProxyConfig: &pb.ProxyConfig{
					Port:                       8090,
					MetricsPort:                9090,
					HealthCheckIntervalSeconds: 5,
				},
			},
			wantErr: true,
		}, {
			name:    "nil config",
			config:  nil,
			wantErr: true,
		},
		{
			name: "nil proxy config",
			config: &pb.Config{
				Services: []*pb.Service{
					{Name: "test"},
				},
			},
			wantErr: true,
		},
		{
			name: "zero port",
			config: &pb.Config{
				ProxyConfig: &pb.ProxyConfig{
					Port: 0,
				},
				Services: []*pb.Service{
					{Name: "test"},
				},
			},
			wantErr: true,
		},
		{
			name: "no services",
			config: &pb.Config{
				ProxyConfig: &pb.ProxyConfig{
					Port: 8089,
				},
				Services: []*pb.Service{},
			},
			wantErr: true,
		},
	}
	fileCient := FileClient{}
	for _, test := range tests {
		err := fileCient.validate(test.config)
		if (err != nil) && test.wantErr {
			t.Errorf("validateConfig() error = %v, wantErr %v", err, test.wantErr)
		}
	}

}
