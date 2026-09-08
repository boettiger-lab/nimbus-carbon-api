# nimbus-carbon-api

A lightweight Go service that estimates the carbon footprint of LLM inference
across the GPU nodes of the lab's Kubernetes cluster
(see [boettiger-lab/k8s](https://github.com/boettiger-lab/k8s)) — one row per
node, one deployment for the whole cluster.

Named for `nimbus`, the GB10 DGX Spark it was originally written for; it now
also covers `cirrus` (2× Quadro RTX 8000) and any other node listed in its
config.

Based on [nrp-carbon-api](https://github.com/boettiger-lab/nrp-carbon-api)
(carbon tracking for the shared NRP Nautilus cluster), adapted for a handful of
machines in one building: no multi-institution grid-intensity lookup — one grid
location (Berkeley, CA / CAMX), and whichever model each node currently serves.

## Configuration

Nodes are declared as a JSON array, either inline in `NODES_JSON` or in a file
named by `NODES_FILE` (a mounted ConfigMap on the cluster):

```json
[
  {"name": "cirrus", "namespace": "vllm", "gpu_hardware": "Quadro RTX 8000",
   "gpu_count": 2, "node_power": true},
  {"name": "nimbus", "namespace": "vllm", "gpu_hardware": "NVIDIA GB10",
   "gpu_count": 1, "node_power": true}
]
```

| Field | Meaning |
|---|---|
| `name` | Kubernetes node name — matched against the `node` label on vLLM metrics and DCGM's `Hostname` |
| `namespace` | namespace the vLLM workload runs in (default `vllm`) |
| `gpu_hardware`, `gpu_count` | display strings for the card |
| `container` | serving container name, display only (default `vllm`) |
| `node_power` | attribute TOTAL node GPU power to this model — an upper bound, flagged as `power_is_node_total` |

**Both `name` and `namespace` are matched on every vLLM query.** Namespace alone
is not enough: cirrus and nimbus both serve out of the `vllm` namespace, so a
namespace-only query sums one node's tokens into the other's row while the power
stays node-scoped, quietly understating CO2 per token.

Set `node_power` whenever a node's GPUs are shared with anything else. It is
also the only mode that works when DCGM's pod-resources mapping attributes a GPU
to some other tenant — on nimbus the GB10's watts are reported under an MCP pod
in `default`, not under vLLM, so a namespace-scoped power query there returns
nothing at all.

The legacy single-node environment variables (`NODE_NAME`, `NAMESPACE`,
`GPU_HARDWARE`, `GPU_COUNT`, `CONTAINER`, `NODE_POWER`) still describe exactly
one node and continue to work.

## How it works

1. **GPU power** is read from [NVIDIA DCGM Exporter](https://github.com/NVIDIA/dcgm-exporter)
   metrics, collected by the cluster's Prometheus
   (see [boettiger-lab/k8s/platform/monitoring](https://github.com/boettiger-lab/k8s/tree/main/platform/monitoring)),
   summed per node.
2. **Token throughput** is read from vLLM's built-in Prometheus metrics.
3. **Grid carbon intensity** is a fixed constant for Berkeley, CA (CAMX
   eGRID 2022 subregion, 0.198 kg CO2/kWh) — see `internal/carbon/intensity.go`.
4. **Live engine activity** — running/queued requests, KV cache usage, GPU
   utilization, request throughput, and speculative-decoding (MTP)
   acceptance rate — is read directly from vLLM's and DCGM's own Prometheus
   gauges (not derived from carbon math) and shown on a companion "Live
   Activity" card next to each model's carbon card.

Carbon = Energy × Grid Intensity. See the
[Methodology](https://carbon.carlboettiger.info/methodology) page for
full details.

## Running locally

```bash
export PROMETHEUS_URL=http://localhost:9090   # kubectl -n monitoring port-forward svc/prometheus-server 9090:80
export NODES_JSON='[{"name":"cirrus","namespace":"vllm","gpu_count":2,"node_power":true}]'
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
| `GET /api/v1/carbon` | Current metrics, one entry per configured node |
| `GET /api/v1/carbon/timeseries?range=24h\|7d\|30d` | Cluster-wide CO2 and power time series (all nodes summed) |
| `GET /api/v1/carbon/{node}/{container}/{metric}?range=...` | Per-node time series (`power_watts`, `co2_grams_per_hour`, `co2_mg_per_token`) — useful for comparing models tried over time on the same hardware. A namespace is still accepted in place of the node when only one node serves it |
| `GET /healthz` | Health check |

## License

[BSD 2-Clause](LICENSE)
