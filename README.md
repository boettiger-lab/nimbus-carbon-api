# nimbus-carbon-api

A lightweight Go service that estimates the carbon footprint of LLM
inference on `nimbus`, a single GB10 DGX Spark node
(see [boettiger-lab/k8s/vllm/nimbus](https://github.com/boettiger-lab/k8s/tree/main/vllm/nimbus)).

Based on [nrp-carbon-api](https://github.com/boettiger-lab/nrp-carbon-api)
(carbon tracking for the shared NRP Nautilus cluster), adapted for a single
fixed-location, single-GPU node: no multi-institution grid-intensity lookup,
no multi-GPU-per-pod accounting — just one GB10, one grid location
(Berkeley, CA / CAMX), and whichever model is currently deployed.

## How it works

1. **GPU power** is read from [NVIDIA DCGM Exporter](https://github.com/NVIDIA/dcgm-exporter)
   metrics, collected by nimbus's own Prometheus
   (see [boettiger-lab/k8s/monitoring](https://github.com/boettiger-lab/k8s/tree/main/monitoring)).
2. **Token throughput** is read from vLLM's built-in Prometheus metrics.
3. **Grid carbon intensity** is a fixed constant for Berkeley, CA (CAMX
   eGRID 2022 subregion, 0.198 kg CO2/kWh) — see `internal/carbon/intensity.go`.
4. **Live engine activity** — running/queued requests, KV cache usage, GPU
   utilization, request throughput, and speculative-decoding (MTP)
   acceptance rate — is read directly from vLLM's and DCGM's own Prometheus
   gauges (not derived from carbon math) and shown on a companion "Live
   Activity" card next to each model's carbon card.

Carbon = Energy × Grid Intensity. See the
[Methodology](https://carbon-nimbus.carlboettiger.info/methodology) page for
full details.

## Running locally

```bash
export PROMETHEUS_URL=http://prometheus-server.monitoring.svc.cluster.local
go run ./cmd
# → http://localhost:8080
```

## Deploying

Pushing to `main` (touching `cmd/`, `internal/`, `go.mod`, or `Dockerfile`)
triggers [`.github/workflows/docker.yml`](.github/workflows/docker.yml),
which builds and pushes a multi-arch image to
`ghcr.io/boettiger-lab/nimbus-carbon-api:latest`. Once that completes, pick
it up on the cluster:

```bash
kubectl apply -f k8s/deployment.yaml   # first time only, or on manifest changes
kubectl rollout restart deployment/nimbus-carbon-api
```

To build and push manually instead (e.g. for a one-off image):

```bash
docker build -t ghcr.io/boettiger-lab/nimbus-carbon-api:latest .
docker push ghcr.io/boettiger-lab/nimbus-carbon-api:latest
```

## API

| Endpoint | Description |
|---|---|
| `GET /api/v1/carbon` | Current metrics for the active model |
| `GET /api/v1/carbon/timeseries?range=24h\|7d\|30d` | CO2 and power time series |
| `GET /api/v1/carbon/{ns}/{container}/{metric}?range=...` | Per-model time series (`power_watts`, `co2_grams_per_hour`, `co2_mg_per_token`) — useful for comparing models tried over time on the same hardware |
| `GET /healthz` | Health check |

## License

[BSD 2-Clause](LICENSE)
