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
  {"name": "nimbus2", "gpu_hardware": "NVIDIA GB10"}
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
`model@node` when a name is reused on two nodes for different checkpoints:

```json
{"qwen@nimbus": {"display_name": "Qwen3.8-Flash-Next NVFP4",
                 "description": "MoE, MTP speculative decoding"},
 "deepseek-v4-flash@nimbus2": {"display_name": "DeepSeek-V4-Flash-DSpark",
                 "power_hosts": ["nimbus2", "nimbus4"], "gpu_count": 2}}
```

A `model@node` entry may also carry **`power_hosts` and `gpu_count`**, overriding the
node's for that one model. Use it for a tensor-parallel model whose node later serves
something smaller. On the node, the extra rank would be charged to every later model
on it. Remove it, and the pair's history loses that rank's energy, because history is
recomputed from Prometheus on every scrape. On the model, both stay right for as long
as Prometheus retains the series. The override must include all of the node's own
hosts (it can add hosts, not drop them).

A host it adds may be one of two kinds:

- **A host no node owns** (`nimbus4`, only ever a TP worker): its power goes to the
  model at every minute the model is serving.
- **Another node's own host** (`nimbus3`, which serves its own models in rotation and
  is also the worker rank of TP2 pairs headed on `nimbus`): its power goes to the model
  only at minutes when the model is serving **and no model is serving on that host's
  own node**. At any other minute it stays with its own node, as it would without the
  override. The worker exports no vLLM metrics, so "nothing serving on nimbus3" is the
  only sign that it is busy as a worker. Without the claim, nimbus3's watts during a
  pair are counted nowhere.

```json
{"glm-5.3-flash@nimbus": {"display_name": "GLM-5.3-Flash",
                          "power_hosts": ["nimbus", "nimbus3"], "gpu_count": 2}}
```

In PromQL, the second kind is its own term of the power join, gated with
`unless on () vllm:num_requests_running{namespace="vllm",node="nimbus3"}`. The gate
uses serving, not the 5 tok/s "active" test, so active energy splits the same way.
Utilisation still averages over all of the model's hosts. Models on the same node may
claim the same host (only one serves there at a time; `glm-5.3-flash@nimbus` and later
`mimo-v2.6-flash@nimbus` can both list `nimbus3`). Models on two different nodes may
not, because both could serve at once. Startup refuses a config that would count a host
twice.

### One card per model

The dashboard shows one card per **model**, not per node. Every `model@node` the
service discovers gets a card key: its `display_name` from `MODELS_FILE` (an exact
`model@node` entry is preferred over the bare name), or, with none, the served model
name. All `model@node`s with the same key are one card. The same served name on
`nimbus` and `nimbus3` is one card. A `model@node` entry with a display name of its own
(a different checkpoint behind a reused served name) stays a separate card. Within a
card:

- energy, CO₂, tokens, requests, serving and active hours are summed, and every mean
  and ratio (mean power, J/token, CO₂/token, tokens per request, decode speed,
  prefix-cache and spec-decode rates) is recomputed from the sums. `covered_hours` is
  the window and is not summed, so uptime is the card's serving time over the window
  (capped at 100% if two nodes served at once);
- peak concurrency is the larger member's peak;
- latency percentiles are exact. The service fetches each histogram's buckets per
  `model@node` and sums them per card, `le` by `le`. It then interpolates them with
  Prometheus's own `histogram_quantile` algorithm. This runs in Go, not PromQL, because
  which series form a card is decided by `MODELS_FILE`;
- live state comes from the members serving now. If two are serving at once, their
  rates and power add, KV cache shows the fuller one, and GPU utilisation is averaged;
- the availability strip takes the higher state at each step; first seen is the
  earliest, last seen the latest.

The dashboard shows hardware (`gpu_count`× `gpu_hardware`, plus "tensor-parallel"
when the model spans several `power_hosts`), never node names: which interchangeable
GB10 served a model is of no interest. The node fields stay in the API.

`node`, `power_hosts`, `gpu_count` and `gpu_hardware` describe the card's primary
member: the one serving now, else the one seen most recently. `nodes` and `model_names`
list every member. `id` is the card key when that is URL-safe, else a slug of it plus a
short hash, so it is stable across restarts.

