# Ananse Service Mesh - Testing Plan

**Author:** Anthony
**Created:** 2026-03-19
**Status:** In Progress
**Target Completion:** 2026-03-31

---

## Objective

Validate Ananse service mesh for production deployment. This testing plan covers:

1. **Scenario Matrix** - Functional validation under various failure conditions
2. **Performance Benchmarks** - Measure mesh overhead (latency, throughput, resource usage)
3. **Chaos Testing** - Deliberate failure injection to find edge cases
4. **Baseline Comparison** - Quantify the cost of running traffic through the mesh

The goal is production-grade confidence before deploying to external clusters.

---

## Test Environment

- **Cluster:** Local Kind cluster (`ananse-control-plane`)
- **Namespaces:**
  - `ananse` - Simple test services (health checks)
  - `boutique` - Google Online Boutique (complex inter-service flows)
  - `boutique-baseline` - Same app WITHOUT mesh (for comparison)
  - `ananse-system` - Control plane
- **Observability Stack:**
  - Prometheus (metrics via PodMonitor — scrapes sidecar `:15021/metrics`)
  - Grafana (dashboards — auto-provisioned from `ananse-chart/dashboards/ananse-dashboard.json`)
  - Tempo (traces via OTLP gRPC — set `filterHealthChecks: true` to suppress probe noise)
  - Loki + Promtail (logs — use `{namespace="<ns>", container="ananse-proxy"}` for sidecar logs only)

---

## Test Application: Google Online Boutique

For realistic mesh validation, we use Google's Online Boutique (Hipster Shop) - a cloud-native microservices demo application.

### Why Online Boutique?

- **11 microservices** with real inter-service dependencies
- **Mixed protocols** - gRPC between services, HTTP for frontend
- **Complex call patterns** - Checkout calls 5 downstream services
- **Built-in load generator** - Simulates realistic user traffic
- **Production-like architecture** - Caching, databases, async processing

### Service Architecture

```
                         ┌─────────────────┐
                         │  LoadGenerator  │ (simulates users)
                         └────────┬────────┘
                                  ▼
                         ┌─────────────────┐
                         │    Frontend     │ (HTTP)
                         └────────┬────────┘
          ┌──────────────────┬────┴────┬──────────────────┐
          ▼                  ▼         ▼                  ▼
   ┌─────────────┐   ┌─────────────┐ ┌─────────────┐ ┌─────────────┐
   │  AdService  │   │CartService  │ │   Product   │ │Recommendation│
   │   (gRPC)    │   │   (gRPC)    │ │  Catalog    │ │   (gRPC)    │
   └─────────────┘   └──────┬──────┘ │   (gRPC)    │ └─────────────┘
                            │        └─────────────┘
                            ▼
                    ┌─────────────┐
                    │CheckoutSvc  │ (gRPC)
                    └──────┬──────┘
       ┌─────────────┬─────┴─────┬─────────────┬─────────────┐
       ▼             ▼           ▼             ▼             ▼
┌─────────────┐┌─────────────┐┌─────────────┐┌─────────────┐┌─────────────┐
│  Currency   ││  Payment    ││  Shipping   ││   Email     ││    Cart     │
│   (gRPC)    ││   (gRPC)    ││   (gRPC)    ││   (gRPC)    ││   (gRPC)    │
└─────────────┘└─────────────┘└─────────────┘└─────────────┘└─────────────┘
```

### Call Flow Example: Checkout

When a user checks out, this call chain executes:

```
Frontend
  └─► CheckoutService
        ├─► CartService (get cart items)
        ├─► ProductCatalogService (get product details)
        ├─► CurrencyService (convert prices)
        ├─► ShippingService (calculate shipping)
        ├─► PaymentService (charge card)
        └─► EmailService (send confirmation)
```

This is **exactly** the kind of fan-out pattern we need to validate mesh behavior.

### Deployment Strategy

We deploy Online Boutique twice:

| Namespace | Mesh Injection | Purpose |
|-----------|----------------|---------|
| `boutique` | **Enabled** | Measure WITH mesh |
| `boutique-baseline` | **Disabled** | Measure WITHOUT mesh (baseline) |

This allows direct A/B comparison of latency, throughput, and resource usage.

### Services Summary

