# Ananse Helm Chart

Helm chart for deploying Ananse service mesh on Kubernetes.

## Prerequisites

- Kubernetes 1.19+
- Helm 3.0+
- kubectl configured to access your cluster

## Installation

### Quick Start

```bash
# Add dependencies (if using observability stack)
helm dependency update ./ananse-chart

# Generate TLS certificates
./scripts/generate-certs.sh
cat ca.crt | base64 | tr -d '\n' > ca.crt.b64

# Install
helm install ananse ./ananse-chart \
  --set-file caBundle=./ca.crt.b64 \
  -n ananse-system --create-namespace
```

### Preview Templates

```bash
helm template ananse-chart
```

## Configuration

### Core Values

| Parameter | Description | Default |
|-----------|-------------|---------|
| `namespace` | Kubernetes namespace | `ananse-system` |
| `controlplane.image` | Control plane image | `anthony4m/ananse-controlplane:v1` |
| `sidecar.image` | Sidecar proxy image | `anthony4m/ananse-proxy:v1` |
| `init.image` | Init container image | `anthony4m/ananse-init:v1` |
| `debug_mode` | Enable debug mode | `"false"` |
| `failurePolicy` | Webhook failure policy | `"Fail"` |

### Discovery Mode

| Parameter | Description | Default |
|-----------|-------------|---------|
| `discovery.mode` | Service discovery: `k8s`, `consul`, or `file` | `k8s` |
| `discovery.consul.address` | Consul agent address | `localhost:8500` |
| `discovery.consul.tag` | Filter services by tag | `""` |

```bash
# Kubernetes discovery (default)
helm install ananse ./ananse-chart

# Consul discovery
helm install ananse ./ananse-chart \
  --set discovery.mode=consul \
  --set discovery.consul.address=consul.example.com:8500 \
  --set discovery.consul.tag=mesh
```

### Proxy Mode

| Parameter | Description | Default |
|-----------|-------------|---------|
| `proxy.mode` | Proxy mode: `sidecar` or `gateway` | `sidecar` |
| `metrics.enabled` | Enable metrics port | `true` |
| `metrics.port` | Metrics port | `9090` |

```bash
# Gateway mode
helm install ananse ./ananse-chart --set proxy.mode=gateway
```

### Observability Stack

Optional dependencies - disabled by default.

| Parameter | Description | Default |
|-----------|-------------|---------|
| `observability.prometheus.enabled` | Install kube-prometheus-stack | `false` |
| `observability.loki.enabled` | Install Loki | `false` |
| `observability.promtail.enabled` | Install Promtail | `false` |
| `observability.tempo.enabled` | Install Tempo | `false` |
| `observability.dashboard.enabled` | Auto-load Grafana dashboard | `true` |

```bash
# Update dependencies first
helm dependency update ./ananse-chart

# Install with full observability
helm install ananse ./ananse-chart \
  --set observability.prometheus.enabled=true \
  --set observability.loki.enabled=true \
  --set observability.promtail.enabled=true \
  --set observability.tempo.enabled=true \
  -n ananse-system --create-namespace
```

## Using Values Files

Create environment-specific values:

```yaml
# prod-values.yaml
namespace: ananse-prod
debug_mode: "false"

controlplane:
  image: anthony4m/ananse-controlplane:v2

observability:
  prometheus:
    enabled: true
  tempo:
    enabled: true
```

```bash
helm install ananse ./ananse-chart -f prod-values.yaml -n ananse-prod
```

## Uninstall

```bash
helm uninstall ananse -n ananse-system
kubectl delete namespace ananse-system
```

## Chart Structure

```
ananse-chart/
├── Chart.yaml              # Chart metadata and dependencies
├── values.yaml             # Default configuration
├── charts/                 # Downloaded dependencies
└── templates/
    ├── namespace.yaml
    ├── rbac.yaml
    ├── injector-config.yaml
    ├── secret.yaml
    ├── webhook-config.yaml
    ├── webhook-deployment.yaml
    └── webhook-service.yaml
```
