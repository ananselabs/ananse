## Last Session: 2026-04-09 (continued — trace filtering + v0.3.0)

### What Got Done
- Split log panel into "Application & Mesh Logs" (everything except health probes) and "Health Probe Logs" (only `/management/health` traffic)
- Added opt-in health check trace filtering: `filterHealthChecks: true` drops successful health probe spans from Tempo, only errored ones (5xx) are exported
- Released v0.3.0 — CI must be triggered from `ananselabs/ananse`, not `Anthony4m/ananse` (GHCR push permissions)
- Enabled `filterHealthChecks: true` in BlackKonnect, bumped all images to v0.3.0
- Updated README, docs, and gh-pages website

### Health Check Trace Filtering

**Why:** kubelet probes fire every 5-10s × 3 pods × 2 probes = ~24 traces/min of `/management/health` requests cluttering Tempo search.

**How (opt-in):**
```yaml
observability:
  tracing:
    filterHealthChecks: true   # false by default
```

Maps to env var `FILTER_HEALTH_CHECKS=true` on every injected sidecar.

**Implementation** (`pkg/proxy/tracer.go`):
- `healthFilterExporter` wraps the OTLP exporter
- At export time (after response is known), drops spans where `http.url` contains `/health` AND status is not `codes.Error`
- 5xx health probe spans pass through — exactly when you need them for debugging

**Tail-based decision** — the span is created and the response is received before the export filter runs, so the filter always knows the outcome. A head-based sampler can't do "only error health checks" because it decides before the request completes.

### Log Panel Split

**Service Mesh Logs** panel was split into two side-by-side panels:

| Panel | Query | Shows |
|---|---|---|
| Application & Mesh Logs | `{namespace="default"} \| drop __error__, __error_details__ != "/management/health"` | All logs except health probes |
| Health Probe Logs | `{namespace="default"} \|= "/management/health" \| drop __error__, __error_details__` | Kubelet probe traffic only |

`| drop __error__, __error_details__` removes the `JSONParserErr` Loki adds when it can't parse plain-text Spring Boot logs as JSON — no filtering of log lines, just removes the noise field.

### Release Process — Always Push to `ananselabs` Remote

The repo has two remotes:
- `origin` → `Anthony4m/ananse` — personal account, **no GHCR write access to `ananselabs` org**
- `ananselabs` → `ananselabs/ananse` — org repo, CI has GHCR write access

To release:
```bash
git push ananselabs main
git push ananselabs v0.3.0   # triggers CI on ananselabs/ananse
```

Never trigger release from `origin` tag push — it will fail with `permission_denied`.

---

## Previous Session: 2026-04-09 (continued — dashboard polish)

### What Got Done
- Fixed Grafana dashboard not auto-loading (missing provider ConfigMap and volume mounts)
- Fixed dashboard JSON format (was Grafana export format `{"dashboard":{...}}`, provisioner needs raw JSON)
- Disabled Alertmanager (crash loop — 600-day-old StatefulSet selector conflict with chart 67.9.0, no alert rules configured anyway)
- Fixed per-service metric breakdown: added `podTargetLabels: [app]` to PodMonitor so `by (app)` queries split by service
- Fixed log panels: changed from `{namespace="default"} | json` (pulled all containers + failed JSON parsing) to `{namespace="default", container="ananse-proxy"}` (sidecar logs only, no parser)
- Updated README Quick Start for new clients (published Helm repo, 5-step flow)
- Updated gh-pages landing page: badge v0.2.0→v0.2.9, added Observability section

### Grafana Dashboard Auto-Provisioning

