<div align="center">
  <img src="ananse.png" alt="Ananse Mascot" width="200"/>
  <h1>Ananse</h1>
  <p><strong>A Service Mesh in Go</strong></p>
  <p><em>Learning distributed systems through building production-grade infrastructure</em></p>

  ![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat&logo=go)
  ![License](https://img.shields.io/badge/license-MIT-green)
  ![Status](https://img.shields.io/badge/status-active%20development-blue)
</div>

---

## What is Ananse?

**Ananse** is a service mesh built in Go, named after the Akan folktale spider known for wisdom and cleverness. It provides transparent traffic interception, load balancing, and observability for microservices.

### Two Operating Modes

| Mode | Purpose | Use Case |
|------|---------|----------|
| **Sidecar** | Transparent proxy using iptables | Injected into pods/containers, intercepts all traffic |
| **Gateway** | Reverse proxy with routing | Edge proxy, API gateway, explicit proxying |

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      Control Plane                          │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────┐  │
│  │   gRPC      │  │  Webhook    │  │  Config Watcher     │  │
│  │   Server    │  │  (K8s)      │  │  (File/K8s)         │  │
│  └─────────────┘  └─────────────┘  └─────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
                              │
                              │ gRPC Stream
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                       Data Plane                            │
│  ┌─────────────────────────────────────────────────────┐    │
│  │                    Sidecar Proxy                    │    │
│  │  ┌───────────┐              ┌───────────┐          │    │
│  │  │  Inbound  │              │  Outbound │          │    │
│  │  │  :15006   │              │  :15001   │          │    │
│  │  └───────────┘              └───────────┘          │    │
│  │        ▲                          │                │    │
│  │        │ iptables REDIRECT        │ SO_ORIGINAL_DST│    │
│  │        │                          ▼                │    │
│  │  [External Traffic]        [Original Destination]  │    │
│  └─────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────┘
```

---

## Features

### Traffic Management
- **Transparent Proxying**: iptables-based traffic interception (sidecar mode)
- **Load Balancing**: Round-robin and least-connections algorithms
- **Circuit Breaker**: Automatic failure detection with Open/HalfOpen/Closed states
- **Health Checking**: Active and passive health monitoring with exponential backoff

### Kubernetes Integration
- **Automatic Sidecar Injection**: MutatingWebhook injects proxy into annotated pods
- **Namespace Exclusions**: Skips system namespaces to prevent deadlocks
- **Security Hardened**: Non-root containers, dropped capabilities, read-only filesystem

### Observability
- **Prometheus Metrics**: Request counts, latencies, circuit breaker states
- **Distributed Tracing**: OpenTelemetry integration with Tempo/Jaeger
- **Structured Logging**: JSON logs with trace correlation

---

## Quick Start

### Option 1: Kubernetes Deployment

**Prerequisites:** kubectl, kind/minikube, Docker

```bash
# Build and push images
docker build -f docker/Dockerfile.controlplane -t anthony4m/ananse-controlplane:v1 .
docker build -f docker/Dockerfile.proxy -t anthony4m/ananse-proxy:v1 .
docker build -f docker/Dockerfile.init -t anthony4m/ananse-init:v1 .

docker push anthony4m/ananse-controlplane:v1
docker push anthony4m/ananse-proxy:v1
docker push anthony4m/ananse-init:v1

# Generate TLS certificates
./scripts/generate-certs.sh

# Deploy to cluster
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/injector-config.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/webhook-deployment.yaml
kubectl apply -f deploy/webhook-service.yaml
kubectl apply -f deploy/webhook-config.yaml

# Test injection - deploy a pod with the annotation
kubectl run test-app --image=nginx \
  --annotations="sidecar.ananse.io/inject=true"

# Verify sidecar was injected
kubectl get pod test-app -o jsonpath='{.spec.containers[*].name}'
# Should show: nginx ananse-proxy
```

### Option 2: Docker Compose (No Kubernetes)

**Sidecar mode** - transparent proxying:

```yaml
# docker-compose.yml
version: '3.8'
services:
  sidecar:
    image: anthony4m/ananse-proxy:v1
    environment:
      - ANANSE_MODE=sidecar
    cap_add:
      - NET_ADMIN
      - NET_RAW
    volumes:
      - ./scripts/iptables-init.sh:/iptables-init.sh
    entrypoint: ["/bin/sh", "-c", "/iptables-init.sh && /ananse-proxy"]

  my-app:
    image: nginx
    network_mode: "service:sidecar"
    depends_on:
      - sidecar
```

**Gateway mode** - reverse proxy:

```yaml
version: '3.8'
services:
  controlplane:
    image: anthony4m/ananse-controlplane:v1
    ports:
      - "50051:50051"
    volumes:
      - ./config:/config

  proxy:
    image: anthony4m/ananse-proxy:v1
    environment:
      - ANANSE_MODE=gateway
      - CONTROL_PLANE_ENDPOINT=controlplane:50051
    ports:
      - "8080:8080"
```

### Option 3: Local Development

```bash
# Run control plane
go run ./controlplane/cmd/

# Run proxy in gateway mode (default)
go run ./proxy/

# Run proxy in sidecar mode (requires Linux + iptables)
ANANSE_MODE=sidecar go run ./proxy/
```

---

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `ANANSE_MODE` | `gateway` | Operating mode: `gateway` or `sidecar` |
| `SIDECAR_IMAGE` | `anthony4m/ananse-proxy:v1` | Image for injected sidecars |
| `INIT_IMAGE` | `anthony4m/ananse-init:v1` | Image for init container |
| `PROXY_PORT` | `15001` | Outbound listener port |
| `INBOUND_PORT` | `15006` | Inbound listener port |
| `PROXY_UID` | `1337` | UID for sidecar (iptables bypass) |

### Pod Annotations

| Annotation | Values | Description |
|------------|--------|-------------|
| `sidecar.ananse.io/inject` | `true`/`false` | Enable/disable sidecar injection |
| `sidecar.ananse.io/status` | `injected` | Set automatically after injection |

### Excluded Namespaces

Injection is automatically skipped for:
- `kube-system`
- `kube-public`
- `cert-manager`
- `ananse-system`

---

## Project Structure

```
ananse/
├── controlplane/
│   ├── cmd/
│   │   ├── main.go              # Control plane entry point
│   │   └── server.go            # gRPC server
│   ├── injector/
│   │   ├── injector.go          # Sidecar injection logic
│   │   └── webhook.go           # Webhook server
│   ├── file-client.go           # File-based config watcher
│   └── k8s-client.go            # K8s service discovery
│
├── pkg/proxy/
│   ├── listener.go              # Inbound/outbound listeners
│   ├── originaldst.go           # SO_ORIGINAL_DST syscall
│   ├── handler.go               # Request handling
│   ├── backend.go               # Backend pool management
│   ├── health.go                # Health checking
│   └── circuit.go               # Circuit breaker
│
├── proxy/
│   └── main.go                  # Proxy entry point
│
├── scripts/
│   ├── iptables-init.sh         # Traffic interception rules
│   └── generate-certs.sh        # TLS certificate generation
│
├── deploy/
│   ├── namespace.yaml
│   ├── rbac.yaml
│   ├── injector-config.yaml     # ConfigMap for image settings
│   ├── webhook-deployment.yaml
│   ├── webhook-service.yaml
│   └── webhook-config.yaml      # MutatingWebhookConfiguration
│
└── docker/
    ├── Dockerfile.controlplane
    ├── Dockerfile.proxy
    └── Dockerfile.init
```

---

## Platform Requirements

| Mode | Requirements |
|------|--------------|
| **Sidecar** | Linux, iptables, NET_ADMIN capability |
| **Gateway** | Any OS (Linux, macOS, Windows) |
| **Control Plane** | Any OS |

The sidecar mode uses `SO_ORIGINAL_DST` to recover original destinations after iptables REDIRECT. This is a Linux-only syscall.

---

## Monitoring

### Prometheus Metrics

```bash
curl http://localhost:9090/metrics
```

Key metrics:
- `ananse_requests_total` - Request count by status
- `ananse_request_duration_seconds` - Latency histogram
- `ananse_circuit_breaker_state` - Circuit breaker status
- `ananse_backend_health` - Backend health status

### Grafana Dashboards

Access Grafana at [http://localhost:3000](http://localhost:3000) (default: admin/admin)

---

## License

MIT License