The legacy single-node environment variables (`NODE_NAME`, `NAMESPACE`,
`GPU_HARDWARE`, `GPU_COUNT`, `CONTAINER`) still describe exactly one node.

## How it works

1. **Models** are discovered from vLLM's own metrics: anything exporting
   `vllm:num_requests_running` on a configured node is a model, measured per
   `model_name@node` and shown one card per model (see above). Nothing is
   listed by hand, and a model scaled to zero keeps its card for as long as
   Prometheus (15 days here) retains its series.
2. **GPU power** comes from [NVIDIA DCGM Exporter](https://github.com/NVIDIA/dcgm-exporter),
   summed per node over its `power_hosts`, and joined onto whichever model is
   serving on that node at each minute. A model's own `power_hosts` override
   adds its extra hosts' power to that model alone, under the same join (a host
   that is another node's own only while that node serves nothing). GPU time
   spent serving no model stays out of every total.
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
| `GET /api/v1/models` | One record per model (card): `id`, `node` and `nodes`, `status` (`generating`/`idle`/`offline`), `live` (null when offline), `aggregates` per window, `availability` strip per window, first/last seen. `/api/v1/carbon` is an alias |
| `GET /api/v1/models/{id}/timeseries?range=3h\|24h\|7d\|15d` | One card's charts over time (the dashboard's drill-down); `{id}` is a card `id` from `/api/v1/models`, `range` defaults to `24h`. See below |
| `GET /api/v1/carbon/timeseries?range=24h\|7d\|15d` | Cluster-wide LLM power and CO2 over time (attributed power only) |
| `GET /healthz` | Health check |

### Per-model time series

`GET /api/v1/models/{id}/timeseries?range=24h` returns:

```json
{
  "id": "GLM-5.3-Flash-FP4-MTP-k-5-b12x-image-TP2-b0092d58",
  "range": "24h", "start": 1795203360, "end": 1795289760,
  "step_seconds": 120, "window": "5m",
  "metrics": {
    "power_watts":                [{"label": "GPU power", "points": [[1795289640, 84.1], ...]}],
    "co2_grams_per_hour":         [{"label": "CO₂", "points": [...]}],
    "tokens_per_sec":             [{"label": "Generation", ...}, {"label": "Prompt", ...}],
    "joules_per_token":           [{"label": "J/token", ...}],
    "requests":                   [{"label": "Running", ...}, {"label": "Waiting", ...}],
    "kv_cache_percent":           [{"label": "KV cache", ...}],
    "spec_decode_accept_percent": [{"label": "Accepted", ...}],
    "prefix_cache_hit_percent":   [{"label": "Hit rate", ...}],
    "latency_seconds":            [{"label": "E2E p50", ...}, {"label": "E2E p95", ...},
                                   {"label": "E2E p99", ...}, {"label": "TTFT p50", ...}]
  },
  "errors": {"<query>": "<message>"}
}
```

Points are `[unix seconds, value]`, oldest first. A step at which the model was not
serving has no point (not a zero). Every chart name is always present; `errors`
appears only if a query failed. An unknown id is a 404, an unknown range a 400.

| Range | Step | Rate / average window |
|---|---|---|
| `3h` | 30 s | 2 m |
| `24h` | 2 m | 5 m |
| `7d` | 15 m | 15 m |
| `15d` | 1 h | 1 h |

It is stateless: 12 Prometheus range queries per request, one per metric, run
concurrently. Each is one of the card queries' own builders (`attributedPower`,
`rate`, `buckets`, ...) evaluated over the config's full node set, so power is
attributed exactly as on the cards, gated pair-host claims included. The card's
`model@node` members are then picked out of the results and combined as the card
combines them: power, token rates and requests add; KV cache takes the fuller
member; prefix-cache and spec-decode rates are recomputed from summed parts;
latency percentiles come from the members' buckets summed `le` by `le`. CO₂ is
power × the grid intensity. J/token is power over prompt + generated tokens/s,
shown only above 5 tok/s, as on the live card. Requests are averaged over the
window (mean concurrency). Unlike the cards, chart percentiles are not withheld
below 20 requests: one point's window may hold only a few.

## License

[BSD 2-Clause](LICENSE)
