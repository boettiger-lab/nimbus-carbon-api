package scraper

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/boettiger-lab/nimbus-carbon-api/internal/carbon"
	"github.com/boettiger-lab/nimbus-carbon-api/internal/prom"
)

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

	// Active-only long-term means, so a visitor who loads the dashboard while
	// the model is idle still sees a meaningful estimate of how fast it runs
	// and how much power/traffic it handles when actually working, instead of
	// a blank or a wall-clock-diluted number. PowerWattsActive*/
	// PromptTokensPerSecActive* are gated the same way as CO2MgPerTokenAvg*
	// (totalTok > 5 = non-idle); DecodeTokensPerSecAvg* is gated on having had
	// a valid (non-idle) decode-speed reading at all. These are the fields the
	// dashboard's "24h Average" card uses end-to-end, so every number in that
	// card shares the same "while working" framing.
	PowerWattsActiveAvg24h             float64 `json:"power_watts_active_avg_24h,omitempty"`
	PowerWattsActiveAvg7d              float64 `json:"power_watts_active_avg_7d,omitempty"`
	PromptTokensPerSecActiveAvg24h     float64 `json:"prompt_tokens_per_sec_active_avg_24h,omitempty"`
	PromptTokensPerSecActiveAvg7d      float64 `json:"prompt_tokens_per_sec_active_avg_7d,omitempty"`
	GenerationTokensPerSecActiveAvg24h float64 `json:"generation_tokens_per_sec_active_avg_24h,omitempty"`
	GenerationTokensPerSecActiveAvg7d  float64 `json:"generation_tokens_per_sec_active_avg_7d,omitempty"`
	DecodeTokensPerSecAvg24h           float64 `json:"decode_tokens_per_sec_avg_24h,omitempty"`
	DecodeTokensPerSecAvg7d            float64 `json:"decode_tokens_per_sec_avg_7d,omitempty"`

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
// and generation tok/s (every reporting sample, active or idle), plus
// active-only sums for power and decode speed.
type avgBucket struct {
	Hour         int64
	WeightedSum  float64
	TokenSum     float64
	PowerSum     float64
	PromptTokSum float64
	GenTokSum    float64
	SampleCount  int

	// ActivePowerSum/ActivePromptTokSum/ActiveSampleCount are gated on
	// totalTok > 5 (same "non-idle" definition as WeightedSum/TokenSum
	// above). DecodeSum/DecodeSampleCount are gated on having had a valid
	// decode-speed reading at all (queryDecodeSpeed already omits idle
	// samples as NaN).
	ActivePowerSum     float64
	ActivePromptTokSum float64
	ActiveGenTokSum    float64
	ActiveSampleCount  int
	DecodeSum          float64
	DecodeSampleCount  int
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
	h.addSample(now, m.PowerWatts, m.PromptTokensPerSec, m.GenerationTokensPerSec, m.DecodeTokensPerSec, m.CarbonIntensity)
}

func (h *modelHistory) addSample(t time.Time, power, promptTok, genTok, decodeTok, intensity float64) {
	if power <= 0 {
		return
	}
	totalTok := promptTok + genTok
	active := totalTok > 5.0
	var co2Weight, co2Tokens float64
	if active {
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
			if active {
				b.ActivePowerSum += power
				b.ActivePromptTokSum += promptTok
				b.ActiveGenTokSum += genTok
				b.ActiveSampleCount++
			}
			if decodeTok > 0 {
				b.DecodeSum += decodeTok
				b.DecodeSampleCount++
			}
			return
		}
	}
	nb := avgBucket{
		Hour:         hourKey,
		WeightedSum:  co2Weight,
		TokenSum:     co2Tokens,
		PowerSum:     power,
		PromptTokSum: promptTok,
		GenTokSum:    genTok,
		SampleCount:  1,
	}
	if active {
		nb.ActivePowerSum = power
		nb.ActivePromptTokSum = promptTok
		nb.ActiveGenTokSum = genTok
		nb.ActiveSampleCount = 1
	}
	if decodeTok > 0 {
		nb.DecodeSum = decodeTok
		nb.DecodeSampleCount = 1
	}
	h.AvgBuckets = append(h.AvgBuckets, nb)
	if len(h.AvgBuckets) > maxBuckets {
		h.AvgBuckets = h.AvgBuckets[len(h.AvgBuckets)-maxBuckets:]
	}
}

