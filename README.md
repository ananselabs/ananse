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

### Service Discovery
- **Kubernetes**: Native service discovery via EndpointSlices
- **Consul**: Watches Consul catalog for service changes
- **File-based**: Static configuration from YAML/JSON files

### Observability
- **Prometheus Metrics**: Request counts, latencies, circuit breaker states
- **Distributed Tracing**: OpenTelemetry integration with Tempo/Jaeger
- **Structured Logging**: JSON logs with trace correlation

---

## Quick Start

### Option 1: Helm — published chart (Recommended)

**Prerequisites:** Helm 3.0+, kubectl, Kubernetes 1.19+

```bash
# 1. Add the Helm repo
helm repo add ananse https://ananselabs.github.io/ananse
helm repo update

# 2. Generate TLS certs for the mutating webhook
bash <(curl -sL https://raw.githubusercontent.com/ananselabs/ananse/main/scripts/generate-certs.sh)

# 3. Install Ananse
helm install ananse ananse/ananse \
  --set-file caBundle=./ca.crt.b64 \
  -n ananse-system --create-namespace

# 4. Label your namespace — injection activates on next pod create
kubectl label namespace default ananse.io/inject=enabled

# 5. Restart your workloads to get sidecars
kubectl rollout restart deployment -n default
```

**Verify injection:**
```bash
kubectl get pod -n default -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[*].name}{"\n"}{end}'
# Each pod should show: <app-name>  ananse-proxy
```

**With tracing (Tempo/Jaeger):**
```bash
helm install ananse ananse/ananse \
  --set-file caBundle=./ca.crt.b64 \
  --set observability.tracing.enabled=true \
  --set observability.tracing.endpoint=tempo.monitoring.svc:4317 \
  -n ananse-system --create-namespace
```

**With Prometheus Operator (PodMonitor auto-scraping):**

If your cluster has Prometheus Operator, the PodMonitor is deployed automatically when `serviceMonitor.enabled: true` (default). Prometheus will discover all injected sidecars across all namespaces via the `sidecar.ananse.io/status: injected` label.

If your Prometheus CR doesn't watch all namespaces, apply RBAC for cross-namespace discovery:
```bash
# Give Prometheus SA cluster-scope pod read access
kubectl apply -f https://raw.githubusercontent.com/ananselabs/ananse/main/k8s/prometheus-rbac.yaml
```

See [ananse-chart/README.md](ananse-chart/README.md) for full configuration options.

### Option 2: kubectl (Manual)

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

### Option 3: VM/Docker with Consul

For non-Kubernetes environments using Consul for service discovery:

```bash
# Run control plane with Consul discovery
./controlplane -consul -consul-addr consul.example.com:8500

# With tag filtering (only services tagged "ananse")
./controlplane -consul -consul-addr consul.example.com:8500 -consul-tag ananse

# Run proxy in gateway mode
./proxy
```

### Option 4: Local Development

```bash
# Run control plane with Kubernetes discovery
go run ./controlplane/cmd/ -k8s

# Run control plane with Consul discovery
go run ./controlplane/cmd/ -consul -consul-addr localhost:8500

# Run control plane with file-based config
go run ./controlplane/cmd/ -config-path ./config -config-name services

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
| `ANANSE_TRACING_ENABLED` | `""` | Set to `"false"` to disable tracing entirely |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` | OTLP gRPC endpoint for traces |
| `FILTER_HEALTH_CHECKS` | `"false"` | Set to `"true"` to drop successful health probe spans from Tempo |

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
│   ├── consul-client.go         # Consul service discovery
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
├── ananse-chart/                # Helm chart
│   ├── Chart.yaml               # Dependencies (observability stack)
│   ├── values.yaml              # Configuration
│   └── templates/               # K8s manifests
│
├── scripts/
│   ├── iptables-init.sh         # Traffic interception rules
│   └── generate-certs.sh        # TLS certificate generation
│
├── deploy/                      # Raw K8s manifests (use Helm instead)
│   ├── namespace.yaml
│   ├── rbac.yaml
│   ├── injector-config.yaml
│   ├── webhook-deployment.yaml
│   ├── webhook-service.yaml
│   └── webhook-config.yaml
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

## Observability

### Metrics

