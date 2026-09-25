# nimbus-carbon-api

A lightweight Go service that estimates the carbon footprint of LLM inference
across the GPU nodes of the lab's Kubernetes cluster
(see [boettiger-lab/k8s](https://github.com/boettiger-lab/k8s)) — one card per
served model, live or aggregated over 24 h / 7 d / 15 d, one deployment for the
whole cluster.

Named for `nimbus`, the GB10 DGX Spark it was originally written for; it now
also covers `cirrus` (2× Quadro RTX 8000) and any other node listed in its
config.

Based on [nrp-carbon-api](https://github.com/boettiger-lab/nrp-carbon-api)
(carbon tracking for the shared NRP Nautilus cluster), adapted for a handful of
machines in one building: no multi-institution grid-intensity lookup — one grid
location (Berkeley, CA / CAMX).

## Configuration

Nodes are declared as a JSON array, either inline in `NODES_JSON` or in a file
named by `NODES_FILE` (a mounted ConfigMap on the cluster):

```json
[
  {"name": "cirrus",  "gpu_hardware": "Quadro RTX 8000", "gpu_count": 2},
  {"name": "nimbus",  "gpu_hardware": "NVIDIA GB10"},
  {"name": "nimbus2", "gpu_hardware": "NVIDIA GB10", "gpu_count": 2,
   "power_hosts": ["nimbus2", "nimbus4"]}
]
```

| Field | Meaning |
|---|---|
| `name` | Kubernetes node name — matched against the `node` label on vLLM metrics |
| `namespace` | namespace the vLLM workload runs in (default `vllm`) |
| `gpu_hardware`, `gpu_count` | display strings for the card; for a multi-node model, the total |
| `power_hosts` | DCGM `Hostname`s whose power belongs to models served from this node (default `[name]`). List every rank of a tensor-parallel model: only the head exports vLLM metrics, but every rank draws power. A host may be claimed by one node only |
| `container` | serving container name, display only (default `vllm`) |

`node_power` from older configs is accepted and ignored: power is **always** the
node total, attributed to a model only while that model is serving there. DCGM's
per-pod attribution is not usable here — it names whichever pod the
pod-resources mapping picked (on nimbus, an MCP pod in `default`).

**Both `name` and `namespace` are matched on every vLLM query.** Every model
serves out of the `vllm` namespace, so a namespace-only query would sum one
node's tokens into another's.

Optional display names come from `MODELS_FILE`, keyed by served model name, or
`model@node` when a name is reused on two nodes:

```json
{"qwen@nimbus": {"display_name": "Qwen3.8-Flash-Next NVFP4",
                 "description": "MoE, MTP speculative decoding"}}
```

The legacy single-node environment variables (`NODE_NAME`, `NAMESPACE`,
`GPU_HARDWARE`, `GPU_COUNT`, `CONTAINER`) still describe exactly one node.

## How it works

1. **Models** are discovered from vLLM's own metrics: anything exporting
   `vllm:num_requests_running` on a configured node is a model, keyed
   `model_name@node`. Nothing is listed by hand, and a model scaled to zero
   keeps its card for as long as Prometheus (15 days here) retains its series.
2. **GPU power** comes from [NVIDIA DCGM Exporter](https://github.com/NVIDIA/dcgm-exporter),
   summed per node over its `power_hosts`, and joined onto whichever model is
   serving on that node at each minute. GPU time spent serving no model stays
   out of every total.
3. **Tokens, latency and engine state** come from vLLM's Prometheus metrics:
   token counters, TTFT / end-to-end / queue-time histograms (percentiles
   withheld below 20 requests), KV-cache and prefix-cache use, and
   speculative-decoding acceptance.
4. **Grid carbon intensity** is a fixed constant for Berkeley, CA (CAMX
   eGRID 2022 subregion, 0.198 kg CO2/kWh) — see `internal/carbon/intensity.go`.
5. **Everything is computed from Prometheus** — live state every 30 s,
   aggregates every 5 min — so the service holds no history and a restart
   loses nothing.

Carbon = Energy × Grid Intensity. See the
[Methodology](https://carbon.carlboettiger.info/methodology) page for
full details.

## Running locally

```bash
export PROMETHEUS_URL=http://localhost:9090   # kubectl -n monitoring port-forward svc/prometheus-server 9090:80
export NODES_JSON='[{"name":"nimbus","gpu_hardware":"NVIDIA GB10"}]'
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
# the live manifest is platform/monitoring/carbon-api.yaml in boettiger-lab/k8s
kubectl -n monitoring rollout restart deployment/carbon-api
```

To build and push manually instead (e.g. for a one-off image):

```bash
docker build -t ghcr.io/boettiger-lab/nimbus-carbon-api:latest .
docker push ghcr.io/boettiger-lab/nimbus-carbon-api:latest
```

## API

| Endpoint | Description |
|---|---|
| `GET /api/v1/models` | One record per model: `status` (`generating`/`idle`/`offline`), `live` (null when offline), `aggregates` per window, `availability` strip per window, first/last seen. `/api/v1/carbon` is an alias |
| `GET /api/v1/carbon/timeseries?range=24h\|7d\|15d` | Cluster-wide LLM power and CO2 over time (attributed power only) |
| `GET /healthz` | Health check |

## License

[BSD 2-Clause](LICENSE)