// Scraper polls Prometheus and maintains in-memory state, keyed by NODE name
// (one row per GPU node in the cluster).
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

// NewWithConfig builds a scraper for an arbitrary set of nodes.
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

// --- query builders -------------------------------------------------------
//
// Every vLLM query is filtered by BOTH namespace and node and grouped by both,
// so a result can be attributed to exactly one configured node. GPU power and
// utilisation come from DCGM, which labels series with `Hostname` (the node)
// rather than `node`.

// vllmQuery wraps a vLLM metric expression in the shared node+namespace
// selector and groups it by (node, namespace) plus any extra labels.
func (s *Scraper) vllmQuery(expr string, extraGroupBy ...string) string {
	groupBy := append([]string{"node", "namespace"}, extraGroupBy...)
	return fmt.Sprintf(`sum by (%s) (%s)`, strings.Join(groupBy, ", "),
		fmt.Sprintf(expr, s.cfg.vllmSelector()))
}

// powerQueries returns the DCGM power expressions needed for the configured
// nodes: one grouped by Hostname for total-node attribution, one grouped by
// (Hostname, namespace) for nodes whose GPUs are read per-tenant. Either may
// be empty when no node needs it.
func (s *Scraper) powerQueries(rangeSel string) (nodeScoped, nsScoped string) {
	npNodes, nsNodes := s.cfg.nodesByPowerMode()
	if len(npNodes) > 0 {
		nodeScoped = fmt.Sprintf(`sum by (Hostname) (%s)`,
			dcgm("DCGM_FI_DEV_POWER_USAGE", alternation(npNodes), rangeSel))
	}
	if len(nsNodes) > 0 {
		nsScoped = fmt.Sprintf(`sum by (Hostname, namespace) (%s)`,
			dcgm("DCGM_FI_DEV_POWER_USAGE", alternation(nsNodes), rangeSel))
	}
	return nodeScoped, nsScoped
}

// dcgm builds a DCGM selector, optionally smoothed over a range window.
// rangeSel is "" for an instant read or e.g. "[5m]" for an averaged one.
func dcgm(metric, hosts, rangeSel string) string {
	sel := fmt.Sprintf(`%s{Hostname=~%q}`, metric, hosts)
	if rangeSel == "" {
		return sel
	}
	return fmt.Sprintf(`avg_over_time(%s%s)`, sel, rangeSel)
}

