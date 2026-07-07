package scraper

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/boettiger-lab/nimbus-carbon-api/internal/carbon"
	"github.com/boettiger-lab/nimbus-carbon-api/internal/prom"
)

// Config parameterizes the scraper for a specific node. Defaults reproduce
// the original nimbus behavior exactly (single GB10 in the "default"
// namespace, per-namespace power attribution), so the same image serves both
// nimbus and other nodes purely via environment overrides.
//
//   - nimbus: one GB10, one vllm pod in "default", DCGM power is already
//     attributed to that pod's namespace by the pod-resources mapping, so
//     power is queried per-namespace (NodePower=false).
//   - cirrus: two RTX 8000s time-sliced across several namespaces
//     (vllm/jupyter/mcp), so DCGM per-GPU power can't be split per tenant.
//     Set NodePower=true to attribute TOTAL node GPU power (both cards) to the
//     vLLM namespace — a deliberate upper bound, documented on the dashboard.
type Config struct {
	Namespace   string // vLLM namespace to read token/request metrics from
	NodeName    string // node display name + DCGM Hostname selector for node-scope power
	GPUHardware string // display string for the GPU model
	GPUCount    int    // number of physical GPUs on the node
	Container   string // serving container name (display only)
	NodePower   bool   // true => attribute total node GPU power to Namespace (shared-GPU node)
}

// DefaultConfig returns the original nimbus configuration.
func DefaultConfig() Config {
	return Config{
		Namespace:   "default",
		NodeName:    "nimbus",
		GPUHardware: "NVIDIA GB10",
		GPUCount:    1,
		Container:   "vllm",
		NodePower:   false,
	}
}