Each sidecar proxy exposes Prometheus metrics on port **15021** at `/metrics`. The controlplane does **not** expose metrics.

```bash
# Scrape a sidecar directly
curl http://<pod-ip>:15021/metrics
```

Key metrics:
- `ananse_requests_total` - Request count by status
- `ananse_request_duration_seconds` - Latency histogram
- `ananse_circuit_breaker_state` - Circuit breaker status
- `ananse_backend_health` - Backend health status

**Prometheus scraping:**

| Setup | How |
|---|---|
| Prometheus Operator installed | PodMonitor auto-deployed by Helm chart (`serviceMonitor.enabled: true`) |
| Standalone | `kubectl apply -f k8s/` (includes Prometheus with kubernetes_sd scraping port 15021) |

The PodMonitor uses `namespaceSelector: any: true` to discover injected pods across all namespaces. It matches pods by **label** `sidecar.ananse.io/status: injected` (set automatically on injection) on the named port `ananse-admin`.

### Logs

Promtail ships pod logs to Loki with `namespace`, `pod`, and `container` labels.

```bash
# Deploy standalone observability stack
kubectl create ns monitoring
kubectl apply -f k8s/
```

### Distributed Tracing

The sidecar sends traces via OpenTelemetry OTLP gRPC (port 4317) to any compatible backend (Tempo, Jaeger).

Configure via Helm values:
```yaml
observability:
  tracing:
    enabled: "true"
    endpoint: "tempo.monitoring.svc.cluster.local:4317"
    # Optional: drop successful health check spans from Tempo.
    # Only errored health probes (5xx / transport failure) are exported.
    # Reduces Tempo noise from kubelet liveness/readiness probes.
    filterHealthChecks: false
```

Or set directly on the injector ConfigMap:
```bash
kubectl patch configmap ananse-injector-config -n ananse-system \
  --type merge -p '{"data":{"TRACING_ENABLED":"true","OTEL_ENDPOINT":"tempo.monitoring.svc:4317"}}'
```

**Health check trace filtering** (`filterHealthChecks: true`): kubelet probes (`/management/health`, `/healthz`, `/ready`) fire every few seconds per pod. By default these generate a trace each. Enabling this filter drops successful health probe spans at the sidecar before they reach Tempo — only spans where the probe returned 5xx are exported, which is exactly when you need the trace for debugging.

### Grafana Dashboards

The Ananse dashboard is auto-provisioned on startup — no manual import needed. Datasources provisioned automatically: **Prometheus**, **Loki** (with TraceID links to Tempo), **Tempo** (with log links to Loki).

**With ingress** (recommended for persistent access — add a host entry to your ingress pointing to `jhipster-grafana:3000`):
```
https://grafana.your-domain.io
```

**With port-forward** (standalone / no ingress):
```bash
kubectl port-forward svc/grafana 3000:3000 -n monitoring
# Open http://localhost:3000
```

**Login:** admin / jhipster (from `jhipster-grafana-credentials` secret)

---

### Load Testing

Before go-live, stress the mesh with the bundled k6 test suite (located in `kubernetes/load-test/` in your deployment repo):

```bash
# 1. Load the test script
kubectl create configmap k6-test-script \
  --from-file=test.js=kubernetes/load-test/k6-test.js \
  -n default --dry-run=client -o yaml | kubectl apply -f -

# 2. Smoke test (3 min, 5 VUs — sanity check)
# Edit K6_SCENARIO=smoke in k6-job.yaml
kubectl apply -f kubernetes/load-test/k6-job.yaml
kubectl logs -f job/k6-load-test -n default
kubectl delete job k6-load-test -n default

# 3. Ramp test (19 min, 0→500 VUs — find capacity ceiling)
# Edit K6_SCENARIO=ramp, apply again

# 4. Overnight soak test (7h, 50 VUs — confirm no goroutine/memory leaks)
# K6_SCENARIO=soak is the default
kubectl apply -f kubernetes/load-test/k6-job.yaml
```

The k6 pod runs outside the mesh (`sidecar.ananse.io/inject: "false"`) but every service it targets is injected — so you're exercising the real inbound proxy path on every request. Metrics are pushed directly into Prometheus so you can watch the Ananse dashboard live during the test.

---

## License

MIT License
