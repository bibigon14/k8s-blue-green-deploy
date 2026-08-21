# k8s-blue-green-deploy

Blue-green deployment pipeline on Kubernetes using Argo Rollouts, running on a Raspberry Pi 5 homelab k3s cluster.

Push to `main` triggers: lint -> test -> build multi-arch image -> push to GHCR -> ArgoCD syncs -> Argo Rollouts creates preview -> smoke test -> auto-promote -> old version scales down.

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

- Go 1.23 (stdlib only, zero dependencies)
- Multi-stage Dockerfile with `distroless/static:nonroot` runtime
- Multi-arch build: `linux/amd64` + `linux/arm64` via `docker buildx`
- GHCR (GitHub Container Registry) - free, no cloud registry cost
- Argo Rollouts blue-green strategy with `prePromotionAnalysis`
- ArgoCD for GitOps sync (already running on the cluster)
- Traefik ingress with wildcard TLS
- k3s on Raspberry Pi 5 (8GB)

## Endpoints

| Path | Purpose |
|---|---|
| `GET /` | JSON with version, color, hostname, timestamp |
| `GET /healthz` | Liveness probe (always 200) |
| `GET /ready` | Readiness probe (503 during startup/shutdown, 200 when ready) |

## CI pipeline

| Trigger | Steps |
|---|---|
| PR to `main` | lint -> test |
| Push to `main` | lint -> test -> build -> push to GHCR (multi-arch) |
| Manual dispatch | promote preview to active via `kubectl argo rollouts promote` |

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
curl http://localhost:8080/
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

- Grafana dashboard for rollout metrics (promotion latency, rollback count)
- Slack/Telegram notification on promotion and rollback
- Progressive delivery variant: canary with traffic splitting instead of blue-green
- Load test as AnalysisTemplate (replace HTTP smoke test with Locust/k6)
