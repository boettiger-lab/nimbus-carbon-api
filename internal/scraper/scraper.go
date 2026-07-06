package scraper

import (
	"log"
	"math"
	"sync"
	"time"

	"github.com/boettiger-lab/nimbus-carbon-api/internal/carbon"
	"github.com/boettiger-lab/nimbus-carbon-api/internal/prom"
)

// nimbus is a single node with exactly one physical GPU (a GB10) and one
// serving container ("vllm", shared by every model deployment — see
// boettiger-lab/k8s/vllm/nimbus/deploy-*.yaml). These are fixed facts, not
// queried, unlike nrp-carbon-api which discovers GPU count/hardware and
// container name per pod across many nodes.
const (
	nimbusNamespace   = "default"
	nimbusGPUCount    = 1
	nimbusGPUHardware = "NVIDIA GB10"
	nimbusContainer   = "vllm"
)

// ModelMetrics holds the latest carbon and performance metrics for the
// currently-active model on nimbus.
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
	GenerationTokensPerSec float64 `json:"generation_tokens_per_sec"` // output (decode) token rate
	TokensPerSec           float64 `json:"tokens_per_sec"`            // total = prompt + generation

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

const maxBuckets = 168    // 7 days of hourly buckets
const maxHistory = 20160  // 7 days at 30s scrape intervals (for Series endpoint ring buffers)

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

// Scraper polls Prometheus and maintains in-memory state. Keyed by
// namespace alone (always "default" on nimbus) — see package comment.
type Scraper struct {
	client   *prom.Client
	interval time.Duration

	mu      sync.RWMutex
	models  map[string]*ModelMetrics
	history map[string]*modelHistory
}