**How it works:**
- Provider ConfigMap (`jhipster-grafana-dashboard-provider`) mounts at `/etc/grafana/provisioning/dashboards/`
- Dashboard ConfigMap (`jhipster-grafana-dashboard`) mounts at `/var/lib/grafana/dashboards/`
- Grafana polls for changes every 30s (`updateIntervalSeconds: 30`)
- JSON must be **raw dashboard format** — top-level keys: `annotations`, `panels`, `title`, etc.
- NOT `{"dashboard": {...}}` (that's the Grafana export-for-sharing format — provisioner silently fails with "Dashboard title cannot be empty")

**Gotcha — ArgoCD overrides live kubectl changes:**
Any `kubectl apply/replace/patch` to a resource managed by ArgoCD will be reverted on next sync. Always commit to git first, then force sync:
```bash
kubectl annotate app <app-name> -n argocd argocd.argoproj.io/refresh=hard --overwrite
```

### PodMonitor `podTargetLabels` — Per-Service Breakdown

Without `podTargetLabels`, scraped metrics only carry `direction`, `status`, `pod`, `namespace` — no `app` label. Adding it copies the pod's `app` label onto every metric.

**In `ananse-chart/templates/servicemonitor.yaml`:**
```yaml
podTargetLabels:
  - app
```

This enables `by (app)` grouping in dashboard queries, splitting metrics by service name (e.g. `blackkonnectcxpgateway`, `blackkonnectcxpcontacts`, `blackkonnectcxpengagement`).

Labels only attach to metrics scraped **after** the patch was applied — existing series don't backfill.

### Loki Query Pattern for Sidecar Logs

**Wrong (old):** `{namespace="default"} | json`
- Pulls all containers in namespace (Spring Boot app logs, system logs, everything)
- `| json` fails on Spring Boot plain-text logs → `__error__=JSONParserErr` on every line

**Correct:**
```
{namespace="default", container="ananse-proxy"}                          # all sidecar logs
{namespace="default", container="ananse-proxy"} |= "level=error"        # errors only
{namespace="default", container="ananse-proxy"} |~ "4[0-9]{2}|5[0-9]{2}" # 4xx/5xx
```

Sidecar proxy logs are JSON — labels like `namespace`, `pod`, `container` come from Promtail's CRI pipeline stage (the `{...}` selectors), not from parsing the log line body.

### Alertmanager Disabled

**Why it was crashing:** Prometheus Operator tried to update an immutable `spec.selector` field on a StatefulSet created 600 days ago with different chart defaults → infinite delete+recreate loop. CRDs were also OutOfSync (schema missing `.status.selector`).

**Why we don't need it:** Alertmanager routes Prometheus alert rules to notification channels (Slack, email, PagerDuty). With no alert rules configured, it does nothing.

**Fix:** `alertmanager.enabled: false` in `kubernetes/monitoring-k8s/prometheus-operator-values.yaml`.

To add alerting later: configure alert rules in a PrometheusRule CR, then re-enable alertmanager and add a receiver (Slack webhook URL etc.).

### Actual Proxy Metrics (what the sidecar emits)

These are the metrics the sidecar actually exposes at `:15021/metrics`. Use these in dashboards — the old names (`ananse_backend_health_status`, `ananse_circuit_breaker_state`) don't exist:

| Metric | Type | Description |
|--------|------|-------------|
| `ananse_sidecar_connections_active` | Gauge | Active connections right now |
| `ananse_sidecar_connections_total` | Counter | Total connections since start |
| `ananse_sidecar_request_duration_seconds` | Histogram | Latency histogram |
| `ananse_http_requests_in_flight` | Gauge | In-flight HTTP requests |
| `ananse_circuit_breaker_failures_total` | Counter | Circuit breaker trip count |
| `ananse_sidecar_connections_by_tls_total` | Counter | Connections by TLS mode |

### End-of-Session BlackKonnect Observability State (2026-04-09 final)
- **Metrics:** 4 Ananse sidecar targets UP, per-service breakdown working via `by (app)`
- **Logs:** Loki showing ananse-proxy container logs only (no Spring Boot noise)
- **Traces:** Tempo has traces from blackkonnectcxpgateway and blackkonnectcxpengagement
- **Dashboard:** Auto-provisioned, "Ananse Service Mesh" dashboard loading on Grafana start
- **Alertmanager:** Disabled (no alert rules, was crash-looping)

---

## Previous Session: 2026-04-09 (observability bring-up)

### What Got Done
- Fixed inbound proxy `proxyBidirectional` leak causing liveness probe timeouts (v0.2.8)
- Fixed app container probe timeout: patched `timeoutSeconds` from 1s → 5s on contacts and engagement deployments
- All BlackKonnect app pods now `2/2 Running` with 0 restarts (except pre-existing engagement restarts)
- Fixed Prometheus scraping architecture (ServiceMonitor → PodMonitor targeting sidecar admin port)
- Got full observability stack working on BlackKonnect: metrics UP, logs in Loki, traces in Tempo
- Released v0.2.9 (CI passed, chart published to gh-pages Helm repo)
- Captured all live patches into BlackKonnect manifests (reproducible on prod in ~5 min)

### Prometheus Scraping Architecture

**Controlplane has no metrics server.** It only runs `:8443` (webhook) and `:50051` (gRPC). The old ServiceMonitor targeting the controlplane was wrong.

**Metrics come from sidecar proxies** at `:15021/metrics` (admin port) on every injected pod.

**Two paths to scraping:**

| Environment | Approach | What to deploy |
|---|---|---|
| Has Prometheus Operator (BlackKonnect) | PodMonitor in Helm chart | `serviceMonitor.enabled: true` in values |
| Standalone (no operator) | Plain Prometheus with k8s SD | `kubectl apply -f k8s/prometheus.yaml ...` |

**PodMonitor selector** (in `ananse-chart/templates/servicemonitor.yaml`):
- Matches pods with **label** `sidecar.ananse.io/status: injected`
- Scrapes port named `ananse-admin` (15021)
- `namespaceSelector: any: true` — finds injected pods in any namespace

**Injector changes** (`controlplane/injector/injector.go`):
- Added `{Name: "ananse-admin", ContainerPort: 15021}` to sidecar container ports (required for PodMonitor port-by-name)
- Now sets `sidecar.ananse.io/status: injected` as both annotation AND label (PodMonitor selector only matches labels, not annotations)

**BlackKonnect-specific cluster patches** (now in manifests, no longer live-only):
- `jhipster-prometheus-cr.yml`: added `podMonitorSelector: {}`, `podMonitorNamespaceSelector: {}`, `serviceMonitorNamespaceSelector: {}`
- `kubernetes/ananse/ananse-prometheus-rbac.yaml`: RoleBinding giving `jhipster-prometheus-sa` view access in `ananse-system`

**Standalone k8s/ stack** (`k8s/` directory):
- `k8s/prometheus.yaml` — Prometheus with `kubernetes_sd_configs` scraping `:15021/metrics` from pods with annotation `sidecar.ananse.io/status: injected`
- `k8s/grafana.yaml` — Grafana with Prometheus + Loki + Tempo datasources (Loki added this session)
- `k8s/loki.yaml`, `k8s/promtail.yaml`, `k8s/tempo.yaml` — log and trace backends
- Deploy with: `kubectl create ns monitoring && kubectl apply -f k8s/`

### BlackKonnect Manifests Now Fully Reproducible

All live patches captured in `BlackKonnectCxpDeployments`:
- `kubernetes/monitoring-k8s/jhipster-prometheus-cr.yml` — Prometheus CR with cross-namespace PodMonitor watching
- `kubernetes/ananse/ananse-prometheus-rbac.yaml` — RBAC for cross-namespace scraping (new file)
- `kubernetes/blackkonnectcxpcontacts-k8s/blackkonnectcxpcontacts-deployment.yml` — `timeoutSeconds: 5` on both probes
- `kubernetes/blackkonnectcxpengagement-k8s/blackkonnectcxpengagement-deployment.yml` — `timeoutSeconds: 5` on both probes
- `kubernetes/ananse/THURSDAY_RUNBOOK.md` — updated runbook with all steps including RBAC + Prometheus CR apply

### v0.2.9 Observability Fixes

**Released:** v0.2.9 — chart published to `ananselabs/ananse` gh-pages Helm repo.

**Key fixes shipped in this version:**

1. **PodMonitor instead of ServiceMonitor** (`ananse-chart/templates/servicemonitor.yaml`)
   - Controlplane has no metrics server. Old ServiceMonitor targeted controlplane port 9090 (wrong).
   - New PodMonitor targets every pod labeled `sidecar.ananse.io/status: injected` on port `ananse-admin` (15021).
   - `namespaceSelector: any: true` — finds injected pods across all namespaces.

2. **Injector now sets injection status as label** (`controlplane/injector/injector.go`)
   - PodMonitor selector only matches labels, not annotations.
   - Injector now sets `sidecar.ananse.io/status: injected` as both label AND annotation.
   - Also added `ananse-admin` ContainerPort 15021 to sidecar (required for PodMonitor port-by-name reference).

3. **Tracing endpoint made configurable** (`ananse-chart/templates/injector-config.yaml`)
   - Added `TRACING_ENABLED` and `OTEL_ENDPOINT` to injector ConfigMap.
   - BlackKonnect values: `tempo.monitoring.svc.cluster.local:4317`.
   - Removed stale `METRICS_PORT` (not used by injector).

4. **Stale metrics port removed from webhook Service** (`ananse-chart/templates/webhook-service.yaml`)
   - Removed port 9090 that pointed at controlplane (no metrics there).

5. **values.yaml restructured**
   - Removed top-level `metrics:` block.
   - Added `observability.tracing.enabled` and `observability.tracing.endpoint`.

### Observability Architecture (v0.2.9+)

**Metrics flow:**
```
sidecar :15021/metrics → PodMonitor (namespaceSelector: any) → jhipster-prometheus → Grafana
```

**Logs flow:**
```
/var/log/pods/*/*.log → promtail (static glob) → loki → Grafana
```

**Traces flow:**
```
sidecar → OTEL gRPC :4317 → Tempo → Grafana
```

**Two deployment paths:**

| Environment | Prometheus approach | What to apply |
|---|---|---|
| Has Prometheus Operator (BlackKonnect) | PodMonitor in Helm chart | `serviceMonitor.enabled: true` |
| Standalone | Plain Prometheus with k8s SD | `kubectl apply -f k8s/` |

### Promtail Fix (static glob vs kubernetes_sd)

`kubernetes_sd_configs` with path relabeling produced 0 active file targets — the replacement pattern `__path__: /var/log/pods/*$1/*.log` with uid/container relabeling matched nothing.

**Working config** (`kubernetes/observability/promtail.yaml`):
```yaml
scrape_configs:
  - job_name: pods
    static_configs:
      - targets: [localhost]
        labels:
          job: pods
          __path__: /var/log/pods/*/*/*.log
    pipeline_stages:
      - cri: {}
      - regex:
          expression: '^/var/log/pods/(?P<namespace>[^_]+)_(?P<pod>[^_]+)_[^/]+/(?P<container>[^/]+)/.*$'
          source: filename
      - labels:
          namespace:
          pod:
          container:
```

**Note:** DaemonSets don't auto-reload on ConfigMap changes. Must `kubectl rollout restart daemonset/promtail -n monitoring` after config change.

### BlackKonnect ClusterRole Requirement

With `namespaceSelector: any: true` in PodMonitor, Prometheus tries to list pods at **cluster scope**. A namespace-scoped Role+RoleBinding in `default` is not enough — Prometheus gets 403 "pods is forbidden at cluster scope".

**Fix:** Upgrade `jhipster-prometheus-sa` to ClusterRole + ClusterRoleBinding (in `jhipster-prometheus-cr.yml`).

### kube-prometheus-stack Inject Opt-out

kube-prometheus-stack components (grafana, operator, kube-state-metrics, alertmanager, prometheus) must NOT get Ananse sidecars. Applied via:
- Live: `kubectl patch` adding `sidecar.ananse.io/inject: "false"` annotation to each StatefulSet/Deployment
- Persisted in: `kubernetes/monitoring-k8s/prometheus-operator-values.yaml` with `podAnnotations` for each component, deployed via ArgoCD app `kube-prometheus-stack.yaml`

### ArgoCD Hard Refresh Pattern

After pushing a new chart version, ArgoCD may wait up to 3 min for cache expiry:
```bash
kubectl annotate app -n argocd ananse argocd.argoproj.io/refresh=hard --overwrite
kubectl get app -n argocd ananse  # wait for Synced/Healthy
kubectl rollout restart deployment/blackkonnectcxpgateway \
  deployment/blackkonnectcxpcontacts \
  deployment/blackkonnectcxpengagement -n default
```

### Grafana Access From Your PC

jhipster-grafana runs as NodePort 31626 on slave node `157.245.35.3`.

**Option 1: Direct NodePort** (if slave node is reachable):
```
http://157.245.35.3:31626
```

**Option 2: Port-forward** (via SSH tunnel):
```bash
# SSH tunnel must be open first
ssh -i ~/.ssh/master_key -L 6443:localhost:6443 root@178.62.79.147 -N -f

# Port-forward Grafana
KUBECONFIG=~/.kube/blackkonnect.yaml kubectl port-forward -n default svc/jhipster-grafana 3000:3000
# Open http://localhost:3000
```

**Login:** admin / jhipster (from `jhipster-grafana-credentials` secret)

**Datasources auto-provisioned:**
- Prometheus → `http://jhipster-prometheus.default.svc:9090`
- Loki → `http://loki.monitoring.svc.cluster.local:3100` (with TraceID derived field → links to Tempo)
- Tempo → `http://tempo.monitoring.svc.cluster.local:3200` (with tracesToLogs → links to Loki)

**To view logs:** Explore → Loki → label filters `namespace=default`
**To view traces:** Explore → Tempo → Search (select service, operation)
**To view metrics:** Explore → Prometheus → metric `ananse_requests_total` or import Ananse dashboard

### Grafana Dashboard Fixes (2026-04-09 continued)

**Problem 1 — Logs panels showed "No data":**
- Dashboard queries used `{namespace="ananse"}` but injected pods are in `default` namespace
- Fixed all 3 Loki panels: `namespace="ananse"` → `namespace="default"`

**Problem 2 — Health/Circuit panels showed "No data":**
- `ananse_backend_health_status` and `ananse_circuit_breaker_state` are not emitted by the proxy
- Proxy actually emits: `ananse_sidecar_connections_active`, `ananse_sidecar_connections_total`, `ananse_sidecar_request_duration_seconds_*`, `ananse_http_requests_in_flight`
- Fixed: "Healthy Backends" → "Active Sidecars" (`count(ananse_sidecar_connections_active)`)
- Fixed: "Backend Health Status" → "Active Connections by Pod" (`ananse_sidecar_connections_active`)
- Fixed: "Circuit Breaker States" → "In-Flight Requests by Pod" (`ananse_http_requests_in_flight`)

**Problem 3 — Dashboard ConfigMap format:**
- Old `jhipster-grafana-dashboard` ConfigMap had Grafana export format (`{"dashboard":{...}}`) — file provisioner needs raw JSON
- Also was missing the dashboard provider ConfigMap and volume mounts in the Grafana deployment
- Fixed by: adding `jhipster-grafana-dashboard-provider` ConfigMap, mounting it + dashboard at `/etc/grafana/provisioning/dashboards` and `/var/lib/grafana/dashboards`
- ArgoCD was overriding live changes — had to commit fixes to `BlackKonnectCxpDeployments` git repo

**Gotcha — ArgoCD overrides live kubectl changes:**
Any `kubectl apply/replace` to a resource managed by ArgoCD will be reverted on next sync. Always commit to git first, then let ArgoCD sync (or force with `kubectl annotate app <name> -n argocd argocd.argoproj.io/refresh=hard --overwrite`).

### End-of-Session BlackKonnect Observability State (2026-04-09)
- **Metrics:** 4 Ananse sidecar targets UP on jhipster-prometheus
- **Logs:** Loki receiving log streams from `default` namespace with labels: namespace/pod/container/filename/job/stream
- **Traces:** Tempo has 5 traces from blackkonnectcxpgateway and blackkonnectcxpengagement

---

### v0.2.8 Bug Fix: Inbound proxyBidirectional Leak

**File:** `pkg/proxy/listener.go`

**Root cause:** After every inbound HTTP request-response cycle, the handler entered `proxyBidirectional` — including for kubelet health probe connections. Kubelet uses `DisableKeepAlives: true` (sends `Connection: close`), so after receiving the response it closes the TCP connection. The proxy's `src → dst` goroutine got EOF and finished, but `dst → src` was stuck waiting for the Spring Boot app to send data on its keep-alive connection. The app waits for a request, the proxy waits for app data — deadlock on that connection for ~75 seconds (Tomcat's keep-alive timeout). After 20+ minutes of liveness probes at 10s period, hundreds of zombie goroutines accumulated, exhausting Spring Boot's Tomcat thread pool and causing intermittent `context deadline exceeded`.

