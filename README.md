# k8s-blue-green-deploy

Blue-green deployment pipeline on Kubernetes using Argo Rollouts, running on a Raspberry Pi 5 homelab k3s cluster.

Push to `main` triggers: lint -> test -> build multi-arch image -> push to GHCR. Manifests are applied manually (see [Deploy to k3s](#deploy-to-k3s)); a follow-up will wire this repo into an ArgoCD Application for GitOps sync.

## How it works

```
                    ┌──────────────┐
                    │   Traefik    │
                    │   Ingress    │
                    └──────┬───────┘
                           │
                ┌──────────┴──────────┐
                │                     │
        ┌───────┴───────┐     ┌───────┴───────┐
        │  active-svc   │     │  preview-svc  │
        │  (production) │     │  (staging)    │
        └───────┬───────┘     └───────┬───────┘
                │                     │
        ┌───────┴───────┐     ┌───────┴───────┐
        │  Blue pods    │     │  Green pods   │
        │  (current)    │     │  (new ver)    │
        └───────────────┘     └───────────────┘
```

1. CI builds and pushes a multi-arch image (amd64 + arm64) to GHCR
2. ArgoCD detects the new image tag and syncs the Rollout manifest
3. Argo Rollouts creates a new ReplicaSet (preview) alongside the current one (active)
4. Preview traffic is routed through `demo-preview.homelab.local`
5. `AnalysisTemplate` runs 3 HTTP health checks against the preview service
6. If all pass, Rollouts swaps the `active-svc` selector to the new pods
7. Old ReplicaSet scales down after a 30-second settle period
8. If the health check fails, the preview is torn down and active stays untouched

## Stack

- Go 1.25 with `log/slog` for structured JSON logging
- One runtime dep: `prometheus/client_golang` for `/metrics` (RED metrics + Go runtime)
- Multi-stage Dockerfile with `distroless/static:nonroot` runtime
- Multi-arch build: `linux/amd64` + `linux/arm64` via `docker buildx`
- GHCR (GitHub Container Registry) - free, no cloud registry cost
- Argo Rollouts blue-green strategy with `prePromotionAnalysis`
- Traefik ingress with wildcard TLS
- k3s on Raspberry Pi 5 (8GB)

## Endpoints

| Path | Purpose |
|---|---|
| `GET /` | JSON with version, color, hostname, timestamp (non-`GET` -> 405; unknown paths -> 404) |
| `GET /healthz` | Liveness probe (always 200; excluded from RED metrics) |
| `GET /ready` | Readiness probe (503 during startup/shutdown, 200 when ready; excluded from RED metrics) |
| `GET /slow?ms=N[&jitter]` | Sleeps for `N` ms (clamped to `[0, 5000]`), then 200. With `&jitter`, sleeps a uniformly random duration in `[0, N]`. Used to seed the p95/p99 tail. |
| `GET /fail?code=N` | Returns status code `N` where `N` is in `[400, 599]`. Anything else falls back to 500. Used to seed non-2xx rate. |
| `GET /metrics` | Prometheus scrape endpoint (RED metrics + Go runtime; excluded from RED metrics itself) |

The mux uses Go 1.22+ route patterns (`GET /{$}` for the exact root, plus a `/` catch-all for 404s). The `path` label in `http_requests_total` and `http_request_duration_seconds` is the matched route pattern rather than the raw URL, so metric cardinality stays bounded no matter what the client throws at the server.

## CI pipeline

| Trigger | Steps |
|---|---|
| PR to `main` | lint -> test (both `app/` and `traffic-gen/`) |
| Push to `main` | lint -> test -> build -> push demo-app image to GHCR (multi-arch) |
| Manual dispatch | promote preview to active via `kubectl argo rollouts promote` |

## Observability

### Structured logging

All log lines are JSON on stdout via `log/slog`. Every line carries `version` (git SHA) and `color` (ReplicaSet hash) as persistent attributes, which makes it trivial to filter one deploy's logs out of a mixed blue/green stream in Loki/Grafana:

```json
{"time":"2026-08-22T21:53:12Z","level":"INFO","msg":"request","version":"9cff7c3b","color":"7757fdf89f","method":"GET","path":"/","route":"GET /{$}","status":200,"duration_ms":1,"bytes":89,"remote":"10.42.0.1"}
```

The `path` field is the raw URL as the client requested it; `route` is the mux pattern the request matched (or `unmatched` if nothing matched, which should never happen after the `/` catch-all). Filter by `route` when you care about the endpoint shape, and by `path` when you care about what a specific client hit.

Set `LOG_LEVEL=debug|info|warn|error` (default `info`). Probe endpoints (`/healthz`, `/ready`) and `/metrics` are skipped by the request-logging middleware to keep the signal readable.

### Metrics

`/metrics` exposes:

| Metric | Type | Description |
|---|---|---|
| `http_requests_total{method,path,status}` | counter | Request count. `path` is the route pattern (e.g. `GET /{$}`, `GET /slow`), not the raw URL. |
| `http_request_duration_seconds{method,path}` | histogram | Latency (5ms..10s buckets), `path` label as above. |
| `http_requests_in_flight` | gauge | Concurrent requests |
| `app_ready` | gauge | 1 if ready, 0 otherwise |
| `app_info{version,color}` | gauge | Always 1, use for join-friendly PromQL |
| `go_*`, `process_*` | various | Runtime + process metrics from `client_golang` |

Example join, "request rate broken out by deploy color":

```promql
sum by (color) (rate(http_requests_total[5m])) * on(instance) group_left(color) app_info
```

A dedicated `demo-app-metrics` `NodePort` Service (port 30130) exposes `/metrics` to a Prometheus that lives outside the cluster (systemd unit on the Pi, not `kube-prometheus-stack`). If you're running an in-cluster Prometheus with operator/annotation discovery, swap that for a `ServiceMonitor` or pod annotations as appropriate.

### Grafana dashboard

`grafana/dashboard.json` -- 11 panels across three rows (Deploy state, RED, Go runtime). Import it:

> Grafana -> Dashboards -> New -> Import -> Upload JSON file -> `grafana/dashboard.json` -> pick your Prometheus datasource -> Import

During a promotion you'll see two versions in the "Ready pods by version + color" table for the `scaleDownDelaySeconds` window, then the old one drop out.

### Continuous traffic

`traffic-gen/` is a small Go binary that keeps the dashboard populated 24/7 so a portfolio visitor sees live RED metrics instead of "No data". The reference deployment runs it on an Oracle Cloud Ampere A1 VM that reaches the Pi over a WireGuard tunnel; it targets `http://10.0.0.1:30130` at ~3 RPS with a slow diurnal wave and a small injected mix of latency and errors so every panel has signal. See [`traffic-gen/README.md`](traffic-gen/README.md) for the traffic shape, flags, and install steps.

## Graceful shutdown

The app handles `SIGTERM` properly:
1. Marks `/ready` as 503 (stops receiving new traffic)
2. Waits 5 seconds (lets in-flight requests drain and readiness probe fail)
3. Calls `http.Server.Shutdown` with a 15-second timeout
4. Exits cleanly

This matters for blue-green: when the old (blue) pods scale down after promotion, active connections drain without errors.

## Local dev

```bash
cd app
go test -v ./...
go run -ldflags="-X main.version=local" .

curl -s http://localhost:8080/            # 200 JSON
curl -si http://localhost:8080/nope       # 404
curl -si -X POST http://localhost:8080/   # 405
curl -si "http://localhost:8080/slow?ms=300&jitter"
curl -si "http://localhost:8080/fail?code=503"
```

## Deploy to k3s

```bash
# First time: install Argo Rollouts controller
kubectl create namespace argo-rollouts
kubectl apply -n argo-rollouts -f https://github.com/argoproj/argo-rollouts/releases/latest/download/install.yaml

# Apply manifests
kubectl apply -f k8s/namespace.yaml
kubectl apply -f k8s/analysis-template.yaml
kubectl apply -f k8s/services.yaml
kubectl apply -f k8s/ingress.yaml
kubectl apply -f k8s/rollout.yaml

# Watch the rollout
kubectl argo rollouts get rollout demo-app -n blue-green-demo --watch
```

## Rollback

```bash
kubectl argo rollouts undo demo-app -n blue-green-demo
```

Argo Rollouts keeps the previous ReplicaSet scaled down (not deleted) for 30 seconds via `scaleDownDelaySeconds`. During that window, rollback is instant (no image pull, no pod scheduling). After the window, rollback still works but requires a new pod to schedule.

## Follow-ups (v2)

- Wire this repo into an ArgoCD Application for GitOps sync (currently `kubectl apply` by hand)
- Slack/Telegram notification on promotion and rollback
- Progressive delivery variant: canary with traffic splitting instead of blue-green
- Load test as AnalysisTemplate (replace HTTP smoke test with Locust/k6)
