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

	// Build index from services
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

	r.routingTable = rt
}