| Service | Language | Protocol | Description |
|---------|----------|----------|-------------|
| frontend | Go | HTTP | Web UI, calls all services |
| cartservice | C# | gRPC | Shopping cart (Redis) |
| productcatalogservice | Go | gRPC | Product listing |
| currencyservice | Node.js | gRPC | Currency conversion |
| paymentservice | Node.js | gRPC | Payment processing |
| shippingservice | Go | gRPC | Shipping quotes |
| emailservice | Python | gRPC | Email confirmation |
| checkoutservice | Go | gRPC | Checkout orchestration |
| recommendationservice | Python | gRPC | Product recommendations |
| adservice | Java | gRPC | Advertisement serving |
| loadgenerator | Python | HTTP | Traffic simulation |

---

## Phase 1: Scenario Matrix

Validate mesh behavior under realistic operational conditions.

### Test Cases

| # | Scenario | Inject | Observe | Pass Criteria |
|---|----------|--------|---------|---------------|
| 1 | Fan-out (1→4 services) | Normal load via `load-test.sh` | RPS distribution across backends | Even distribution (no pod >40%) |
| 2 | Backend failure | `kubectl delete pod` (1 of 3) | Health status, circuit breaker, error rate | Recovery <30s, errors return to baseline |
| 3 | Slow backend | Add 5s delay to one service | P99 latency, circuit breaker state | CB opens, traffic shifts to healthy backends |
| 4 | Rolling update | `kubectl rollout restart` | Error rate, RPS stability | Zero errors during rollout |
| 5 | Sidecar crash | `pkill ananse-proxy` in container | Error rate, pod restart | Other services unaffected, sidecar restarts |
| 6 | Control plane disconnect | Scale controlplane to 0 | Traffic continuity, reconnection | Traffic uninterrupted, reconnects on restore |
| 7 | Config hot reload | Label change during load | Error rate, config propagation | Zero dropped requests |

### Execution

```bash
# Start load generator
kubectl exec -n ananse deployment/auth -c auth -- sh /load-test.sh 300

# Watch dashboard
open http://localhost:3000/d/ananse-mesh-dashboard

# Execute failure injection per scenario
# Record observations in test report
```

---

## Phase 2: Performance Benchmarks

Quantify mesh overhead to set expectations for production.

### Metrics to Capture

| Metric | Description | Target |
|--------|-------------|--------|
| P50 Latency | Median request latency | <5ms overhead |
| P99 Latency | Tail latency | <10ms overhead |
| Max RPS | Throughput ceiling | <15% reduction |
| CPU per sidecar | Resource consumption | <100m under load |
| Memory per sidecar | Resource consumption | <128Mi steady state |

### Baseline Comparison Method

We deploy Online Boutique in two namespaces - one with mesh, one without:

```
┌─────────────────────────────────────────────────────────────┐
│ NAMESPACE: boutique-baseline (NO MESH)                      │
│                                                             │
│   frontend ──► checkout ──► payment ──► cart                │
│            direct calls, no sidecars                        │
│                                                             │
│   Measures: Native application latency                      │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│ NAMESPACE: boutique (WITH MESH)                             │
│                                                             │
│   frontend ──► [sidecar] ──► [sidecar] ──► checkout         │
│            ──► [sidecar] ──► [sidecar] ──► payment          │
│                                                             │
│   Measures: Application latency + mesh overhead             │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│ OVERHEAD CALCULATION                                        │
│                                                             │
│   Mesh Overhead = boutique_p99 - boutique_baseline_p99      │
│                                                             │
│   Per-hop overhead = Total overhead / number of hops        │
│   (Checkout flow has 6 hops, so divide by 6)                │
└─────────────────────────────────────────────────────────────┘
```

### Benchmark Procedure

1. Deploy Online Boutique to `boutique-baseline` (no injection)
2. Deploy Online Boutique to `boutique` (with injection)
3. Let both stabilize (2-3 minutes)
4. Disable loadgenerator in both namespaces
5. Run controlled benchmark from external client
6. Compare p50, p99, throughput, resource usage
7. Calculate per-hop overhead

### Tools

- `load-test.sh` - Traffic generation with realistic patterns
- `curl` with timing - Single request latency measurement
- Grafana dashboard - Real-time p50/p99/error rate visualization
- `kubectl top pods` - Resource consumption

---

## Phase 3: Chaos Testing

