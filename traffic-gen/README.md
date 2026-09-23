# traffic-gen

A tiny long-lived HTTP client that keeps the [k8s-blue-green-deploy][repo]
Grafana dashboard populated 24/7, so a portfolio visitor sees live request
rate, error rate and a real p99 latency tail instead of a flat "No data"
panel.

The reference deployment runs it on an Oracle Cloud Ampere A1 VM (Ubuntu
24.04) that reaches the homelab Raspberry Pi 5 over a WireGuard tunnel. The
Pi's k3s NodePort `demo-app-metrics:30130` is reachable at
`http://10.0.0.1:30130` from the VM; the generator's own `/metrics` binds to
`10.0.0.4:9102` on the tunnel interface so the homelab Prometheus can scrape
it without exposing anything on the internet.

[repo]: https://github.com/bibigon14/k8s-blue-green-deploy

## Traffic shape

Each request is picked in three steps:

1. **Baseline traffic.** With probability `1 - slow-prob - error-prob`, hit
   a weighted-random route from a fixed table (`/`, `/healthz`, `/ready`,
   plus a small share of unknown paths that intentionally produce 404s).
2. **Latency tail.** With probability `slow-prob`, hit `/slow?ms=N&jitter`
   with `N` uniform in `[0, max-slow-ms]`. Adds a visible p95/p99 tail.
3. **Error mix.** With probability `error-prob`, hit `/fail?code=N` with the
   code sampled from `{500: 40, 502: 15, 503: 15, 504: 5, 404: 15, 429: 10}`
   by relative weight.

On top of the target RPS, a slow sinusoid modulates the actual rate by
`modulation-amplitude` (default 0.4, i.e. ±40 %) over `modulation-period-min`
(default 30 min). This is what turns the "Request rate by path" panel from
a flat line into a wave, without needing to correlate anything.

## Flags

| Flag                       | Default                    | Meaning                                          |
|----------------------------|----------------------------|--------------------------------------------------|
| `-target`                  | `http://10.0.0.1:30130`    | Base URL of the demo app                         |
| `-rps`                     | `3.0`                      | Steady-state target requests per second          |
| `-slow-prob`               | `0.10`                     | Chance of `/slow?ms=…`                           |
| `-max-slow-ms`             | `800`                      | Upper bound for the sampled sleep                |
| `-error-prob`              | `0.05`                     | Chance of `/fail?code=…`                         |
| `-timeout`                 | `10s`                      | Per-request HTTP timeout                         |
| `-metrics-listen`          | `127.0.0.1:9101`           | Prometheus `/metrics` listen address             |
| `-modulation-period-min`   | `30`                       | Full-cycle length of the diurnal sinusoid (0 = off) |
| `-modulation-amplitude`    | `0.4`                      | RPS modulation amplitude as a fraction of `-rps` |

The generator refuses to start if `slow-prob + error-prob > 1` or the
target scheme is not `http`/`https`.

## Metrics exposed by the generator

- `trafficgen_requests_total{outcome}` where `outcome` is one of
  `ok`, `http_error`, `transport_error`, `timeout`.
- `trafficgen_request_duration_seconds` (histogram, default buckets).
- `trafficgen_current_rps` (gauge, after modulation).
- `trafficgen_target_info{target}` (constant 1).

These are for observing the generator itself. A flat
`trafficgen_current_rps` with rising `transport_error` means the tunnel is
down; a flat `trafficgen_requests_total` means the generator crashed.

## Build

Cross-compile from a workstation (macOS or Linux) to the arm64 VM:

```
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
    go build -ldflags="-s -w" -o bin/traffic-gen-linux-arm64 ./traffic-gen
```

Copy the binary to `/usr/local/bin/traffic-gen` on the VM.

## Install as a systemd service

The unit at [`deploy/systemd/traffic-gen.service`](deploy/systemd/traffic-gen.service)
runs as a `DynamicUser`, drops all capabilities, restricts syscalls to
`@system-service`, and caps the process at 64 MiB and 64 tasks. Install:

```
sudo install -m 0755 bin/traffic-gen-linux-arm64 /usr/local/bin/traffic-gen
sudo install -m 0644 deploy/systemd/traffic-gen.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now traffic-gen.service
```

Verify:

```
systemctl status traffic-gen.service
journalctl -u traffic-gen.service -f
curl -s http://10.0.0.4:9102/metrics | grep '^trafficgen_'
```

## Wire into homelab Prometheus

Add a scrape job to `/etc/prometheus/prometheus.yml` on the Pi:

```yaml
scrape_configs:
  - job_name: traffic-gen
    scrape_interval: 15s
    static_configs:
      - targets: [10.0.0.4:9102]
        labels:
          app: traffic-gen
          instance: oci-sanjose
          transport: wireguard
```

Reload Prometheus with `curl -X POST http://localhost:9090/-/reload`.