**Fix in `handleInboundConnection` (listener.go ~line 1047):**
```go
// Skip bidirectional proxy when connection should close:
// - client sent Connection: close (req.Close = true)
// - server responded with Connection: close (resp.Close = true)
// - request is a health/probe endpoint (short-lived, no streaming)
if req.Close || resp.Close || strings.Contains(req.URL.Path, "/health") {
    return
}
```

Also fixed: `conn` field was nil on `rw` and `targetRW` in inbound handler, making `CloseWrite()` a no-op. Now properly set like the outbound handler does.

**Outbound handler already had this pattern** (line ~718):
```go
if req.URL.Path == "/health" || req.URL.Path == "/healthz" || req.URL.Path == "/ready" {
    return
}
```
But inbound was missing it entirely.

### Current BlackKonnect Pod State (2026-04-09 end of session)
| Pod | Status | Notes |
|-----|--------|-------|
| blackkonnectcxpadmin | 2/2 Running | |
| blackkonnectcxpcontacts | 2/2 Running, 0 restarts | |
| blackkonnectcxpengagement | 2/2 Running | |
| blackkonnectcxpgateway | 1/1 Running | Gateway mode, no sidecar |

### Probe Timeout Patch (applied directly to deployments)
Contacts and engagement had `timeoutSeconds: 1` on the readiness probe. Patched to `5s` directly on the deployment objects (not in the app's Helm chart). This is not persisted — if the deployments are recreated from their Helm chart, they'll revert to 1s.

---

## Previous Session: 2026-04-09

### BlackKonnect Cluster Access

**Master node:** `178.62.79.147`
**SSH key:** `~/.ssh/master_key`
**Kubeconfig:** `~/.kube/blackkonnect.yaml`

**Connect (run once per session):**
```bash
# 1. Open tunnel (background)
ssh -i ~/.ssh/master_key -L 6443:localhost:6443 root@178.62.79.147 -N -f -o StrictHostKeyChecking=no

# 2. Grab kubeconfig (first time or if creds rotated)
ssh -i ~/.ssh/master_key -o StrictHostKeyChecking=no root@178.62.79.147 'cat /etc/kubernetes/admin.conf' \
  | sed 's|https://[^:]*:6443|https://localhost:6443|g' \
  > ~/.kube/blackkonnect.yaml
awk '/certificate-authority-data:/ { print "    insecure-skip-tls-verify: true"; next } { print }' \
  ~/.kube/blackkonnect.yaml > ~/.kube/blackkonnect.yaml.tmp && mv ~/.kube/blackkonnect.yaml.tmp ~/.kube/blackkonnect.yaml
```

**Use:** `KUBECONFIG=~/.kube/blackkonnect.yaml kubectl ...`

---

## Previous Session: 2026-04-07

### What Got Done
- Added Consul service discovery support for VM/Docker deployments
- Created Helm chart for Ananse installation on K8s
- Added optional observability stack (Prometheus, Loki, Promtail, Tempo)
- Auto-loading Grafana dashboard when Prometheus enabled
- ServiceMonitor for Prometheus auto-scraping

### Consul Integration

**New file:** `controlplane/consul-client.go` - implements `ConfigWatcher` interface
**Updated:** `controlplane/cmd/main.go` - added `-consul`, `-consul-addr`, `-consul-tag` flags

**Usage:**
```bash
# Run control plane with Consul discovery
./controlplane -consul -consul-addr "consul-host:8500"

# With tag filtering (only services tagged "ananse")
./controlplane -consul -consul-addr "consul-host:8500" -consul-tag "ananse"
```

**How it works:**
- Uses Consul blocking queries for real-time catalog updates
- Same debounce pattern as K8sClient (500ms window)
- Discovers healthy service instances from Consul health API
- Builds same `pb.Config` structure as other watchers
- Routes created at `/{service-name}` for each discovered service

**Use case:** Client with Consul gateway running in Docker on VMs (not K8s)

### Client Deployments

| Client | Infra | Ananse Mode | Discovery Flag |
|--------|-------|-------------|----------------|
| BlackKonnect | K8s (DigitalOcean) | Sidecar | `-k8s` |
| Other client | VM + Docker + Consul | Gateway | `-consul` |

### Helm Chart (Complete)

**Structure:**
```
ananse-chart/
├── Chart.yaml              # Dependencies defined here
├── values.yaml
├── charts/                 # Downloaded dependencies (gitignored)
├── dashboards/
│   └── ananse-dashboard.json
└── templates/
    ├── namespace.yaml
    ├── rbac.yaml
    ├── injector-config.yaml
    ├── secret.yaml
    ├── webhook-config.yaml
    ├── webhook-deployment.yaml
    ├── webhook-service.yaml
    ├── grafana-dashboard.yaml  # Auto-loads dashboard into Grafana
    └── servicemonitor.yaml     # Prometheus auto-scraping
```

**values.yaml:**
```yaml
name: "ananse"
namespace: "ananse-system"
debug_mode: "false"
failurePolicy: "Fail"

controlplane:
  image: "anthony4m/ananse-controlplane:v1"

sidecar:
  image: "anthony4m/ananse-proxy:v1"

init:
  image: "anthony4m/ananse-init:v1"

discovery:
  mode: "k8s"  # k8s | consul | file
  consul:
    address: "localhost:8500"
    tag: ""

metrics:
  enabled: true
  port: 9090

proxy:
  mode: "sidecar"  # sidecar | gateway

# Observability - disabled by default
observability:
  prometheus:
    enabled: false
  loki:
    enabled: false
  promtail:
    enabled: false
  tempo:
    enabled: false
  dashboard:
    enabled: true  # Auto-load Grafana dashboard
```

**Basic Usage:**
```bash
# Preview rendered manifests
helm template ananse-chart

# Install Ananse only
helm install ananse ./ananse-chart \
  --set-file caBundle=./ca.crt.b64 \
  -n ananse-system --create-namespace

# Override discovery mode
helm template ananse-chart --set discovery.mode=consul \
  --set discovery.consul.address=consul.example.com:8500

# Change proxy mode
helm template ananse-chart --set proxy.mode=gateway
```

**With Observability Stack:**
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

### Next Actions
- Test helm install on K8s cluster
- Add caBundle generation to install workflow

---

## Previous Session: 2026-02-26

### What Got Done
- Set up breakpoint debugging for sidecar proxy in Kubernetes with GoLand
- Fixed SO_ORIGINAL_DST implementation (use raw syscall, not GetsockoptString)
- Fixed inbound handler to use dynamic port from SO_ORIGINAL_DST instead of hardcoded port 80
- Added DEBUG_MODE to injector for relaxed security context during debugging
- Fixed Delve Dockerfile flags (`--accept-multiclient`, `--continue`)

### Kubernetes Debugging with Delve/GoLand

**Files created/modified:**
- `proxy/Dockerfile.proxy.debug` - Debug image with Delve
- `controlplane/injector/injector.go` - DEBUG_MODE support
- `pkg/proxy/originaldst.go` - Fixed SO_ORIGINAL_DST syscall
- `pkg/proxy/listener.go` - Dynamic port extraction for inbound

**Debug Dockerfile (`proxy/Dockerfile.proxy.debug`):**
```dockerfile
FROM golang:alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
RUN go install github.com/go-delve/delve/cmd/dlv@latest
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -gcflags="all=-N -l" -o /server ./proxy/main.go

FROM alpine:latest
RUN apk add --no-cache libc6-compat
WORKDIR /root/
COPY --from=builder /go/bin/dlv /usr/local/bin/dlv
COPY --from=builder /server /server
RUN chmod +x /server
EXPOSE 2345
CMD ["/usr/local/bin/dlv", "exec", "/server", "--headless", "--listen=:2345", "--api-version=2", "--accept-multiclient", "--continue"]
```

**Key flags:**
- `-gcflags="all=-N -l"` - Disable optimizations for debugging
- `--accept-multiclient` - Allow reconnecting debugger
- `--continue` - Auto-start app (don't wait for debugger)

**Injector DEBUG_MODE (`controlplane/injector/injector.go`):**
```go
DebugMode = getEnv("DEBUG_MODE", "false") == "true"

// In security context:
if DebugMode {
    // Relaxed for Delve: root, no readonly fs, seccomp unconfined
    return &corev1.SecurityContext{
        RunAsUser:                ptr(int64(0)),
        RunAsNonRoot:             ptr(false),
        AllowPrivilegeEscalation: ptr(true),
        ReadOnlyRootFilesystem:   ptr(false),
        SeccompProfile: &corev1.SeccompProfile{
            Type: corev1.SeccompProfileTypeUnconfined,
        },
    }
}
// Production: locked down
```

**ConfigMap for debug mode:**
```bash
kubectl patch configmap ananse-injector-config -n ananse-system \
  --type merge -p '{"data":{"DEBUG_MODE":"true","SIDECAR_IMAGE":"anthony4m/ananse-proxy:debug-v1"}}'
```

**Debugging workflow:**
```bash
# 1. Scale to 1 replica (avoid load balancing confusion)
kubectl scale deployment analytics -n ananse --replicas=1

# 2. Port-forward debugger port
kubectl port-forward -n ananse <pod-name> 2345:2345

# 3. Connect GoLand: Run → Debug → Go Remote → localhost:2345

# 4. Set breakpoints in pkg/proxy/listener.go

# 5. Send traffic FROM INSIDE CLUSTER (port-forward bypasses iptables!)
kubectl exec -n ananse deployment/auth -- wget -qO- http://analytics:5004/health
```

**Why port-forward doesn't hit breakpoints:**
- `kubectl port-forward` connects directly to container port, bypassing pod network namespace
- iptables rules only intercept traffic entering through pod's network namespace
- Health probes from kubelet go through iptables → trigger breakpoints
- Traffic from other pods goes through iptables → trigger breakpoints

**SO_ORIGINAL_DST fix (`pkg/proxy/originaldst.go`):**
```go
// Use raw syscall, not GetsockoptString (which returns truncated data)
err = rawConn.Control(func(fd uintptr) {
    var raw [16]byte
    rawLen := uint32(len(raw))
    _, _, errno := unix.Syscall6(
        unix.SYS_GETSOCKOPT,
        fd,
        uintptr(SOL_IP),
        uintptr(SO_ORIGINAL_DST),
        uintptr(unsafe.Pointer(&raw[0])),
        uintptr(unsafe.Pointer(&rawLen)),
        0,
    )
    // Parse: family (LE), port (BE), IP (BE)
    family := binary.LittleEndian.Uint16(raw[0:2])
    port := binary.BigEndian.Uint16(raw[2:4])
    ip := net.IPv4(raw[4], raw[5], raw[6], raw[7])
    destAddr = fmt.Sprintf("%s:%d", ip.String(), port)
})
```

**Inbound handler fix (`pkg/proxy/listener.go`):**
```go
// Get original destination dynamically, don't hardcode port
origDst, err := getOriginalDst(clientConn)
_, port, _ := net.SplitHostPort(origDst)
target := net.JoinHostPort("127.0.0.1", port)
```

---

## Previous Session: 2026-01-30 (Research Week - Exams)

### What Got Done
- Designed mutating webhook architecture for sidecar mesh injection
- Clarified mental model: MutatingAdmissionWebhook intercepts pod creation, returns JSON patch
- Researched SO_ORIGINAL_DST socket option for transparent proxying
- Decided traffic interception strategy: Phase 1 REDIRECT, Phase 2 TPROXY
- Wrote complete iptables script for init container
- Researched JSON Patch edge cases (array append vs create, map handling, path escaping)
- Studied Istio sidecar injector patterns (injection hierarchy, jsonpatch library)
- Studied Linkerd proxy-injector patterns (Injectable() with reason strings)
- Researched AdmissionReview v1 vs v1beta1 (use v1 only, explicit fields required)
- Researched certificate management (self-signed MVP, cert-manager for production, chicken-egg deadlock)
- Researched security contexts (NET_ADMIN capability, UID 1337, Pod Security Standards)
- Researched failure modes (iptables trap, sidecar crash = connection refused, health checks)
- Researched resource limits (CPU throttle vs memory OOMKill, tuning strategy)
- Studied Envoy original destination (listener filter + cluster, x-envoy-original-dst-host header)
- Researched Go syscall patterns (RawConn.Control vs File(), golang.org/x/sys/unix)
- Defined project structure (10 files to create, prioritized)

### SO_ORIGINAL_DST Research Notes
**Why critical:** Core of transparent proxying - without this, sidecar can't forward to correct destination after iptables REDIRECT.

**Mechanism:**
- iptables REDIRECT performs DNAT → kernel's conntrack stores original dest mapping
- Proxy accepts on :15001 → destination appears as localhost:15001
- `getsockopt()` with SO_ORIGINAL_DST queries conntrack → retrieves pre-NAT destination

**Go Implementation:**
```go  
syscall.GetsockoptIPv6Mreq(fd, syscall.SOL_IP, syscall.SO_ORIGINAL_DST)  
```  
- IPv6Mreq is a 16-byte hack (Go lacks direct SO_ORIGINAL_DST helper)
- Getting fd: `net.Conn` → `SyscallConn()` → `Control(func(fd uintptr))`

**Returned sockaddr_in struct (16 bytes):**
- Bytes 0-1: Address Family (0x02 0x00 = AF_INET, little endian)
- Bytes 2-3: Original Port (big endian)
- Bytes 4-7: Original IP (big endian)
- Bytes 8-15: Padding (zeros)

**Reference Implementations Studied:**
- [x] ~~Istio's `original_dst.go`~~ - Istio's ztunnel moved to Rust, Go impl abandoned
- [x] Istio sidecar injector (webhook.go, inject.go)
- [x] Linkerd proxy-injector (report.go, webhook.go)
- [ ] Envoy's original destination cluster docs
- [ ] Go syscall package documentation

**Go Implementation - Two Patterns:**

**Pattern 1: Old way (IPv6Mreq Hack) - works but clunky:**
```go  
const SO_ORIGINAL_DST = 80  // Linux-specific  
  
// 1. Get fd (puts socket in blocking mode!)  
clientConnFile, _ := clientConn.File()  
clientConn.Close()  
  
// 2. Syscall - IPv6Mreq is 16-byte container matching sockaddr_in  
addr, _ := syscall.GetsockoptIPv6Mreq(  
    int(clientConnFile.Fd()),    syscall.IPPROTO_IP,    SO_ORIGINAL_DST,)  
  
// 3. Recreate conn in non-blocking mode  
newConn, _ := net.FileConn(clientConnFile)  
newTCPConn := newConn.(*net.TCPConn)  
clientConnFile.Close()  
  
// 4. Parse sockaddr_in from Multiaddr field  
// Multiaddr[2:4] = port (big endian)  
// Multiaddr[4:8] = IP (big endian)  
```  

**Pattern 2: Modern way (RawConn.Control) - RECOMMENDED:**
```go  
import (  
    "net"    "golang.org/x/sys/unix")  
  
const SO_ORIGINAL_DST = 80  
  
func getOriginalDst(conn net.Conn) (string, error) {  
    tcpConn := conn.(*net.TCPConn)  
    // Get raw connection WITHOUT changing socket mode    rawConn, err := tcpConn.SyscallConn()    if err != nil {        return "", err    }  
    var destAddr string    var operr error  
    // Control() executes func with fd, stays non-blocking    err = rawConn.Control(func(fd uintptr) {        // Read SO_ORIGINAL_DST        sa, err := unix.GetsockoptInet4Addr(int(fd), unix.SOL_IP, SO_ORIGINAL_DST)        if err != nil {            operr = err            return        }        // sa is [4]byte for IPv4        // Need raw sockaddr for port - use Getsockopt directly        var raw [16]byte        _, operr = unix.Getsockopt(int(fd), unix.SOL_IP, SO_ORIGINAL_DST, raw[:])        if operr != nil {            return        }        // Parse: bytes 2-3 = port (big endian), bytes 4-7 = IP        port := (uint16(raw[2]) << 8) + uint16(raw[3])        ip := net.IPv4(raw[4], raw[5], raw[6], raw[7])        destAddr = fmt.Sprintf("%s:%d", ip.String(), port)    })  
    if err != nil {        return "", err    }    return destAddr, operr}  
```  

**Pattern comparison:**

| Aspect | Pattern 1 (File()) | Pattern 2 (Control()) |  
|--------|-------------------|----------------------|  
| Blocking mode | Changes to blocking | Stays non-blocking |  
| Cleanup | Must recreate conn | No cleanup needed |  
| Package | `syscall` (deprecated) | `golang.org/x/sys/unix` |  

**Use Pattern 2 for Ananse** - cleaner, modern, no socket mode issues.

**Alternative Approach:** TPROXY (go-tproxy library) avoids conntrack overhead, uses `IP_TRANSPARENT` socket option instead of NAT

**Sources:**
- https://gist.github.com/fangdingjun/11e5d63abe9284dc0255a574a76bbcb1
- https://github.com/KatelynHaworth/go-tproxy
- https://github.com/cybozu-go/transocks/blob/master/DESIGN.md

### Design Decisions Made
- **Exclusions:** Skip `kube-system` and `kube-public` namespaces
- **Init container:** Required for iptables setup (must complete before app starts)
- **Sidecar purpose:** mTLS, observability, traffic control
- **Traffic model:** Apps talk to localhost, sidecar handles external traffic
- **Outbound routing:** Pass-through model using SO_ORIGINAL_DST to get original destination
- **Existing proxy:** Can run as sidecar (pass-through, no routing table needed)

**Traffic Interception Strategy (Decided 2026-01-29):**
- **Phase 1 (MVP):** REDIRECT + SO_ORIGINAL_DST
    - Easier to debug (`iptables -t nat -L` is readable)
    - Works with all K8s CNIs (Calico, Flannel, Cilium)
    - IPv6Mreq hack is ugly but battle-tested (Istio/Linkerd production-proven)
- **Phase 2 (Scale):** Migrate to TPROXY when needed
    - Trigger: 10k+ concurrent connections or conntrack table exhaustion
    - Benefit: No conntrack overhead, preserves real client IP for backend logs
    - Cleaner Go code (LocalAddr() gives original dest directly)

### Sidecar Port Configuration
- **15001** - Outbound listener (captures app→external traffic)
- **15006** - Inbound listener (captures external→app traffic)

### Init Container iptables Script
```bash  
#!/bin/sh  
set -e  
  
PROXY_UID=1337  
PROXY_INBOUND_PORT=15006  
PROXY_OUTBOUND_PORT=15001  
APP_HEALTH_PORT=8080  
KUBE_API_IP="${KUBERNETES_SERVICE_HOST:-10.96.0.1}"  
  
# Create chains  
iptables -t nat -N ANANSE_INBOUND  
iptables -t nat -N ANANSE_OUTBOUND  
  
# === INBOUND (PREROUTING - external traffic entering pod) ===  
# Exclusions first (order matters: checked top-to-bottom)  
iptables -t nat -A ANANSE_INBOUND -p tcp --dport $APP_HEALTH_PORT -j RETURN      # K8s probes  
iptables -t nat -A ANANSE_INBOUND -p tcp --dport $PROXY_INBOUND_PORT -j RETURN   # Don't redirect proxy port  
# Redirect everything else to inbound listener  
iptables -t nat -A ANANSE_INBOUND -p tcp -j REDIRECT --to-ports $PROXY_INBOUND_PORT  
# Activate  
iptables -t nat -A PREROUTING -p tcp -j ANANSE_INBOUND  
  
# === OUTBOUND (OUTPUT - app-generated traffic leaving pod) ===  
# Loop prevention (CRITICAL - proxy's own traffic must pass through)  
iptables -t nat -A ANANSE_OUTBOUND -m owner --uid-owner $PROXY_UID -j RETURN  
# Localhost bypass (app talking to itself)  
iptables -t nat -A ANANSE_OUTBOUND -d 127.0.0.1/32 -j RETURN  
# K8s API bypass (MVP safety)  
iptables -t nat -A ANANSE_OUTBOUND -d $KUBE_API_IP -j RETURN  
# Redirect everything else to outbound listener  
iptables -t nat -A ANANSE_OUTBOUND -p tcp -j REDIRECT --to-ports $PROXY_OUTBOUND_PORT  
# Activate  
iptables -t nat -A OUTPUT -p tcp -j ANANSE_OUTBOUND  
  
# Debug output  
iptables -t nat -L -v  
echo "Ananse iptables rules applied."  
```  

**Key Design Notes:**
- `-A` appends rules (order = top-to-bottom evaluation)
- Exclusions MUST come before catch-all REDIRECT
- DNS (port 53) unaffected - script only targets TCP
- UID 1337 is convention (Istio uses same) - sidecar must run as this user

### Components to Build
1. `controlplane/` - Add `/mutate` endpoint to existing controlplane server
2. `deploy/webhook-deployment.yaml` - K8s deployment for the webhook
3. `deploy/webhook-config.yaml` - MutatingWebhookConfiguration resource
4. Init container image - Sets up iptables rules (needs NET_ADMIN capability)
5. TLS certificates for webhook communication

### Architecture Notes
- JSON patch adds two containers: init container + sidecar container
- Init container runs iptables commands to redirect traffic to sidecar
- Must check if `initContainers` spec already exists before patching

### JSON Patch Edge Cases (RFC 6902)

**Array Handling (initContainers, containers):**
```go  
// If array EXISTS → append with /-  
if len(pod.Spec.InitContainers) > 0 {  
    patch = `{"op": "add", "path": "/spec/initContainers/-", "value": {...}}`  // Object}  
// If array MISSING → create with full array  
if len(pod.Spec.InitContainers) == 0 {  
    patch = `{"op": "add", "path": "/spec/initContainers", "value": [{...}]}`  // Array!}  
```  

**Map Handling (annotations, labels):**
```go  
// If map EXISTS → add key directly  
if pod.Annotations != nil {  
    patch = `{"op": "add", "path": "/metadata/annotations/sidecar.ananse.io~1status", "value": "injected"}`}  
// If map MISSING → create with object  
if pod.Annotations == nil {  
    patch = `{"op": "add", "path": "/metadata/annotations", "value": {"sidecar.ananse.io/status": "injected"}}`}  
```  

**Path Escaping:**
- `/` in key → `~1` (e.g., `sidecar.ananse.io/status` → `sidecar.ananse.io~1status`)
- `~` in key → `~0`

**Skip Conditions (do NOT inject):**
```go  
func shouldSkip(pod *corev1.Pod, ns string) bool {  
    // hostNetwork - would modify NODE's iptables, not pod's    if pod.Spec.HostNetwork {        return true    }    // System namespaces    if ns == "kube-system" || ns == "kube-public" {        return true    }    // Explicit opt-out annotation    if pod.Annotations["sidecar.ananse.io/inject"] == "false" {        return true    }    return false}  
```  

**Why skip hostNetwork:**
- Pod shares Node's network namespace
- iptables commands would modify Node's firewall (dangerous!)
- Port 15001/15006 could conflict with Node processes

### Istio Injector Patterns (Reference)

**Source:**
- https://github.com/istio/istio/blob/master/pkg/kube/inject/webhook.go
- https://github.com/istio/istio/blob/master/pkg/kube/inject/inject.go

**Injection Decision Hierarchy:**
```  
1. hostNetwork: true → SKIP (always, iptables would modify node)  
2. Ignored namespaces (kube-system, etc.) → SKIP  
3. Pod annotation `sidecar.istio.io/inject: "false"` → SKIP  
4. Pod annotation `sidecar.istio.io/inject: "true"` → INJECT  
5. NeverInjectSelector matches → SKIP  
6. AlwaysInjectSelector matches → INJECT  
7. Namespace policy (enabled/disabled) → Default behavior  
```  

**Patch Generation Pattern:**
```go  
// Istio uses jsonpatch library to diff original vs modified pod  
import "github.com/mattbaird/jsonpatch"  
patch, _ := jsonpatch.CreatePatch(originalPodBytes, modifiedPodBytes)  
```  

**MVP Skip Logic (simplified from Istio):**
```go  
func injectRequired(pod *corev1.Pod, ns string) bool {  
    if pod.Spec.HostNetwork { return false }    if ns == "kube-system" || ns == "kube-public" { return false }    if pod.Annotations["sidecar.ananse.io/inject"] == "false" { return false }    return true}  
```  

### Linkerd Injector Patterns (Reference)

**Source:**
- https://github.com/linkerd/linkerd2/blob/main/pkg/inject/report.go
- https://github.com/linkerd/linkerd2/blob/main/controller/proxy-injector/webhook.go

**Skip Reasons (returns reasons for debugging - cleaner than Istio):**
```go  
const (  
    hostNetworkEnabled                   = "host_network_enabled"    sidecarExists                        = "sidecar_already_exists"    unsupportedResource                  = "unsupported_resource"    injectEnableAnnotationAbsent         = "injection_enable_annotation_absent"    injectDisableAnnotationPresent       = "injection_disable_annotation_present"    disabledAutomountServiceAccountToken = "disabled_automount_service_account_token"    udpPortsEnabled                      = "udp_ports_enabled")  
  
func (r *Report) Injectable() (bool, []string) {  
    var reasons []string    if r.HostNetwork        { reasons = append(reasons, hostNetworkEnabled) }    if r.Sidecar            { reasons = append(reasons, sidecarExists) }    if r.UnsupportedResource { reasons = append(reasons, unsupportedResource) }    if r.InjectDisabled     { reasons = append(reasons, r.InjectDisabledReason) }    return len(reasons) == 0, reasons}  
```  

**Istio vs Linkerd Comparison:**  
| Aspect | Istio | Linkerd |  
|--------|-------|---------|  
| Annotation | `sidecar.istio.io/inject` | `linkerd.io/inject` |  
| Default mode | Namespace label enables | Annotation required |  
| Skip reasons | Boolean only | Returns reason strings |  
| Sidecar check | Checks status annotation | Checks for existing proxy container |  
| UDP handling | Allowed | Skips if UDP ports present |

**Pattern to use for Ananse:** Linkerd's `Injectable() (bool, []string)` - returning reasons is better for debugging.

### AdmissionReview Versions

**Bottom line:** Use v1 only. v1beta1 deprecated in K8s 1.19, removed in 1.25.

| Aspect | v1beta1 | v1 |  
|--------|---------|-----|  
| Status | Removed in 1.25 | Current standard |  
| `Allowed` field | Defaulted to true | Must be explicit |  
| `PatchType` | Optional | Required if returning patch |  
| `sideEffects` | Optional | Required in webhook config |  

**Go imports:**
```go  
import admissionv1 "k8s.io/api/admission/v1"  
```  

**Response pattern (all fields required in v1):**
```go  
jsonPatchType := admissionv1.PatchTypeJSONPatch  
  
response := &admissionv1.AdmissionResponse{  
    UID:       req.UID,           // MUST match request UID    Allowed:   true,              // MUST be explicit (no default)    PatchType: &jsonPatchType,    // MUST be set if Patch present    Patch:     patchBytes,}  
```  

**MutatingWebhookConfiguration (v1):**
```yaml  
apiVersion: admissionregistration.k8s.io/v1  
kind: MutatingWebhookConfiguration  
metadata:  
  name: ananse-sidecar-injectorwebhooks:  
  - name: sidecar.ananse.io    admissionReviewVersions: ["v1"]    sideEffects: None  # REQUIRED in v1    clientConfig:      service:        name: ananse-webhook        namespace: ananse-system        path: /mutate    rules:      - operations: ["CREATE"]        apiGroups: [""]        apiVersions: ["v1"]        resources: ["pods"]    namespaceSelector:      matchExpressions:        - key: kubernetes.io/metadata.name          operator: NotIn          values: ["kube-system", "kube-public"]  
```  

### Certificate Management

**Why TLS required:** API server → webhook connection. API server refuses to call webhook without valid TLS.

**`caBundle`:** Tells API server "trust this CA when calling my webhook"

**failurePolicy implications:**  
| Policy | On TLS Error | Risk |  
|--------|--------------|------|  
| `Fail` | Blocks pod creation | Outage if cert expires |  
| `Ignore` | Pod runs without sidecar | Security gap, silent failure |

**Strategy Decision:**
- **Phase 1 (MVP):** Self-signed script, `failurePolicy: Fail`
- **Phase 2 (Production):** cert-manager with auto-rotation

**Cert Strategies Compared:**  
| Strategy | Pros | Cons |  
|----------|------|------|  
| Self-signed script | Zero dependencies, simple | Manual rotation (365 days) |  
| K8s CSR API | Built-in, no external deps | Complex Go code for CSR controller |  
| cert-manager | Auto-rotation, industry standard | External dependency, chicken-egg risk |

**Chicken-and-Egg Deadlock (cert-manager):**
- cert-manager pod restarts → webhook intercepts it → webhook needs cert from cert-manager → DEADLOCK
- **Fix:** Exclude `cert-manager` namespace from injection!

**Updated namespace exclusions:**
```go  
excludedNS := map[string]bool{  
    "kube-system":   true,    "kube-public":   true,    "cert-manager":  true,  // Prevent deadlock!    "ananse-system": true,  // Don't inject into own controlplane}  
```  

**Self-signed cert script (MVP):**
```bash  
#!/bin/bash  
SERVICE=ananse-webhook  
NAMESPACE=ananse-system  
SECRET_NAME=ananse-webhook-certs  
DAYS=365  
  
# Generate CA  
openssl genrsa -out ca.key 2048  
openssl req -x509 -new -nodes -key ca.key -days $DAYS -out ca.crt -subj "/CN=ananse-ca"  
  
# Generate server cert (SAN required for K8s 1.19+)  
openssl genrsa -out server.key 2048  
openssl req -new -key server.key -out server.csr -subj "/CN=${SERVICE}.${NAMESPACE}.svc" \  
  -addext "subjectAltName=DNS:${SERVICE}.${NAMESPACE}.svc,DNS:${SERVICE}.${NAMESPACE}.svc.cluster.local"openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out server.crt -days $DAYS  
  
# Create K8s secret  
kubectl create secret tls $SECRET_NAME \  
  --cert=server.crt --key=server.key \  -n $NAMESPACE --dry-run=client -o yaml | kubectl apply -f -  
# Output caBundle for webhook config  
echo "caBundle (base64):"  
cat ca.crt | base64 | tr -d '\n'  
```  

### Security Context Requirements

**Why different contexts for init vs sidecar:**
- Init container: Needs iptables access (network namespace manipulation)
- Sidecar: Only needs to bind ports, handle traffic - no special privileges
- UID 1337: Required for iptables rule `--uid-owner 1337 -j RETURN` (loop prevention)

**Linux Capabilities (granular privileges):**
- `NET_ADMIN`: iptables, network namespace config, interface config
- `NET_RAW`: Raw sockets (needed for some iptables operations)
- Not full root - if compromised, can't load kernel modules, mount filesystems, etc.

**Pod Security Standards compatibility:**

| Level | Init Container (NET_ADMIN, root) | Sidecar (uid 1337, no caps) |  
|-------|----------------------------------|----------------------------|  
| `privileged` | ✅ Allowed | ✅ Allowed |  
| `baseline` | ✅ Allowed | ✅ Allowed |  
| `restricted` | ❌ Blocked | ✅ Allowed |  

**If cluster enforces `restricted`:**
1. Exempt mesh namespace from PSS
2. Use CNI-level interception (Cilium, Istio ambient) - no init container
3. Accept mesh can't run on `restricted` clusters

**Init container security context:**
```yaml  
initContainers:  
  - name: ananse-init    image: ananse-init:latest    securityContext:      runAsUser: 0                    # Root required for iptables      runAsNonRoot: false      capabilities:        add: ["NET_ADMIN", "NET_RAW"] # Minimal caps needed        drop: ["ALL"]                 # Drop everything else      privileged: false               # NOT fully privileged      allowPrivilegeEscalation: false  
```  

**Sidecar security context:**
```yaml  
containers:  
  - name: ananse-proxy    image: ananse-proxy:latest    securityContext:      runAsUser: 1337                 # Match iptables --uid-owner rule      runAsNonRoot: true      capabilities:        drop: ["ALL"]                 # No caps needed      allowPrivilegeEscalation: false      readOnlyRootFilesystem: true    # Extra hardening  
```  

### Failure Modes & Recovery

**Key insight:** iptables rules persist in kernel even when sidecar dies. Traffic still redirected → black hole.

**Sidecar crash flow:**
```  
App calls google.com:443  
    ↓  
OUTPUT chain: -j REDIRECT --to-ports 15001  
    ↓  
Kernel rewrites dest to 127.0.0.1:15001  
    ↓  
Nothing listening on 15001  
    ↓  
"Connection refused" (instant, not timeout)  
```  

**Failure mode summary:**

| Scenario | Symptom | Root Cause | Recovery |  
|----------|---------|------------|----------|  
| Webhook down | Pods rejected (or un-injected if `Ignore`) | Webhook unreachable | Fix webhook, pods retry |  
| Init container fails | `Init:CrashLoopBackOff` | iptables command error | Check logs, fix script |  
| Sidecar crashes | App gets "connection refused" on ALL traffic | iptables active, no listener | Restart sidecar, or delete pod |  
| Sidecar OOMKilled | Same as crash | Memory limit too low | Increase memory limit |  

**Sidecar health checks (critical):**
```yaml  
containers:  
  - name: ananse-proxy    livenessProbe:      httpGet:        path: /healthz        port: 15021           # Admin port      initialDelaySeconds: 5      periodSeconds: 10    readinessProbe:      httpGet:        path: /ready        port: 15021      initialDelaySeconds: 1      periodSeconds: 2  
```  

**Why readiness matters:** If sidecar not ready → K8s removes pod from Service endpoints → no traffic to broken pod.

**Emergency escape hatch:**
```bash  
# Option 1: Clear iptables manually (if can exec in)  
kubectl exec -it pod-name -c app -- iptables -t nat -F  
  
# Option 2: Recreate pod without injection  
kubectl annotate pod pod-name sidecar.ananse.io/inject=false  
kubectl delete pod pod-name  # Deployment recreates without sidecar  
```  

**failurePolicy decision:** Use `Fail` (not `Ignore`) - better to reject pods than run un-injected pods silently.

### Resource Limits

**Requests vs Limits:**

| Resource | Request | Limit Exceeded |  
|----------|---------|----------------|  
| **CPU** | Scheduling guarantee (reserve on node) | Throttled (slower, NOT killed) |  
| **Memory** | Scheduling guarantee (reserve on node) | **OOMKilled** (container dies) |  

**Why this matters for sidecar:**
- CPU too low → High latency (throttling)
- Memory too low → Sidecar dies → "connection refused" (iptables trap)

**Reference: Istio/Linkerd defaults:**

| Mesh | CPU Req | CPU Limit | Mem Req | Mem Limit |  
|------|---------|-----------|---------|-----------|  
| Istio | 100m | 2000m | 128Mi | 1024Mi |  
| Linkerd | 100m | none | 20Mi | 250Mi |  

**Ananse starting point:**
```yaml  
# Sidecar (handles all traffic)  
containers:  
  - name: ananse-proxy    resources:      requests:        cpu: 100m        memory: 64Mi      # Simpler than Envoy      limits:        cpu: 500m         # Allow bursting        memory: 128Mi     # OOMKill threshold  
# Init container (runs once, minimal)  
initContainers:  
  - name: ananse-init    resources:      requests:        cpu: 10m        memory: 10Mi      limits:        cpu: 100m        memory: 50Mi  
```  

**Tuning strategy:**
1. Start with above values
2. Monitor: `kubectl top pods`
3. Check OOMKills: `kubectl get events --field-selector reason=OOMKilled`
4. Increase memory if OOMKilled, increase CPU if p99 latency spikes

### Envoy Original Destination (Reference Architecture)

**Why study Envoy:** Istio uses Envoy under the hood. Understanding Envoy's pattern informs Ananse design.

**Two components working together:**

| Component | Purpose | How it works |  
|-----------|---------|--------------|  
| **Listener Filter** (`original_dst`) | Extracts original destination | Reads `SO_ORIGINAL_DST` from socket |  
| **Cluster** (`ORIGINAL_DST`) | Routes to extracted destination | Adds upstream hosts on-demand, cleans unused |  

**Use cases for original destination:**
1. Route to previously unknown destinations (egress proxy)
2. Route to user-specified arbitrary upstream addresses (no load balancing)

**Envoy config pattern:**
```yaml  
# Listener with original_dst filter  
listeners:  
  - name: outbound    address:      socket_address: { address: 0.0.0.0, port_value: 15001 }    listener_filters:      - name: envoy.filters.listener.original_dst    filter_chains:      - filters:          - name: envoy.filters.network.tcp_proxy            typed_config:              cluster: original_dst_cluster  
# Cluster that routes to original destination  
clusters:  
  - name: original_dst_cluster    type: ORIGINAL_DST    lb_policy: CLUSTER_PROVIDED  # Required for ORIGINAL_DST  
```  

**Fallback mechanism:** `x-envoy-original-dst-host` header
- When SO_ORIGINAL_DST unavailable, Envoy reads destination from this header
- Example: `x-envoy-original-dst-host: 10.195.16.237:8888`

**Security warning:** Original destination routing allows routing to ANY host - requires mTLS/RBAC controls.

**Ananse vs Envoy comparison:**

| Aspect | Envoy | Ananse Proxy |  
|--------|-------|--------------|  
| Get original dest | Listener filter (C++) | `getOriginalDst()` (Go syscall) |  
| Route decision | Cluster config | Direct `net.Dial(originalDst)` |  
| Host management | On-demand add, periodic cleanup | Stateless (dial per connection) |  
| Complexity | Full L7 proxy | Simple passthrough |  

**Sources:**
- https://www.envoyproxy.io/docs/envoy/latest/configuration/listeners/listener_filters/original_dst_filter
- https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/load_balancing/original_dst
- https://venilnoronha.medium.com/introduction-to-original-destination-in-envoy-d8a8aa184bb6

### Research Checklist (2026-01-30)
- [x] SO_ORIGINAL_DST mechanism
- [x] Go implementation pattern (IPv6Mreq hack)
- [x] REDIRECT vs TPROXY decision (Phase 1: REDIRECT, Phase 2: TPROXY)
- [x] iptables rules for init container
- [x] JSON Patch edge cases (RFC 6902)
- [x] Istio sidecar injector source
- [x] Linkerd proxy-injector source
- [x] AdmissionReview v1 vs v1beta1
- [x] Certificate management (self-signed MVP, cert-manager later)
- [x] Security context requirements (capabilities, PSS levels)
- [x] Failure modes & recovery (iptables trap, health checks, escape hatch)
- [x] Resource limits (requests vs limits, tuning strategy)
- [x] Envoy original destination (listener filter + cluster pattern)
- [x] Go syscall package (RawConn.Control pattern, golang.org/x/sys/unix)

### Project Structure

```  
ananse/  
├── controlplane/  
│   ├── main.go                    # Existing - add webhook server  
│   ├── injector/  
│   │   ├── injector.go            # Core injection logic (shouldSkip, Injectable)  
│   │   ├── patch.go               # JSON patch generation (array/map handling)  
│   │   ├── webhook.go             # /mutate HTTP handler (AdmissionReview v1)  
│   │   └── config.go              # Injection config (ports, images, UIDs)  
│   └── ...  
│  
├── pkg/  
│   └── proxy/  
│       ├── handler.go             # Existing  
│       ├── tracer.go              # Existing  
│       ├── originaldst.go         # NEW: getOriginalDst() using RawConn.Control  
│       ├── inbound.go             # NEW: 15006 listener  
│       └── outbound.go            # NEW: 15001 listener  
│  
├── scripts/  
│   ├── generate-certs.sh          # TLS cert generation (self-signed MVP)  
│   └── iptables-init.sh           # Init container iptables script  
│  
├── deploy/  
│   ├── namespace.yaml             # ananse-system namespace  
│   ├── rbac.yaml                  # ServiceAccount, ClusterRole, ClusterRoleBinding  
│   ├── webhook-deployment.yaml    # Controlplane deployment  
│   ├── webhook-service.yaml       # Service for webhook endpoint  
│   ├── webhook-config.yaml        # MutatingWebhookConfiguration  
│   └── secret.yaml                # TLS certs (generated, not committed)  
│  
├── docker/  
│   ├── Dockerfile.proxy           # Sidecar proxy image  
│   └── Dockerfile.init            # Init container image (alpine + iptables)  
│  
└── CLAUDE.md  
```  

**Files to create (post-exams):**

| Priority | File | Purpose |  
|----------|------|---------|  
| 1 | `pkg/proxy/originaldst.go` | `getOriginalDst()` function |  
| 2 | `pkg/proxy/outbound.go` | 15001 listener + dial original dest |  
| 3 | `pkg/proxy/inbound.go` | 15006 listener |  
| 4 | `scripts/iptables-init.sh` | Init container script (already written above) |  
| 5 | `controlplane/injector/injector.go` | Skip conditions, Injectable() |  
| 6 | `controlplane/injector/patch.go` | JSON patch builder |  
| 7 | `controlplane/injector/webhook.go` | /mutate handler |  
| 8 | `scripts/generate-certs.sh` | Cert generation (already written above) |  
| 9 | `deploy/*.yaml` | K8s manifests |  
| 10 | `docker/Dockerfile.*` | Container images |  

### In Progress
- [x] ~~Determine exact iptables rules~~ - DONE
- [x] ~~Define project structure~~ - DONE
- Need to define concrete container specs:
    - Init container: image, capabilities (NET_ADMIN, NET_RAW)
    - Sidecar container: image, ports, env vars, UID 1337

### Next Immediate Steps (Post-Exams)
1. ~~Write out the exact iptables commands for init container~~ - DONE
2. Implement `getOriginalDst(conn net.Conn)` function in proxy
3. Add inbound/outbound listeners (15006, 15001) to proxy
4. Define the JSON patch structure with concrete values
5. Implement `/mutate` handler in controlplane
6. Create webhook deployment and config YAMLs
7. Generate TLS certs for webhook

### Blockers
- None

### Previous Session (2026-01-21)
- Implemented distributed tracing in proxy gateway
- `pkg/proxy/tracer.go`, `pkg/proxy/handler.go`, `pkg/proxy/reverseProxy.go`
- Tempo integration working in Grafana