Deliberately break things to find failure modes before production does.

### Failure Injection Scenarios

| Category | Scenario | Method | Expected Behavior |
|----------|----------|--------|-------------------|
| **Pod** | Sidecar OOM | Set memory limit to 10Mi, generate load | K8s restarts container, brief error spike |
| **Pod** | Sidecar receives bad config | Push malformed config | Reject and continue on last-known-good |
| **Network** | Packet loss | `tc qdisc add dev eth0 root netem loss 10%` | Retries succeed, latency increases |
| **Network** | Partition | Network policy blocking traffic | Circuit breaker opens, clear errors |
| **Control Plane** | Prolonged disconnect | Scale to 0 for 10+ minutes | Sidecars continue, reconnect on restore |
| **Scale** | Rapid scaling | 2→20→2 pods in 60s | Mesh adapts, no dropped requests |
| **Config** | Update during spike | Config change + traffic spike | No race conditions, clean apply |

### Chaos Tools

**Option 1: Manual kubectl (simple, no setup)**
```bash
kubectl delete pod ...
kubectl exec ... -- pkill ...
kubectl scale ...
```

**Option 2: Chaos Mesh (automated, repeatable)**
```bash
helm install chaos-mesh chaos-mesh/chaos-mesh -n chaos-mesh --create-namespace
```

### Documentation Requirements

For each failure mode, document:
- Symptoms observed
- Time to detection
- Time to recovery
- Logs/metrics that indicate the failure
- Recommended operator response

---

## Phase 4: Test Backend Requirements

Current test services (`analytics`, `auth`, `users`, `payments`, `echo`) perform simple health checks. For realistic mesh validation, we need services that:

1. **Make inter-service calls** - Service A calls Service B calls Service C
2. **Have realistic latency** - Variable response times (10ms-500ms)
3. **Support fault injection** - Configurable errors, delays
4. **Generate traces** - Propagate trace context across calls

### Options

| Option | Services | Complexity | Inter-service Calls | Setup |
|--------|----------|------------|---------------------|-------|
| Google Online Boutique | 11 | High | Yes (checkout→cart→product→shipping) | `kubectl apply -f` |
| Weaveworks Sock Shop | 7 | Medium | Yes (orders→carts→catalogue) | `kubectl apply -f` |
| Istio Bookinfo | 4 | Low | Yes (productpage→reviews→ratings) | `kubectl apply -f` |
| OpenTelemetry Demo | 12 | High | Yes (frontend→cart→checkout→payment) | Helm chart |
| Custom enhanced services | 5 | Low | Add to existing | Modify current code |

### Recommendation

Use **Istio Bookinfo** or **Google Online Boutique** for complex flow testing. They provide:
- Real service-to-service communication patterns
- Fan-out scenarios (1 service calling multiple backends)
- Database dependencies
- Configurable failure injection

---

## Test Execution Schedule

| Day | Phase | Focus |
|-----|-------|-------|
| 1 | Setup | Deploy test backends, verify observability |
| 2-3 | Phase 1 | Scenario matrix (7 test cases) |
| 4 | Phase 2 | Performance benchmarks |
| 5-6 | Phase 3 | Chaos testing |
| 7 | Documentation | Test report, findings, recommendations |

---

## Exit Criteria

Testing is complete when:

- [ ] All 7 scenario matrix tests pass
- [ ] Performance overhead documented (p50, p99, throughput, resources)
- [ ] All chaos scenarios documented with observed behavior
- [ ] No silent failures - every failure produces logs/metrics/alerts
- [ ] Escape hatches documented (how to remove service from mesh)
- [ ] Test report written with findings and recommendations

---

## Deliverables

1. **Test Report** (`docs/TEST_REPORT.md`)
   - Test results for each scenario
   - Performance benchmark data
   - Failure mode observations
   - Recommendations

2. **Benchmark Data** (`benchmark-results/`)
   - Raw latency measurements
   - Resource consumption logs
   - Comparison charts

3. **Runbook** (`docs/RUNBOOK.md`)
   - How to diagnose common issues
   - Emergency procedures
   - Escape hatches

---

## References

- [Ananse Dashboard](../tools/dashboard/ananse_dashboard.json)
- [Load Test Script](../scripts/load-test.sh)
- [Week 10-12 Roadmap](../CLAUDE.md)