func New(promURL string, interval time.Duration) *Scraper {
	return &Scraper{
		client:   prom.NewClient(promURL, 30*time.Second),
		interval: interval,
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

// backfill queries Prometheus for 7 days of historical power and token data
// and seeds the hourly average buckets so that 24h/7d averages are
// immediately correct after a restart, rather than starting from zero.
func (s *Scraper) backfill() {
	log.Println("scraper: backfilling 7-day averages from Prometheus...")
	end := time.Now()
	start := end.Add(-7 * 24 * time.Hour)
	step := 5 * time.Minute

	powerSeries, err := s.client.RangeQuery(
		`sum by (namespace) (DCGM_FI_DEV_POWER_USAGE{namespace="default"})`,
		start, end, step,
	)
	if err != nil {
		log.Printf("scraper: backfill power query failed: %v", err)
		return
	}
	promptSeries, err := s.client.RangeQuery(
		`sum by (namespace) (rate(vllm:prompt_tokens_total{namespace="default"}[5m]))`,
		start, end, step,
	)
	if err != nil {
		log.Printf("scraper: backfill prompt token query failed: %v", err)
		return
	}
	genSeries, err := s.client.RangeQuery(
		`sum by (namespace) (rate(vllm:generation_tokens_total{namespace="default"}[5m]))`,
		start, end, step,
	)
	if err != nil {
		log.Printf("scraper: backfill generation token query failed: %v", err)
		return
	}

	type sample struct{ power, promptTok, genTok float64 }
	byKeyTime := make(map[string]map[int64]*sample)

	for _, sr := range powerSeries {
		key := sr.Metric["namespace"]
		if byKeyTime[key] == nil {
			byKeyTime[key] = make(map[int64]*sample)
		}
		for _, pt := range sr.Points {
			ts := pt.Time.Unix()
			if byKeyTime[key][ts] == nil {
				byKeyTime[key][ts] = &sample{}
			}
			byKeyTime[key][ts].power += pt.Value
		}
	}
	for _, sr := range promptSeries {
		key := sr.Metric["namespace"]
		if byKeyTime[key] == nil {
			byKeyTime[key] = make(map[int64]*sample)
		}
		for _, pt := range sr.Points {
			ts := pt.Time.Unix()
			if byKeyTime[key][ts] == nil {
				byKeyTime[key][ts] = &sample{}
			}
			byKeyTime[key][ts].promptTok += pt.Value
		}
	}
	for _, sr := range genSeries {
		key := sr.Metric["namespace"]
		if byKeyTime[key] == nil {
			byKeyTime[key] = make(map[int64]*sample)
		}
		for _, pt := range sr.Points {
			ts := pt.Time.Unix()
			if byKeyTime[key][ts] == nil {
				byKeyTime[key][ts] = &sample{}
			}
			byKeyTime[key][ts].genTok += pt.Value
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

// Series returns the history for a namespace/metric combination.
// container is accepted for API-compatibility with the
// /api/v1/carbon/{ns}/{container}/{metric} route but is otherwise unused —
// nimbus has exactly one container ("vllm") per namespace, always.
// metric is one of "power_watts", "co2_grams_per_hour", "co2_mg_per_token".
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
			Container:              nimbusContainer,
			GPUHardware:            nimbusGPUHardware,
			Node:                   "nimbus",
			GPUCount:               nimbusGPUCount,
			PowerWatts:             math.Round(power*10) / 10,
			PromptTokensPerSec:     math.Round(promptTok*10) / 10,
			GenerationTokensPerSec: math.Round(genTok*10) / 10,
			TokensPerSec:           math.Round(totalTok*10) / 10,
			CarbonIntensity:        intensity,
			CO2GramsPerHour:        math.Round(co2PerHour*10) / 10,
			UpdatedAt:              now,
		}
		if co2PerToken > 0 {
			m.CO2MgPerToken = math.Round(co2PerToken*1000) / 1000
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
			// powerAvg24h is 0 only when no samples fell in the 24h window
			// (addSample never records a sample with power <= 0), so this
			// single check gates all three time-weighted means together.
			m.PowerWattsAvg24h = powerAvg24h
			m.PromptTokensPerSecAvg24h = promptAvg24h
			m.GenerationTokensPerSecAvg24h = genAvg24h
		}
	}
}

// average24h7d computes the 24h/7d token-weighted CO₂/token means (from
// active samples only) and the 24h time-weighted power/prompt/gen-token
// means (from every reporting sample), given the current time and a
// model's accumulated hourly buckets.
//
// avgBucket.PowerSum/PromptTokSum/GenTokSum are sums over every individual
// sample within that hour (accumulated by addSample), so the 24h means must
// divide by the total number of samples across the touched buckets
// (SampleCount), not by the number of buckets touched -- otherwise the
// result scales with samples-per-hour instead of being a true per-sample
// mean.
//
// Each return value is 0 when it could not be computed (no data in the
// relevant window), matching how the corresponding ModelMetrics fields are
// already zero-valued with `omitempty` JSON tags when not computed.
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

// queryPower returns total GPU power (W) keyed by namespace.
func (s *Scraper) queryPower() (map[string]float64, error) {
	results, err := s.client.Query(
		`sum by (namespace) (avg_over_time(DCGM_FI_DEV_POWER_USAGE{namespace="default"}[5m]))`,
	)
	if err != nil {
		return nil, err
	}

	power := make(map[string]float64)
	for _, r := range results {
		power[r.Metric["namespace"]] += r.Value
	}
	return power, nil
}

// queryTokens returns 2-minute prompt and generation token rates keyed by
// namespace, plus the vLLM model_name label for whichever model is
// currently reporting traffic.
func (s *Scraper) queryTokens() (genTokens, promptTokens map[string]float64, names map[string]string, err error) {
	// [2m], not [5m]: nimbus's traffic is bursty single-user generation, not
	// steady multi-tenant load. A 5-minute window dilutes a 20-second burst
	// across 280 seconds of surrounding idle time and goes fully stale within
	// minutes of the burst ending. [2m] is still >=4x the 30s scrape interval
	// (Prometheus's own recommended minimum for a stable rate() over a
	// counter) while tracking real bursts far more closely. Confirmed live:
	// a real burst measured via raw counter deltas ran 30-133 tok/s, while
	// rate(...[5m]) read 0-5 tok/s during and shortly after it.
	genResults, err := s.client.Query(
		`sum by (namespace, model_name) (rate(vllm:generation_tokens_total{namespace="default"}[2m]))`,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	promptResults, err := s.client.Query(
		`sum by (namespace, model_name) (rate(vllm:prompt_tokens_total{namespace="default"}[2m]))`,
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

	powerSeries, err := s.client.RangeQuery(
		`sum by (namespace) (DCGM_FI_DEV_POWER_USAGE{namespace="default"})`,
		start, end, step,
	)
	if err != nil {
		return nil, err
	}
	tokenSeries, err := s.client.RangeQuery(
		`sum by (namespace) (rate(vllm:generation_tokens_total{namespace="default"}[5m]) + rate(vllm:prompt_tokens_total{namespace="default"}[5m]))`,
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