// ModelMetrics holds the latest carbon and performance metrics for the
// currently-active model.
type ModelMetrics struct {
	// Identity
	ModelName   string `json:"model_name"`
	Namespace   string `json:"namespace"`
	Container   string `json:"container"`
	GPUHardware string `json:"gpu_hardware"`
	Node        string `json:"node"`

	// Raw
	GPUCount               int     `json:"gpu_count"`
	PowerWatts             float64 `json:"power_watts"`
	PromptTokensPerSec     float64 `json:"prompt_tokens_per_sec"`     // input (prefill) token rate
	GenerationTokensPerSec float64 `json:"generation_tokens_per_sec"` // output throughput over wall-clock (incl. idle gaps)
	TokensPerSec           float64 `json:"tokens_per_sec"`            // total throughput = prompt + generation
	// DecodeTokensPerSec is the ACTUAL generation speed while generating —
	// the inverse of vLLM's mean inter-token latency, so it is not diluted by
	// idle time between requests (unlike the throughput rates above). This is
	// the "N tok/s" figure people usually quote. Omitted when idle.
	DecodeTokensPerSec float64 `json:"decode_tokens_per_sec,omitempty"`

	// Carbon
	CarbonIntensity     float64 `json:"carbon_intensity_kg_per_kwh"`
	CO2GramsPerHour     float64 `json:"co2_grams_per_hour"`
	CO2MgPerToken       float64 `json:"co2_mg_per_token,omitempty"`         // 0 when idle (5-min window, ≥5 tok/s)
	CO2MgPerTokenAvg24h float64 `json:"co2_mg_per_token_avg_24h,omitempty"` // token-weighted 24h mean, active periods only
	CO2MgPerTokenAvg7d  float64 `json:"co2_mg_per_token_avg_7d,omitempty"`  // token-weighted 7-day mean, active periods only

	// Time-weighted 24h means (all samples, active + idle).
	PowerWattsAvg24h             float64 `json:"power_watts_avg_24h,omitempty"`
	PromptTokensPerSecAvg24h     float64 `json:"prompt_tokens_per_sec_avg_24h,omitempty"`
	GenerationTokensPerSecAvg24h float64 `json:"generation_tokens_per_sec_avg_24h,omitempty"`

	// Live engine activity, read directly from vLLM/DCGM.
	NumRequestsRunning float64 `json:"num_requests_running"`
	NumRequestsWaiting float64 `json:"num_requests_waiting"`
	KVCacheUsagePerc   float64 `json:"kv_cache_usage_percent"`
	GPUUtilPerc        float64 `json:"gpu_util_percent"`
	RequestsPerHour    float64 `json:"requests_per_hour"`

	// MTPAcceptancePerc is the speculative-decoding (MTP) draft-token
	// acceptance rate. Omitted entirely (not zero) for models that don't run
	// speculative decoding.
	MTPAcceptancePerc float64 `json:"mtp_acceptance_percent,omitempty"`

	// PowerIsNodeTotal flags that PowerWatts (and derived CO2) is the whole
	// node's GPU power, not this model's isolated draw — true on shared,
	// time-sliced GPU nodes where per-tenant power can't be measured.
	PowerIsNodeTotal bool `json:"power_is_node_total,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
}

// History is a fixed-size ring buffer of (time, value) pairs per metric.
type dataPoint struct {
	T time.Time
	V float64
}

// avgBucket holds one hour of aggregates: a token-weighted CO₂/token mean
// (active samples only) plus time-weighted means for power, prompt tok/s,
// and generation tok/s (every reporting sample, active or idle).
type avgBucket struct {
	Hour         int64
	WeightedSum  float64
	TokenSum     float64
	PowerSum     float64
	PromptTokSum float64
	GenTokSum    float64
	SampleCount  int
}

const maxBuckets = 168   // 7 days of hourly buckets
const maxHistory = 20160 // 7 days at 30s scrape intervals (for Series endpoint ring buffers)

type modelHistory struct {
	PowerWatts      []dataPoint
	CO2GramsPerHour []dataPoint
	CO2MgPerToken   []dataPoint
	AvgBuckets      []avgBucket
}

func (h *modelHistory) append(now time.Time, m *ModelMetrics) {
	push := func(buf *[]dataPoint, v float64) {
		*buf = append(*buf, dataPoint{T: now, V: v})
		if len(*buf) > maxHistory {
			*buf = (*buf)[len(*buf)-maxHistory:]
		}
	}
	push(&h.PowerWatts, m.PowerWatts)
	push(&h.CO2GramsPerHour, m.CO2GramsPerHour)
	if m.CO2MgPerToken > 0 {
		push(&h.CO2MgPerToken, m.CO2MgPerToken)
	}
	h.addSample(now, m.PowerWatts, m.PromptTokensPerSec, m.GenerationTokensPerSec, m.CarbonIntensity)
}

func (h *modelHistory) addSample(t time.Time, power, promptTok, genTok, intensity float64) {
	if power <= 0 {
		return
	}
	totalTok := promptTok + genTok
	var co2Weight, co2Tokens float64
	if totalTok > 5.0 {
		co2PerToken := carbon.MgPerToken(power, intensity, totalTok)
		co2Weight = co2PerToken * totalTok
		co2Tokens = totalTok
	}
	hourKey := t.Truncate(time.Hour).Unix()
	for i := len(h.AvgBuckets) - 1; i >= 0; i-- {
		if h.AvgBuckets[i].Hour == hourKey {
			b := &h.AvgBuckets[i]
			b.WeightedSum += co2Weight
			b.TokenSum += co2Tokens
			b.PowerSum += power
			b.PromptTokSum += promptTok
			b.GenTokSum += genTok
			b.SampleCount++
			return
		}
	}
	h.AvgBuckets = append(h.AvgBuckets, avgBucket{
		Hour:         hourKey,
		WeightedSum:  co2Weight,
		TokenSum:     co2Tokens,
		PowerSum:     power,
		PromptTokSum: promptTok,
		GenTokSum:    genTok,
		SampleCount:  1,
	})
	if len(h.AvgBuckets) > maxBuckets {
		h.AvgBuckets = h.AvgBuckets[len(h.AvgBuckets)-maxBuckets:]
	}
}

// Scraper polls Prometheus and maintains in-memory state, keyed by namespace.
type Scraper struct {
	client   *prom.Client
	interval time.Duration
	cfg      Config

	mu      sync.RWMutex
	models  map[string]*ModelMetrics
	history map[string]*modelHistory
}

// New builds a scraper with the default (nimbus) configuration.
func New(promURL string, interval time.Duration) *Scraper {
	return NewWithConfig(promURL, interval, DefaultConfig())
}

// NewWithConfig builds a scraper for an arbitrary node.
func NewWithConfig(promURL string, interval time.Duration, cfg Config) *Scraper {
	return &Scraper{
		client:   prom.NewClient(promURL, 30*time.Second),
		interval: interval,
		cfg:      cfg,
		models:   make(map[string]*ModelMetrics),
		history:  make(map[string]*modelHistory),
	}
}

func (s *Scraper) Run() {
	s.scrape()
	s.backfill()
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for range t.C {
		s.scrape()
	}
}

// powerKey maps a power series' namespace label to the key used for state.
// In node-power mode every watt is attributed to the configured vLLM
// namespace (the DCGM series has no meaningful/consistent namespace on a
// time-sliced GPU), so the power and token histories share one key.
func (s *Scraper) powerKey(ns string) string {
	if s.cfg.NodePower {
		return s.cfg.Namespace
	}
	return ns
}

// --- query builders (selectors depend on Config) ---

func (s *Scraper) powerInstantQuery() string {
	if s.cfg.NodePower {
		return fmt.Sprintf(`sum (avg_over_time(DCGM_FI_DEV_POWER_USAGE{Hostname=%q}[5m]))`, s.cfg.NodeName)
	}
	return fmt.Sprintf(`sum by (namespace) (avg_over_time(DCGM_FI_DEV_POWER_USAGE{namespace=%q}[5m]))`, s.cfg.Namespace)
}

func (s *Scraper) powerRangeQuery() string {
	if s.cfg.NodePower {
		return fmt.Sprintf(`sum (DCGM_FI_DEV_POWER_USAGE{Hostname=%q})`, s.cfg.NodeName)
	}
	return fmt.Sprintf(`sum by (namespace) (DCGM_FI_DEV_POWER_USAGE{namespace=%q})`, s.cfg.Namespace)
}

func (s *Scraper) utilQuery() string {
	if s.cfg.NodePower {
		return fmt.Sprintf(`avg (avg_over_time(DCGM_FI_DEV_GPU_UTIL{Hostname=%q}[5m]))`, s.cfg.NodeName)
	}
	return fmt.Sprintf(`avg by (namespace) (avg_over_time(DCGM_FI_DEV_GPU_UTIL{namespace=%q}[5m]))`, s.cfg.Namespace)
}

// backfill queries Prometheus for 7 days of historical power and token data
// and seeds the hourly average buckets so 24h/7d averages are immediately
// correct after a restart.
func (s *Scraper) backfill() {
	log.Println("scraper: backfilling 7-day averages from Prometheus...")
	end := time.Now()
	start := end.Add(-7 * 24 * time.Hour)
	step := 5 * time.Minute
	ns := s.cfg.Namespace

	powerSeries, err := s.client.RangeQuery(s.powerRangeQuery(), start, end, step)
	if err != nil {
		log.Printf("scraper: backfill power query failed: %v", err)
		return
	}
	promptSeries, err := s.client.RangeQuery(
		fmt.Sprintf(`sum by (namespace) (rate(vllm:prompt_tokens_total{namespace=%q}[5m]))`, ns),
		start, end, step,
	)
	if err != nil {
		log.Printf("scraper: backfill prompt token query failed: %v", err)
		return
	}
	genSeries, err := s.client.RangeQuery(
		fmt.Sprintf(`sum by (namespace) (rate(vllm:generation_tokens_total{namespace=%q}[5m]))`, ns),
		start, end, step,
	)
	if err != nil {
		log.Printf("scraper: backfill generation token query failed: %v", err)
		return
	}

	type sample struct{ power, promptTok, genTok float64 }
	byKeyTime := make(map[string]map[int64]*sample)
	ensure := func(key string) map[int64]*sample {
		if byKeyTime[key] == nil {
			byKeyTime[key] = make(map[int64]*sample)
		}
		return byKeyTime[key]
	}

	for _, sr := range powerSeries {
		m := ensure(s.powerKey(sr.Metric["namespace"]))
		for _, pt := range sr.Points {
			ts := pt.Time.Unix()
			if m[ts] == nil {
				m[ts] = &sample{}
			}
			m[ts].power += pt.Value
		}
	}
	for _, sr := range promptSeries {
		m := ensure(sr.Metric["namespace"])
		for _, pt := range sr.Points {
			ts := pt.Time.Unix()
			if m[ts] == nil {
				m[ts] = &sample{}
			}
			m[ts].promptTok += pt.Value
		}
	}
	for _, sr := range genSeries {
		m := ensure(sr.Metric["namespace"])
		for _, pt := range sr.Points {
			ts := pt.Time.Unix()
			if m[ts] == nil {
				m[ts] = &sample{}
			}
			m[ts].genTok += pt.Value
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for key, timestamps := range byKeyTime {
		if s.history[key] == nil {
			s.history[key] = &modelHistory{}
		}
		h := s.history[key]
		for ts, samp := range timestamps {
			if samp.power <= 0 {
				continue
			}
			h.addSample(time.Unix(ts, 0), samp.power, samp.promptTok, samp.genTok, carbon.BerkeleyIntensity)
		}
	}

	log.Printf("scraper: backfilled %d key(s) from Prometheus", len(byKeyTime))
}

// Models returns a snapshot of all current model metrics.
func (s *Scraper) Models() []*ModelMetrics {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ModelMetrics, 0, len(s.models))
	for _, m := range s.models {
		cp := *m
		out = append(out, &cp)
	}
	return out
}

// Series returns the history for a namespace/metric combination. container is
// accepted for API-compatibility with the route but is unused (one container
// per namespace). metric is one of "power_watts", "co2_grams_per_hour",
// "co2_mg_per_token".
func (s *Scraper) Series(namespace, container, metric string, since time.Duration) [][2]interface{} {
	_ = container
	s.mu.RLock()
	h, ok := s.history[namespace]
	s.mu.RUnlock()
	if !ok {
		return nil
	}

	cutoff := time.Now().Add(-since)
	var buf []dataPoint
	switch metric {
	case "power_watts":
		buf = h.PowerWatts
	case "co2_grams_per_hour":
		buf = h.CO2GramsPerHour
	case "co2_mg_per_token":
		buf = h.CO2MgPerToken
	default:
		return nil
	}

	var out [][2]interface{}
	for _, p := range buf {
		if p.T.After(cutoff) {
			out = append(out, [2]interface{}{p.T.Unix(), p.V})
		}
	}
	return out
}

// ---- internal ----

func (s *Scraper) scrape() {
	powerByKey, err := s.queryPower()
	if err != nil {
		log.Printf("scraper: power query failed: %v", err)
	}

	genTokensByKey, promptTokensByKey, modelNameByKey, err := s.queryTokens()
	if err != nil {
		log.Printf("scraper: token query failed: %v", err)
	}

	runningByKey, waitingByKey, kvCacheByKey, err := s.queryRequestStats()
	if err != nil {
		log.Printf("scraper: request stats query failed: %v", err)
	}

	gpuUtilByKey, err := s.queryGPUUtil()
	if err != nil {
		log.Printf("scraper: GPU util query failed: %v", err)
	}

	requestRateByKey, err := s.queryRequestRate()
	if err != nil {
		log.Printf("scraper: request rate query failed: %v", err)
	}

	mtpAcceptanceByKey, err := s.queryMTPAcceptance()
	if err != nil {
		log.Printf("scraper: MTP acceptance query failed: %v", err)
	}

	decodeSpeedByKey, err := s.queryDecodeSpeed()
	if err != nil {
		log.Printf("scraper: decode speed query failed: %v", err)
	}

	keys := make(map[string]struct{})
	for k := range powerByKey {
		keys[k] = struct{}{}
	}
	for k := range genTokensByKey {
		keys[k] = struct{}{}
	}
	for k := range promptTokensByKey {
		keys[k] = struct{}{}
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	for key := range keys {
		power := powerByKey[key]
		intensity := carbon.BerkeleyIntensity

		genTok := genTokensByKey[key]
		promptTok := promptTokensByKey[key]
		totalTok := genTok + promptTok
		modelName := modelNameByKey[key]

		co2PerHour := carbon.GramsPerHour(power, intensity)
		co2PerToken := 0.0
		if totalTok > 5.0 {
			co2PerToken = carbon.MgPerToken(power, intensity, totalTok)
		}

		m := &ModelMetrics{
			ModelName:              modelName,
			Namespace:              key,
			Container:              s.cfg.Container,
			GPUHardware:            s.cfg.GPUHardware,
			Node:                   s.cfg.NodeName,
			GPUCount:               s.cfg.GPUCount,
			PowerWatts:             math.Round(power*10) / 10,
			PromptTokensPerSec:     math.Round(promptTok*10) / 10,
			GenerationTokensPerSec: math.Round(genTok*10) / 10,
			TokensPerSec:           math.Round(totalTok*10) / 10,
			CarbonIntensity:        intensity,
			CO2GramsPerHour:        math.Round(co2PerHour*10) / 10,
			NumRequestsRunning:     runningByKey[key],
			NumRequestsWaiting:     waitingByKey[key],
			KVCacheUsagePerc:       math.Round(kvCacheByKey[key]*10) / 10,
			GPUUtilPerc:            math.Round(gpuUtilByKey[key]*10) / 10,
			RequestsPerHour:        math.Round(requestRateByKey[key]*10) / 10,
			PowerIsNodeTotal:       s.cfg.NodePower,
			UpdatedAt:              now,
		}
		if co2PerToken > 0 {
			m.CO2MgPerToken = math.Round(co2PerToken*1000) / 1000
		}
		if mtp, ok := mtpAcceptanceByKey[key]; ok {
			m.MTPAcceptancePerc = math.Round(mtp*10) / 10
		}
		if ds, ok := decodeSpeedByKey[key]; ok && ds > 0 {
			m.DecodeTokensPerSec = math.Round(ds*10) / 10
		}

		s.models[key] = m

		if s.history[key] == nil {
			s.history[key] = &modelHistory{}
		}
		h := s.history[key]
		h.append(now, m)

		co2Avg24h, co2Avg7d, powerAvg24h, promptAvg24h, genAvg24h := average24h7d(now, h.AvgBuckets)
		if co2Avg24h != 0 {
			m.CO2MgPerTokenAvg24h = co2Avg24h
		}
		if co2Avg7d != 0 {
			m.CO2MgPerTokenAvg7d = co2Avg7d
		}
		if powerAvg24h != 0 {
			m.PowerWattsAvg24h = powerAvg24h
			m.PromptTokensPerSecAvg24h = promptAvg24h
			m.GenerationTokensPerSecAvg24h = genAvg24h
		}
	}
}

// average24h7d computes the 24h/7d token-weighted CO₂/token means (active
// samples only) and the 24h time-weighted power/prompt/gen-token means (every
// reporting sample). Each return value is 0 when it could not be computed.
func average24h7d(now time.Time, buckets []avgBucket) (co2Avg24h, co2Avg7d, powerAvg24h, promptAvg24h, genAvg24h float64) {
	var wSum24, tSum24, wSum7d, tSum7d float64
	var powSum24, promptSum24, genSum24 float64
	var sampleSum24 int
	cutoff24h := now.Add(-24 * time.Hour).Truncate(time.Hour).Unix()
	cutoff7d := now.Add(-7 * 24 * time.Hour).Truncate(time.Hour).Unix()
	for _, b := range buckets {
		if b.Hour >= cutoff7d {
			wSum7d += b.WeightedSum
			tSum7d += b.TokenSum
		}
		if b.Hour >= cutoff24h {
			wSum24 += b.WeightedSum
			tSum24 += b.TokenSum
			powSum24 += b.PowerSum
			promptSum24 += b.PromptTokSum
			genSum24 += b.GenTokSum
			sampleSum24 += b.SampleCount
		}
	}
	if tSum24 > 0 {
		co2Avg24h = math.Round(wSum24/tSum24*1000) / 1000
	}
	if tSum7d > 0 {
		co2Avg7d = math.Round(wSum7d/tSum7d*1000) / 1000
	}
	if sampleSum24 > 0 {
		n := float64(sampleSum24)
		powerAvg24h = math.Round(powSum24/n*10) / 10
		promptAvg24h = math.Round(promptSum24/n*10) / 10
		genAvg24h = math.Round(genSum24/n*10) / 10
	}
	return
}

// queryPower returns GPU power (W) keyed by namespace (or, in node-power mode,
// total node power under the configured namespace key).
func (s *Scraper) queryPower() (map[string]float64, error) {
	results, err := s.client.Query(s.powerInstantQuery())
	if err != nil {
		return nil, err
	}
	power := make(map[string]float64)
	for _, r := range results {
		power[s.powerKey(r.Metric["namespace"])] += r.Value
	}
	return power, nil
}

// queryTokens returns 2-minute prompt and generation token rates keyed by
// namespace, plus the vLLM model_name label.
func (s *Scraper) queryTokens() (genTokens, promptTokens map[string]float64, names map[string]string, err error) {
	ns := s.cfg.Namespace
	genResults, err := s.client.Query(
		fmt.Sprintf(`sum by (namespace, model_name) (rate(vllm:generation_tokens_total{namespace=%q}[2m]))`, ns),
	)
	if err != nil {
		return nil, nil, nil, err
	}
	promptResults, err := s.client.Query(
		fmt.Sprintf(`sum by (namespace, model_name) (rate(vllm:prompt_tokens_total{namespace=%q}[2m]))`, ns),
	)
	if err != nil {
		return nil, nil, nil, err
	}

	genTokens = make(map[string]float64)
	promptTokens = make(map[string]float64)
	names = make(map[string]string)
	for _, r := range genResults {
		key := r.Metric["namespace"]
		genTokens[key] += r.Value
		if names[key] == "" {
			names[key] = r.Metric["model_name"]
		}
	}
	for _, r := range promptResults {
		key := r.Metric["namespace"]
		promptTokens[key] += r.Value
		if names[key] == "" {
			names[key] = r.Metric["model_name"]
		}
	}
	return genTokens, promptTokens, names, nil
}

// queryRequestStats returns vLLM's live engine-state gauges keyed by namespace.
func (s *Scraper) queryRequestStats() (running, waiting, kvCache map[string]float64, err error) {
	ns := s.cfg.Namespace
	runResults, err := s.client.Query(fmt.Sprintf(`sum by (namespace) (vllm:num_requests_running{namespace=%q})`, ns))
	if err != nil {
		return nil, nil, nil, err
	}
	waitResults, err := s.client.Query(fmt.Sprintf(`sum by (namespace) (vllm:num_requests_waiting{namespace=%q})`, ns))
	if err != nil {
		return nil, nil, nil, err
	}
	kvResults, err := s.client.Query(fmt.Sprintf(`avg by (namespace) (vllm:kv_cache_usage_perc{namespace=%q}) * 100`, ns))
	if err != nil {
		return nil, nil, nil, err
	}

	running = make(map[string]float64)
	waiting = make(map[string]float64)
	kvCache = make(map[string]float64)
	for _, r := range runResults {
		running[r.Metric["namespace"]] = r.Value
	}
	for _, r := range waitResults {
		waiting[r.Metric["namespace"]] = r.Value
	}
	for _, r := range kvResults {
		kvCache[r.Metric["namespace"]] = r.Value
	}
	return running, waiting, kvCache, nil
}

// queryGPUUtil returns average GPU compute utilization (%), attributed to the
// configured namespace key (node-wide in node-power mode).
func (s *Scraper) queryGPUUtil() (map[string]float64, error) {
	results, err := s.client.Query(s.utilQuery())
	if err != nil {
		return nil, err
	}
	util := make(map[string]float64)
	for _, r := range results {
		util[s.powerKey(r.Metric["namespace"])] = r.Value
	}
	return util, nil
}

// queryRequestRate returns completed requests per hour keyed by namespace.
func (s *Scraper) queryRequestRate() (map[string]float64, error) {
	results, err := s.client.Query(
		fmt.Sprintf(`sum by (namespace) (rate(vllm:request_success_total{namespace=%q}[15m])) * 3600`, s.cfg.Namespace),
	)
	if err != nil {
		return nil, err
	}
	rate := make(map[string]float64)
	for _, r := range results {
		rate[r.Metric["namespace"]] = r.Value
	}
	return rate, nil
}

// queryDecodeSpeed returns the actual generation speed (output tokens/sec
// while generating) keyed by namespace, computed as the inverse of vLLM's
// mean inter-token latency over a 2-minute window: rate(count)/rate(sum) of
// the inter_token_latency_seconds histogram. Unlike the wall-clock token
// rate, this excludes idle time between requests, so it reflects the real
// per-token decode rate (e.g. ~140 tok/s here) rather than a utilization
// average. Idle namespaces produce no series (0/0) and are simply absent.
func (s *Scraper) queryDecodeSpeed() (map[string]float64, error) {
	ns := s.cfg.Namespace
	results, err := s.client.Query(
		fmt.Sprintf(`sum by (namespace) (rate(vllm:inter_token_latency_seconds_count{namespace=%q}[2m]))
			/ sum by (namespace) (rate(vllm:inter_token_latency_seconds_sum{namespace=%q}[2m]))`, ns, ns),
	)
	if err != nil {
		return nil, err
	}
	speed := make(map[string]float64)
	for _, r := range results {
		if math.IsNaN(r.Value) || math.IsInf(r.Value, 0) {
			continue
		}
		speed[r.Metric["namespace"]] = r.Value
	}
	return speed, nil
}

// queryMTPAcceptance returns the speculative-decoding (MTP) draft-token
// acceptance rate (%) keyed by namespace.
func (s *Scraper) queryMTPAcceptance() (map[string]float64, error) {
	ns := s.cfg.Namespace
	results, err := s.client.Query(
		fmt.Sprintf(`100 * sum by (namespace) (rate(vllm:spec_decode_num_accepted_tokens_total{namespace=%q}[15m]))
			/ sum by (namespace) (rate(vllm:spec_decode_num_draft_tokens_total{namespace=%q}[15m]))`, ns, ns),
	)
	if err != nil {
		return nil, err
	}
	rate := make(map[string]float64)
	for _, r := range results {
		rate[r.Metric["namespace"]] = r.Value
	}
	return rate, nil
}

// ClusterTimePoint is one time-step of aggregated cluster-wide carbon data.
type ClusterTimePoint struct {
	Timestamp       int64   `json:"t"`
	PowerWatts      float64 `json:"power_watts"`
	CO2GramsPerHour float64 `json:"co2_grams_per_hour"`
	CO2MgPerToken   float64 `json:"co2_mg_per_token,omitempty"`
}

// ClusterTimeSeries queries Prometheus for historical power + token data and
// returns aggregated totals per time step, using the fixed Berkeley intensity.
func (s *Scraper) ClusterTimeSeries(rangeBack, step time.Duration) ([]ClusterTimePoint, error) {
	end := time.Now()
	start := end.Add(-rangeBack)
	ns := s.cfg.Namespace

	powerSeries, err := s.client.RangeQuery(s.powerRangeQuery(), start, end, step)
	if err != nil {
		return nil, err
	}
	tokenSeries, err := s.client.RangeQuery(
		fmt.Sprintf(`sum by (namespace) (rate(vllm:generation_tokens_total{namespace=%q}[5m]) + rate(vllm:prompt_tokens_total{namespace=%q}[5m]))`, ns, ns),
		start, end, step,
	)
	if err != nil {
		return nil, err
	}

	type agg struct{ power, co2, tokens float64 }
	byTime := make(map[int64]*agg)

	for _, sr := range powerSeries {
		for _, pt := range sr.Points {
			ts := pt.Time.Unix()
			if byTime[ts] == nil {
				byTime[ts] = &agg{}
			}
			byTime[ts].power += pt.Value
			byTime[ts].co2 += carbon.GramsPerHour(pt.Value, carbon.BerkeleyIntensity)
		}
	}
	for _, sr := range tokenSeries {
		for _, pt := range sr.Points {
			ts := pt.Time.Unix()
			if byTime[ts] == nil {
				byTime[ts] = &agg{}
			}
			byTime[ts].tokens += pt.Value
		}
	}

	out := make([]ClusterTimePoint, 0, len(byTime))
	for ts, a := range byTime {
		pt := ClusterTimePoint{
			Timestamp:       ts,
			PowerWatts:      math.Round(a.power*10) / 10,
			CO2GramsPerHour: math.Round(a.co2*10) / 10,
		}
		if a.tokens > 0.1 {
			pt.CO2MgPerToken = math.Round(a.co2/a.tokens/3.6*1000) / 1000
		}
		out = append(out, pt)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Timestamp < out[j-1].Timestamp; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}