// backfill queries Prometheus for 7 days of historical power and token data
// and seeds the hourly average buckets so 24h/7d averages are immediately
// correct after a restart. Every series is keyed by node, exactly as the live
// scrape is.
func (s *Scraper) backfill() {
	log.Println("scraper: backfilling 7-day averages from Prometheus...")
	end := time.Now()
	start := end.Add(-7 * 24 * time.Hour)
	step := 5 * time.Minute

	type sample struct{ power, promptTok, genTok, latencySum float64 }
	byKeyTime := make(map[string]map[int64]*sample)
	ensure := func(key string) map[int64]*sample {
		if byKeyTime[key] == nil {
			byKeyTime[key] = make(map[int64]*sample)
		}
		return byKeyTime[key]
	}
	add := func(key string, ts int64, f func(*sample)) {
		m := ensure(key)
		if m[ts] == nil {
			m[ts] = &sample{}
		}
		f(m[ts])
	}

	// GPU power, from DCGM (Hostname-labelled).
	nodeScoped, nsScoped := s.powerQueries("")
	for _, q := range []struct {
		expr       string
		byHostOnly bool
	}{{nodeScoped, true}, {nsScoped, false}} {
		if q.expr == "" {
			continue
		}
		series, err := s.client.RangeQuery(q.expr, start, end, step)
		if err != nil {
			log.Printf("scraper: backfill power query failed: %v", err)
			return
		}
		for _, sr := range series {
			key := ""
			if q.byHostOnly {
				if n := s.cfg.node(sr.Metric["Hostname"]); n != nil {
					key = n.Name
				}
			} else {
				key = s.cfg.keyFor(sr.Metric["Hostname"], sr.Metric["namespace"])
			}
			if key == "" {
				continue
			}
			for _, pt := range sr.Points {
				add(key, pt.Time.Unix(), func(sm *sample) { sm.power += pt.Value })
			}
		}
	}

	// vLLM token and latency rates, node+namespace scoped.
	for _, q := range []struct {
		expr  string
		apply func(*sample, float64)
	}{
		{s.vllmQuery(`rate(vllm:prompt_tokens_total{%s}[5m])`), func(sm *sample, v float64) { sm.promptTok += v }},
		{s.vllmQuery(`rate(vllm:generation_tokens_total{%s}[5m])`), func(sm *sample, v float64) { sm.genTok += v }},
		{s.vllmQuery(`rate(vllm:inter_token_latency_seconds_sum{%s}[5m])`), func(sm *sample, v float64) { sm.latencySum += v }},
	} {
		series, err := s.client.RangeQuery(q.expr, start, end, step)
		if err != nil {
			log.Printf("scraper: backfill token query failed: %v", err)
			return
		}
		apply := q.apply
		for _, sr := range series {
			key := s.cfg.keyFor(sr.Metric["node"], sr.Metric["namespace"])
			if key == "" {
				continue
			}
			for _, pt := range sr.Points {
				value := pt.Value
				add(key, pt.Time.Unix(), func(sm *sample) { apply(sm, value) })
			}
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
			decodeTok := 0.0
			if samp.latencySum > 0 {
				decodeTok = samp.genTok / samp.latencySum
			}
			h.addSample(time.Unix(ts, 0), samp.power, samp.promptTok, samp.genTok, decodeTok, carbon.BerkeleyIntensity)
		}
	}

	log.Printf("scraper: backfilled %d node(s) from Prometheus", len(byKeyTime))
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

// nodeForNamespace resolves a namespace to the single node serving it, or ""
// if no node or more than one node matches.
func (s *Scraper) nodeForNamespace(namespace string) string {
	match := ""
	for _, n := range s.cfg.Nodes {
		if n.Namespace != namespace {
			continue
		}
		if match != "" {
			return "" // ambiguous: several nodes share the namespace
		}
		match = n.Name
	}
	return match
}

// Series returns the history for a node/metric combination. container is
// accepted for API-compatibility with the route but is unused (one serving
// container per node). metric is one of "power_watts", "co2_grams_per_hour",
// "co2_mg_per_token".
//
// State is keyed by node, but the first path segment used to be a namespace,
// so a namespace is still accepted and resolved to the node serving it — old
// links keep working as long as the namespace is unambiguous.
func (s *Scraper) Series(node, container, metric string, since time.Duration) [][2]interface{} {
	_ = container
	s.mu.RLock()
	h, ok := s.history[node]
	s.mu.RUnlock()
	if !ok {
		if resolved := s.nodeForNamespace(node); resolved != "" {
			s.mu.RLock()
			h, ok = s.history[resolved]
			s.mu.RUnlock()
		}
	}
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

	// One row per configured node, in configured order, so the dashboard's
	// card order is stable rather than map-iteration order.
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.cfg.Nodes {
		node := s.cfg.Nodes[i]
		key := node.Name
		if _, hasPower := powerByKey[key]; !hasPower {
			_, hasGen := genTokensByKey[key]
			_, hasPrompt := promptTokensByKey[key]
			if !hasGen && !hasPrompt {
				// Nothing reported for this node at all (exporter down, node
				// drained). Leave any previous reading in place rather than
				// overwriting it with zeros.
				continue
			}
		}
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
			Namespace:              node.Namespace,
			Container:              node.Container,
			GPUHardware:            node.GPUHardware,
			Node:                   node.Name,
			GPUCount:               node.GPUCount,
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
			PowerIsNodeTotal:       node.NodePower,
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

		avgs := average24h7d(now, h.AvgBuckets)
		if avgs.CO2Avg24h != 0 {
			m.CO2MgPerTokenAvg24h = avgs.CO2Avg24h
		}
		if avgs.CO2Avg7d != 0 {
			m.CO2MgPerTokenAvg7d = avgs.CO2Avg7d
		}
		if avgs.PowerAvg24h != 0 {
			m.PowerWattsAvg24h = avgs.PowerAvg24h
			m.PromptTokensPerSecAvg24h = avgs.PromptAvg24h
			m.GenerationTokensPerSecAvg24h = avgs.GenAvg24h
		}
		if avgs.PowerActiveAvg24h != 0 {
			m.PowerWattsActiveAvg24h = avgs.PowerActiveAvg24h
			m.PromptTokensPerSecActiveAvg24h = avgs.PromptActiveAvg24h
			m.GenerationTokensPerSecActiveAvg24h = avgs.GenActiveAvg24h
		}
		if avgs.PowerActiveAvg7d != 0 {
			m.PowerWattsActiveAvg7d = avgs.PowerActiveAvg7d
			m.PromptTokensPerSecActiveAvg7d = avgs.PromptActiveAvg7d
			m.GenerationTokensPerSecActiveAvg7d = avgs.GenActiveAvg7d
		}
		if avgs.DecodeAvg24h != 0 {
			m.DecodeTokensPerSecAvg24h = avgs.DecodeAvg24h
		}
		if avgs.DecodeAvg7d != 0 {
			m.DecodeTokensPerSecAvg7d = avgs.DecodeAvg7d
		}
	}
}

// windowAverages holds every rolling-window mean derived from a model's
// hourly avgBuckets: CO2/token and active-only power/decode means at 24h and
// 7d, plus the 24h time-weighted power/prompt/gen-token means (all samples).
type windowAverages struct {
	CO2Avg24h, CO2Avg7d       float64
	PowerAvg24h               float64
	PromptAvg24h, GenAvg24h   float64
	PowerActiveAvg24h         float64
	PowerActiveAvg7d          float64
	PromptActiveAvg24h        float64
	PromptActiveAvg7d         float64
	GenActiveAvg24h           float64
	GenActiveAvg7d            float64
	DecodeAvg24h, DecodeAvg7d float64
}

// average24h7d computes the 24h/7d token-weighted CO₂/token means (active
// samples only), the 24h time-weighted power/prompt/gen-token means (every
// reporting sample), and the 24h/7d active-only power and decode-speed means.
// Each field is 0 when it could not be computed (no data in that window).
func average24h7d(now time.Time, buckets []avgBucket) windowAverages {
	var wSum24, tSum24, wSum7d, tSum7d float64
	var powSum24, promptSum24, genSum24 float64
	var sampleSum24 int
	var activePowSum24, activePowSum7d float64
	var activePromptSum24, activePromptSum7d float64
	var activeGenSum24, activeGenSum7d float64
	var activeCount24, activeCount7d int
	var decodeSum24, decodeSum7d float64
	var decodeCount24, decodeCount7d int
	cutoff24h := now.Add(-24 * time.Hour).Truncate(time.Hour).Unix()
	cutoff7d := now.Add(-7 * 24 * time.Hour).Truncate(time.Hour).Unix()
	for _, b := range buckets {
		if b.Hour >= cutoff7d {
			wSum7d += b.WeightedSum
			tSum7d += b.TokenSum
			activePowSum7d += b.ActivePowerSum
			activePromptSum7d += b.ActivePromptTokSum
			activeGenSum7d += b.ActiveGenTokSum
			activeCount7d += b.ActiveSampleCount
			decodeSum7d += b.DecodeSum
			decodeCount7d += b.DecodeSampleCount
		}
		if b.Hour >= cutoff24h {
			wSum24 += b.WeightedSum
			tSum24 += b.TokenSum
			powSum24 += b.PowerSum
			promptSum24 += b.PromptTokSum
			genSum24 += b.GenTokSum
			sampleSum24 += b.SampleCount
			activePowSum24 += b.ActivePowerSum
			activePromptSum24 += b.ActivePromptTokSum
			activeGenSum24 += b.ActiveGenTokSum
			activeCount24 += b.ActiveSampleCount
			decodeSum24 += b.DecodeSum
			decodeCount24 += b.DecodeSampleCount
		}
	}
	var out windowAverages
	if tSum24 > 0 {
		out.CO2Avg24h = math.Round(wSum24/tSum24*1000) / 1000
	}
	if tSum7d > 0 {
		out.CO2Avg7d = math.Round(wSum7d/tSum7d*1000) / 1000
	}
	if sampleSum24 > 0 {
		n := float64(sampleSum24)
		out.PowerAvg24h = math.Round(powSum24/n*10) / 10
		out.PromptAvg24h = math.Round(promptSum24/n*10) / 10
		out.GenAvg24h = math.Round(genSum24/n*10) / 10
	}
	if activeCount24 > 0 {
		out.PowerActiveAvg24h = math.Round(activePowSum24/float64(activeCount24)*10) / 10
		out.PromptActiveAvg24h = math.Round(activePromptSum24/float64(activeCount24)*10) / 10
		out.GenActiveAvg24h = math.Round(activeGenSum24/float64(activeCount24)*10) / 10
	}
	if activeCount7d > 0 {
		out.PowerActiveAvg7d = math.Round(activePowSum7d/float64(activeCount7d)*10) / 10
		out.PromptActiveAvg7d = math.Round(activePromptSum7d/float64(activeCount7d)*10) / 10
		out.GenActiveAvg7d = math.Round(activeGenSum7d/float64(activeCount7d)*10) / 10
	}
	if decodeCount24 > 0 {
		out.DecodeAvg24h = math.Round(decodeSum24/float64(decodeCount24)*10) / 10
	}
	if decodeCount7d > 0 {
		out.DecodeAvg7d = math.Round(decodeSum7d/float64(decodeCount7d)*10) / 10
	}
	return out
}

// queryPower returns GPU power (W) keyed by node name.
func (s *Scraper) queryPower() (map[string]float64, error) {
	nodeScoped, nsScoped := s.powerQueries("[5m]")
	power := make(map[string]float64)

	if nodeScoped != "" {
		results, err := s.client.Query(nodeScoped)
		if err != nil {
			return nil, err
		}
		for _, r := range results {
			if n := s.cfg.node(r.Metric["Hostname"]); n != nil {
				power[n.Name] += r.Value
			}
		}
	}
	if nsScoped != "" {
		results, err := s.client.Query(nsScoped)
		if err != nil {
			return nil, err
		}
		for _, r := range results {
			if key := s.cfg.keyFor(r.Metric["Hostname"], r.Metric["namespace"]); key != "" {
				power[key] += r.Value
			}
		}
	}
	return power, nil
}

// queryTokens returns 2-minute prompt and generation token rates keyed by
// node, plus the vLLM model_name label.
func (s *Scraper) queryTokens() (genTokens, promptTokens map[string]float64, names map[string]string, err error) {
	genResults, err := s.client.Query(s.vllmQuery(`rate(vllm:generation_tokens_total{%s}[2m])`, "model_name"))
	if err != nil {
		return nil, nil, nil, err
	}
	promptResults, err := s.client.Query(s.vllmQuery(`rate(vllm:prompt_tokens_total{%s}[2m])`, "model_name"))
	if err != nil {
		return nil, nil, nil, err
	}

	genTokens = make(map[string]float64)
	promptTokens = make(map[string]float64)
	names = make(map[string]string)
	collect := func(results []prom.Result, into map[string]float64) {
		for _, r := range results {
			key := s.cfg.keyFor(r.Metric["node"], r.Metric["namespace"])
			if key == "" {
				continue
			}
			into[key] += r.Value
			if names[key] == "" {
				names[key] = r.Metric["model_name"]
			}
		}
	}
	collect(genResults, genTokens)
	collect(promptResults, promptTokens)
	return genTokens, promptTokens, names, nil
}

// queryRequestStats returns vLLM's live engine-state gauges keyed by node.
func (s *Scraper) queryRequestStats() (running, waiting, kvCache map[string]float64, err error) {
	runResults, err := s.client.Query(s.vllmQuery(`vllm:num_requests_running{%s}`))
	if err != nil {
		return nil, nil, nil, err
	}
	waitResults, err := s.client.Query(s.vllmQuery(`vllm:num_requests_waiting{%s}`))
	if err != nil {
		return nil, nil, nil, err
	}
	// avg (not sum) across engines, then scaled to a percentage.
	kvResults, err := s.client.Query(
		fmt.Sprintf(`avg by (node, namespace) (vllm:kv_cache_usage_perc{%s}) * 100`, s.cfg.vllmSelector()),
	)
	if err != nil {
		return nil, nil, nil, err
	}

	running = s.byNode(runResults)
	waiting = s.byNode(waitResults)
	kvCache = s.byNode(kvResults)
	return running, waiting, kvCache, nil
}

// byNode reduces query results labelled with (node, namespace) to a map keyed
// by node name, dropping anything no configured node claims.
func (s *Scraper) byNode(results []prom.Result) map[string]float64 {
	out := make(map[string]float64, len(results))
	for _, r := range results {
		if key := s.cfg.keyFor(r.Metric["node"], r.Metric["namespace"]); key != "" {
			out[key] += r.Value
		}
	}
	return out
}

// queryGPUUtil returns average GPU compute utilization (%) keyed by node.
func (s *Scraper) queryGPUUtil() (map[string]float64, error) {
	npNodes, nsNodes := s.cfg.nodesByPowerMode()
	util := make(map[string]float64)

	if len(npNodes) > 0 {
		results, err := s.client.Query(fmt.Sprintf(`avg by (Hostname) (%s)`,
			dcgm("DCGM_FI_DEV_GPU_UTIL", alternation(npNodes), "[5m]")))
		if err != nil {
			return nil, err
		}
		for _, r := range results {
			if n := s.cfg.node(r.Metric["Hostname"]); n != nil {
				util[n.Name] = r.Value
			}
		}
	}
	if len(nsNodes) > 0 {
		results, err := s.client.Query(fmt.Sprintf(`avg by (Hostname, namespace) (%s)`,
			dcgm("DCGM_FI_DEV_GPU_UTIL", alternation(nsNodes), "[5m]")))
		if err != nil {
			return nil, err
		}
		for _, r := range results {
			if key := s.cfg.keyFor(r.Metric["Hostname"], r.Metric["namespace"]); key != "" {
				util[key] = r.Value
			}
		}
	}
	return util, nil
}

// queryRequestRate returns completed requests per hour keyed by node.
func (s *Scraper) queryRequestRate() (map[string]float64, error) {
	results, err := s.client.Query(
		fmt.Sprintf(`%s * 3600`, s.vllmQuery(`rate(vllm:request_success_total{%s}[15m])`)),
	)
	if err != nil {
		return nil, err
	}
	return s.byNode(results), nil
}

// queryDecodeSpeed returns the actual generation speed (output tokens/sec
// while generating) keyed by node: total generation tokens divided by total
// time actually spent generating, over a 2-minute window. This excludes idle
// gaps between requests and reflects the true per-token decode rate (e.g.
// ~138 tok/s) rather than a wall-clock utilization average.
//
//	decode tok/s = rate(vllm:generation_tokens_total) / rate(vllm:inter_token_latency_seconds_sum)
//
// The _sum of the inter-token-latency histogram accumulates step latency, i.e.
// the total wall-time the engine spent producing tokens, so it only advances
// while generating — dividing tokens by it yields the active decode rate with
// no idle dilution and no busy-fraction estimate needed. It is MTP-correct:
// generation_tokens_total counts every emitted token (a step that accepts 3
// speculative tokens counts all 3), unlike inter_token_latency_count which
// records one sample per step and would undercount ~3x. Numerator and
// denominator scale together with load, so the ratio is stable even when the
// window is only partly busy. Fully-idle nodes give 0/0 -> NaN (filtered), so
// they're simply absent.
func (s *Scraper) queryDecodeSpeed() (map[string]float64, error) {
	results, err := s.client.Query(fmt.Sprintf("%s\n\t\t\t/ %s",
		s.vllmQuery(`rate(vllm:generation_tokens_total{%s}[2m])`),
		s.vllmQuery(`rate(vllm:inter_token_latency_seconds_sum{%s}[2m])`),
	))
	if err != nil {
		return nil, err
	}
	speed := make(map[string]float64)
	for _, r := range results {
		if math.IsNaN(r.Value) || math.IsInf(r.Value, 0) {
			continue
		}
		if key := s.cfg.keyFor(r.Metric["node"], r.Metric["namespace"]); key != "" {
			speed[key] = r.Value
		}
	}
	return speed, nil
}

// queryMTPAcceptance returns the speculative-decoding (MTP) draft-token
// acceptance rate (%) keyed by node.
func (s *Scraper) queryMTPAcceptance() (map[string]float64, error) {
	results, err := s.client.Query(fmt.Sprintf("100 * %s\n\t\t\t/ %s",
		s.vllmQuery(`rate(vllm:spec_decode_num_accepted_tokens_total{%s}[15m])`),
		s.vllmQuery(`rate(vllm:spec_decode_num_draft_tokens_total{%s}[15m])`),
	))
	if err != nil {
		return nil, err
	}
	return s.byNode(results), nil
}

// ClusterTimePoint is one time-step of aggregated cluster-wide carbon data.
type ClusterTimePoint struct {
	Timestamp       int64   `json:"t"`
	PowerWatts      float64 `json:"power_watts"`
	CO2GramsPerHour float64 `json:"co2_grams_per_hour"`
	CO2MgPerToken   float64 `json:"co2_mg_per_token,omitempty"`
}

// ClusterTimeSeries queries Prometheus for historical power + token data
// across EVERY configured node and returns cluster totals per time step,
// using the fixed Berkeley intensity.
func (s *Scraper) ClusterTimeSeries(rangeBack, step time.Duration) ([]ClusterTimePoint, error) {
	end := time.Now()
	start := end.Add(-rangeBack)

	var powerSeries []prom.RangeSeries
	nodeScoped, nsScoped := s.powerQueries("")
	for _, expr := range []string{nodeScoped, nsScoped} {
		if expr == "" {
			continue
		}
		series, err := s.client.RangeQuery(expr, start, end, step)
		if err != nil {
			return nil, err
		}
		powerSeries = append(powerSeries, series...)
	}

	tokenSeries, err := s.client.RangeQuery(
		s.vllmQuery(`rate(vllm:generation_tokens_total{%[1]s}[5m]) + rate(vllm:prompt_tokens_total{%[1]s}[5m])`),
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
