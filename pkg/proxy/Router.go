package proxy

import (
	"fmt"
	"net/http"
	"sync"
)

type Router struct {
	mu           sync.RWMutex
	routingTable *RoutingTable
}

// FindServiceByVIP looks up service name by ClusterIP:port (from SO_ORIGINAL_DST)
func (r *Router) FindServiceByVIP(vip string) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.routingTable == nil || r.routingTable.vipIndex == nil {
		return "", fmt.Errorf("routing table not initialized")
	}

	if serviceName, ok := r.routingTable.vipIndex[vip]; ok {
		return serviceName, nil
	}
	return "", fmt.Errorf("no service found for VIP: %s", vip)
}

// FindService looks up service by path and method (for path-based routing)
func (r *Router) FindService(req *http.Request) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if methods, ok := r.routingTable.pathIndex[req.URL.Path]; ok {
		if serviceName, ok := methods[req.Method]; ok {
			return serviceName, nil
		}
	}
	return "", fmt.Errorf("no service found for this route")
}

func (r *Router) UpdateRoutes(rt *RoutingTable) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Build path index from services
	rt.pathIndex = make(map[string]map[string]string)
	for serviceName, svc := range rt.Services {
		for _, route := range svc.Routes {
			if rt.pathIndex[route.Path] == nil {
				rt.pathIndex[route.Path] = make(map[string]string)
			}
			for method := range route.Methods {
				rt.pathIndex[route.Path][method] = serviceName
			}
		}
	}

	// Build VIP index from services
	rt.vipIndex = make(map[string]string)
	for serviceName, svc := range rt.Services {
		if svc.ClusterIP != "" && svc.Port > 0 {
			vip := fmt.Sprintf("%s:%d", svc.ClusterIP, svc.Port)
			rt.vipIndex[vip] = serviceName
		}
	}

	r.routingTable = rt
}
