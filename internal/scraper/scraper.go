package scraper

import (
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/boettiger-lab/nimbus-carbon-api/internal/carbon"
	"github.com/boettiger-lab/nimbus-carbon-api/internal/prom"
)

// Everything here is computed from Prometheus on a timer; the service keeps no
// history of its own, so a restart loses nothing and needs no backfill. The
// unit of reporting is a MODEL (a vLLM served model name on the node whose API
// server exports it), not a node: a node that switches models gets one card per
// model, and a model that is scaled to 0 keeps its card, with aggregate stats,
// for as long as Prometheus retains its series.

// Live is a model's current state. Pointer fields are nil when there is
// nothing to report (e.g. no requests completed in the latency window), which
// the dashboard shows as "—" rather than a misleading 0.
type Live struct {
	PowerWatts             float64  `json:"power_watts"`
	CO2GramsPerHour        float64  `json:"co2_grams_per_hour"`
	CO2MgPerToken          *float64 `json:"co2_mg_per_token,omitempty"` // only while > 5 tok/s
	PromptTokensPerSec     float64  `json:"prompt_tokens_per_sec"`
	GenerationTokensPerSec float64  `json:"generation_tokens_per_sec"`
	// DecodeTokensPerSec is generation speed WHILE generating:
	// rate(generation_tokens) / rate(inter_token_latency_sum). Not diluted by
	// idle gaps, and MTP-correct (see methodology).
	DecodeTokensPerSec *float64 `json:"decode_tokens_per_sec,omitempty"`
	RequestsRunning    float64  `json:"requests_running"`
	RequestsWaiting    float64  `json:"requests_waiting"`
	KVCachePercent     *float64 `json:"kv_cache_usage_percent,omitempty"`
	GPUUtilPercent     *float64 `json:"gpu_util_percent,omitempty"`
	RequestsPerHour    float64  `json:"requests_per_hour"`
	TTFTP50Seconds     *float64 `json:"ttft_p50_seconds,omitempty"`
	E2EP50Seconds      *float64 `json:"e2e_p50_seconds,omitempty"`
	E2EP95Seconds      *float64 `json:"e2e_p95_seconds,omitempty"`
	QueueP50Seconds    *float64 `json:"queue_p50_seconds,omitempty"`
	PrefixHitPercent   *float64 `json:"prefix_cache_hit_percent,omitempty"`
	SpecAcceptPercent  *float64 `json:"spec_decode_accept_percent,omitempty"`
}

// Aggregate is a model's record over one window, clipped to Prometheus
// retention. Energy is node GPU power integrated over the minutes the model was
// serving on it, so idle hosting counts — that is what the model really cost.
type Aggregate struct {
	Window         string  `json:"window"`
	CoveredHours   float64 `json:"covered_hours"` // window length actually in retention
	ServingHours   float64 `json:"serving_hours"`
	ActiveHours    float64 `json:"active_hours"` // minutes above 5 tok/s
	UptimePercent  float64 `json:"uptime_percent"`
	EnergyKWh      float64 `json:"energy_kwh"`
	CO2Grams       float64 `json:"co2_grams"`
	PowerWatts     float64 `json:"power_watts"`        // mean while serving
	ActivePower    float64 `json:"active_power_watts"` // mean while > 5 tok/s
	CO2GramsPerHr  float64 `json:"co2_grams_per_hour"` // mean while serving
	PromptTokens   float64 `json:"prompt_tokens"`
	GenTokens      float64 `json:"generation_tokens"`
	Requests       float64 `json:"requests"`
	PeakConcurrent float64 `json:"peak_requests_running"`

	// All-in: every serving joule over every token. Active: only the joules
	// spent while generating, over the same tokens — the marginal cost.
	CO2MgPerToken       *float64 `json:"co2_mg_per_token,omitempty"`
	CO2MgPerTokenActive *float64 `json:"co2_mg_per_token_active,omitempty"`
	JoulesPerToken      *float64 `json:"joules_per_token,omitempty"`

	DecodeTokensPerSec *float64 `json:"decode_tokens_per_sec,omitempty"`
	// Throughput while active, the frontier comparison's input.
	PromptTokensPerSecActive *float64 `json:"prompt_tokens_per_sec_active,omitempty"`
	GenTokensPerSecActive    *float64 `json:"generation_tokens_per_sec_active,omitempty"`
	TTFTP50Seconds           *float64 `json:"ttft_p50_seconds,omitempty"`
	TTFTP95Seconds           *float64 `json:"ttft_p95_seconds,omitempty"`
	E2EP50Seconds            *float64 `json:"e2e_p50_seconds,omitempty"`
	E2EP95Seconds            *float64 `json:"e2e_p95_seconds,omitempty"`
	QueueP50Seconds          *float64 `json:"queue_p50_seconds,omitempty"`
	PrefixHitPercent         *float64 `json:"prefix_cache_hit_percent,omitempty"`
	SpecAcceptPercent        *float64 `json:"spec_decode_accept_percent,omitempty"`
	MeanPromptTokens         *float64 `json:"mean_prompt_tokens,omitempty"`
	MeanGenTokens            *float64 `json:"mean_generation_tokens,omitempty"`
}

// Availability is a strip of equal buckets covering a window, oldest first:
// 0 = not serving, 1 = serving but idle, 2 = generating.
type Availability struct {
	Start       int64 `json:"start"`
	StepSeconds int64 `json:"step_seconds"`
	States      []int `json:"states"`
}

// Model is one card on the dashboard.
type Model struct {
	ID          string   `json:"id"` // model@node
	ModelName   string   `json:"model_name"`
	DisplayName string   `json:"display_name,omitempty"`
	Description string   `json:"description,omitempty"`
	Node        string   `json:"node"`
	Namespace   string   `json:"namespace"`
	PowerHosts  []string `json:"power_hosts"`
	GPUHardware string   `json:"gpu_hardware"`
	GPUCount    int      `json:"gpu_count"`

	// Status is "generating", "idle" (serving, no traffic) or "offline".
	Status    string     `json:"status"`
	FirstSeen *time.Time `json:"first_seen,omitempty"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`

	CarbonIntensity float64                  `json:"carbon_intensity_kg_per_kwh"`
	Live            *Live                    `json:"live"`
	Aggregates      map[string]*Aggregate    `json:"aggregates"`
	Availability    map[string]*Availability `json:"availability"`
}

// Window is one aggregation period and the bucket size of its strip.
type Window struct {
	Name string
	Dur  time.Duration
	Step time.Duration
}

// DefaultWindows stop at 15 days because that is the cluster's Prometheus
// retention; a longer window would silently report the same 15 days.
var DefaultWindows = []Window{
	{"24h", 24 * time.Hour, 30 * time.Minute},
	{"7d", 7 * 24 * time.Hour, 3 * time.Hour},
	{"15d", 15 * 24 * time.Hour, 6 * time.Hour},
}

// querier is the slice of the Prometheus client the scraper uses, so tests
// can substitute canned results.
type querier interface {
	Query(query string) ([]prom.Result, error)
	RangeQuery(query string, start, end time.Time, step time.Duration) ([]prom.RangeSeries, error)
}

type seen struct{ first, last time.Time }

// Scraper polls Prometheus and holds the latest snapshot.
type Scraper struct {
	client      querier
	interval    time.Duration
	aggInterval time.Duration
	cfg         Config
	windows     []Window
	now         func() time.Time

	mu        sync.RWMutex
	live      map[string]*Live
	aggs      map[string]map[string]*Aggregate    // window -> id -> agg
	avail     map[string]map[string]*Availability // window -> id -> strip
	seen      map[string]seen
	updated   time.Time
	retention time.Time
}

// New builds a scraper with the default (nimbus) configuration.
func New(promURL string, interval time.Duration) *Scraper {
	return NewWithConfig(promURL, interval, DefaultConfig())
}

// NewWithConfig builds a scraper for an arbitrary set of nodes.
func NewWithConfig(promURL string, interval time.Duration, cfg Config) *Scraper {
	return newScraper(prom.NewClient(promURL, 30*time.Second), interval, cfg)
}

func newScraper(q querier, interval time.Duration, cfg Config) *Scraper {
	return &Scraper{
		client:      q,
		interval:    interval,
		aggInterval: 5 * time.Minute,
		cfg:         cfg,
		windows:     DefaultWindows,
		now:         time.Now,
		live:        map[string]*Live{},
		aggs:        map[string]map[string]*Aggregate{},
		avail:       map[string]map[string]*Availability{},
		seen:        map[string]seen{},
	}
}

// Windows returns the configured aggregation windows.
func (s *Scraper) Windows() []Window { return s.windows }

// Run refreshes live state every interval and aggregates every aggInterval.
func (s *Scraper) Run() {
	s.refreshLive()
	s.refreshAggregates()
	lastAgg := s.now()
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for range t.C {
		s.refreshLive()
		if s.now().Sub(lastAgg) >= s.aggInterval {
			s.refreshAggregates()
			lastAgg = s.now()
		}
	}
}

// RetentionStart is the oldest sample Prometheus holds (zero if unknown). A
// first_seen near it means "at least this old", not "appeared then".
func (s *Scraper) RetentionStart() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.retention
}

// Updated is when the live snapshot was last refreshed.
func (s *Scraper) Updated() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.updated
}

// --- query plumbing -------------------------------------------------------

// byModel runs an instant query and keys each result "model@node", dropping
// results no configured node claims, and non-finite values.
func (s *Scraper) byModel(expr string) (map[string]float64, error) {
	res, err := s.client.Query(expr)
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(res))
	for _, r := range res {
		if math.IsInf(r.Value, 0) || math.IsNaN(r.Value) {
			continue
		}
		if id := s.modelID(r.Metric); id != "" {
			out[id] = r.Value
		}
	}
	return out, nil
}

func (s *Scraper) modelID(m map[string]string) string {
	node := s.cfg.keyFor(m["node"], m["namespace"])
	if node == "" || m["model_name"] == "" {
		return ""
	}
	return m["model_name"] + "@" + node
}

// batch runs several named queries, logging (not failing on) any error so
// one bad expression blanks one field instead of the whole card.
func (s *Scraper) batch(queries map[string]string) map[string]map[string]float64 {
	out := make(map[string]map[string]float64, len(queries))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, expr := range queries {
		wg.Add(1)
		go func(name, expr string) {
			defer wg.Done()
			v, err := s.byModel(expr)
			if err != nil {
				log.Printf("scraper: %s query failed: %v", name, err)
				v = map[string]float64{}
			}
			mu.Lock()
			out[name] = v
			mu.Unlock()
		}(name, expr)
	}
	wg.Wait()
	return out
}

func ptr(m map[string]float64, id string, digits int) *float64 {
	v, ok := m[id]
	if !ok {
		return nil
	}
	r := round(v, digits)
	return &r
}

func round(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}

// minQuantileRequests is the fewest completed requests a latency percentile
// is reported from. Traffic here is bursty and often single-user: a "p95" of
// two requests is two data points dressed up as a statistic, so below this
// the percentile is withheld rather than shown.
const minQuantileRequests = 20

// quantilesIf returns v only when enough requests back it.
func quantilesIf(requests float64, v *float64) *float64 {
	if requests < minQuantileRequests {
		return nil
	}
	return v
}

// --- live -----------------------------------------------------------------

func (s *Scraper) liveQueries() map[string]string {
	c := s.cfg
	return map[string]string{
		"running":  c.vllm(`vllm:num_requests_running{%s}`),
		"waiting":  c.vllm(`vllm:num_requests_waiting{%s}`),
		"kv":       fmt.Sprintf(`avg by (%s) (vllm:kv_cache_usage_perc{%s}) * 100`, modelLabels, c.vllmSelector()),
		"power":    c.attributedPower("5m", c.presence()),
		"util":     fmt.Sprintf(`(%s * on (node) group_right () %s)`, c.dcgmByNode("DCGM_FI_DEV_GPU_UTIL", "avg", "5m"), c.presence()),
		"prompt":   c.vllm(`rate(vllm:prompt_tokens_total{%s}[2m])`),
		"gen":      c.vllm(`rate(vllm:generation_tokens_total{%s}[2m])`),
		"decode":   fmt.Sprintf(`%s / %s`, c.vllm(`rate(vllm:generation_tokens_total{%s}[2m])`), c.vllm(`rate(vllm:inter_token_latency_seconds_sum{%s}[2m])`)),
		"reqs":     c.vllm(`rate(vllm:request_success_total{%s}[15m])`) + " * 3600",
		"ttft50":   c.quantile(0.5, "vllm:time_to_first_token_seconds", "15m"),
		"e2e50":    c.quantile(0.5, "vllm:e2e_request_latency_seconds", "15m"),
		"e2e95":    c.quantile(0.95, "vllm:e2e_request_latency_seconds", "15m"),
		"queue50":  c.quantile(0.5, "vllm:request_queue_time_seconds", "15m"),
		"prefix":   c.ratio("vllm:prefix_cache_hits_total", "vllm:prefix_cache_queries_total", "15m", 100),
		"accept":   c.ratio("vllm:spec_decode_num_accepted_tokens_total", "vllm:spec_decode_num_draft_tokens_total", "15m", 100),
		"lastseen": fmt.Sprintf(`max by (%s) (timestamp(vllm:num_requests_running{%s}))`, modelLabels, c.vllmSelector()),
	}
}

func (s *Scraper) refreshLive() {
	r := s.batch(s.liveQueries())
	intensity := carbon.BerkeleyIntensity
	live := make(map[string]*Live)
	// The running gauge is exported for as long as the server is up, so it
	// defines which models are live.
	for id, running := range r["running"] {
		power := r["power"][id]
		prompt, gen := r["prompt"][id], r["gen"][id]
		l := &Live{
			PowerWatts:             round(power, 1),
			CO2GramsPerHour:        round(carbon.GramsPerHour(power, intensity), 2),
			PromptTokensPerSec:     round(prompt, 1),
			GenerationTokensPerSec: round(gen, 1),
			RequestsRunning:        running,
			RequestsWaiting:        r["waiting"][id],
			KVCachePercent:         ptr(r["kv"], id, 1),
			GPUUtilPercent:         ptr(r["util"], id, 1),
			RequestsPerHour:        round(r["reqs"][id], 1),
			TTFTP50Seconds:         quantilesIf(r["reqs"][id]/4, ptr(r["ttft50"], id, 3)),
			E2EP50Seconds:          quantilesIf(r["reqs"][id]/4, ptr(r["e2e50"], id, 2)),
			E2EP95Seconds:          quantilesIf(r["reqs"][id]/4, ptr(r["e2e95"], id, 2)),
			QueueP50Seconds:        quantilesIf(r["reqs"][id]/4, ptr(r["queue50"], id, 3)),
			PrefixHitPercent:       ptr(r["prefix"], id, 1),
			SpecAcceptPercent:      ptr(r["accept"], id, 1),
		}
		if d, ok := r["decode"][id]; ok && d > 0 {
			l.DecodeTokensPerSec = ptr(r["decode"], id, 1)
		}
		if tok := prompt + gen; tok > 5 && power > 0 {
			v := round(carbon.MgPerToken(power, intensity, tok), 4)
			l.CO2MgPerToken = &v
		}
		live[id] = l
	}

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = live
	s.updated = now
	for id, ts := range r["lastseen"] {
		sn := s.seen[id]
		t := time.Unix(int64(ts), 0)
		if t.After(sn.last) {
			sn.last = t
		}
		if sn.first.IsZero() {
			sn.first = t
		}
		s.seen[id] = sn
	}
}

// --- aggregates -----------------------------------------------------------

func (s *Scraper) aggregateQueries(w string) map[string]string {
	c := s.cfg
	return map[string]string{
		"serving_s":  overTime("count_over_time", c.presence(), w) + " * 60",
		"active_s":   overTime("count_over_time", c.active(), w) + " * 60",
		"energy_j":   overTime("sum_over_time", c.attributedPower("", c.presence()), w) + " * 60",
		"active_j":   overTime("sum_over_time", c.attributedPower("", c.active()), w) + " * 60",
		"peak":       overTime("max_over_time", c.vllm(`vllm:num_requests_running{%s}`), w),
		"prompt":     c.increase("vllm:prompt_tokens_total", w),
		"gen":        c.increase("vllm:generation_tokens_total", w),
		"requests":   c.increase("vllm:request_success_total", w),
		"decode":     c.ratio("vllm:generation_tokens_total", "vllm:inter_token_latency_seconds_sum", w, 1),
		"ttft50":     c.quantile(0.5, "vllm:time_to_first_token_seconds", w),
		"ttft95":     c.quantile(0.95, "vllm:time_to_first_token_seconds", w),
		"e2e50":      c.quantile(0.5, "vllm:e2e_request_latency_seconds", w),
		"e2e95":      c.quantile(0.95, "vllm:e2e_request_latency_seconds", w),
		"queue50":    c.quantile(0.5, "vllm:request_queue_time_seconds", w),
		"prefix":     c.ratio("vllm:prefix_cache_hits_total", "vllm:prefix_cache_queries_total", w, 100),
		"accept":     c.ratio("vllm:spec_decode_num_accepted_tokens_total", "vllm:spec_decode_num_draft_tokens_total", w, 100),
		"first_seen": overTime("min_over_time", fmt.Sprintf(`min by (%s) (timestamp(vllm:num_requests_running{%s}))`, modelLabels, c.vllmSelector()), w),
		"last_seen":  overTime("max_over_time", fmt.Sprintf(`max by (%s) (timestamp(vllm:num_requests_running{%s}))`, modelLabels, c.vllmSelector()), w),
	}
}

// retentionStart is the oldest sample Prometheus still holds, so a window
// longer than retention reports its true coverage instead of a fake uptime.
func (s *Scraper) retentionStart() time.Time {
	res, err := s.client.Query(`min(prometheus_tsdb_lowest_timestamp_seconds)`)
	if err != nil || len(res) == 0 || res[0].Value <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(res[0].Value), 0)
}

// buildAggregate turns one window's raw query results into an Aggregate.
func buildAggregate(w string, covered time.Duration, r map[string]map[string]float64, id string, intensity float64) *Aggregate {
	servingS := r["serving_s"][id]
	if servingS <= 0 {
		return nil
	}
	energyJ := r["energy_j"][id]
	activeJ := r["active_j"][id]
	activeS := r["active_s"][id]
	prompt, gen := r["prompt"][id], r["gen"][id]
	tokens := prompt + gen
	requests := r["requests"][id]

	kwh := energyJ / 3.6e6
	a := &Aggregate{
		Window:             w,
		CoveredHours:       round(covered.Hours(), 1),
		ServingHours:       round(servingS/3600, 2),
		ActiveHours:        round(activeS/3600, 2),
		EnergyKWh:          round(kwh, 4),
		CO2Grams:           round(kwh*intensity*1000, 1),
		PowerWatts:         round(energyJ/servingS, 1),
		CO2GramsPerHr:      round(carbon.GramsPerHour(energyJ/servingS, intensity), 2),
		PromptTokens:       math.Round(prompt),
		GenTokens:          math.Round(gen),
		Requests:           math.Round(requests),
		PeakConcurrent:     r["peak"][id],
		DecodeTokensPerSec: ptr(r["decode"], id, 1),
		TTFTP50Seconds:     quantilesIf(requests, ptr(r["ttft50"], id, 3)),
		TTFTP95Seconds:     quantilesIf(requests, ptr(r["ttft95"], id, 3)),
		E2EP50Seconds:      quantilesIf(requests, ptr(r["e2e50"], id, 2)),
		E2EP95Seconds:      quantilesIf(requests, ptr(r["e2e95"], id, 2)),
		QueueP50Seconds:    quantilesIf(requests, ptr(r["queue50"], id, 3)),
		PrefixHitPercent:   ptr(r["prefix"], id, 1),
		SpecAcceptPercent:  ptr(r["accept"], id, 1),
	}
	if covered > 0 {
		a.UptimePercent = round(math.Min(100, 100*servingS/covered.Seconds()), 1)
	}
	if activeS > 0 {
		a.ActivePower = round(activeJ/activeS, 1)
		p, g := round(prompt/activeS, 2), round(gen/activeS, 2)
		a.PromptTokensPerSecActive, a.GenTokensPerSecActive = &p, &g
	}
	if tokens >= 1 && energyJ > 0 {
		all := round(kwh*intensity*1e6/tokens, 4)
		jpt := round(energyJ/tokens, 3)
		a.CO2MgPerToken, a.JoulesPerToken = &all, &jpt
		if activeJ > 0 {
			act := round(activeJ/3.6e6*intensity*1e6/tokens, 4)
			a.CO2MgPerTokenActive = &act
		}
	}
	if requests >= 1 {
		mp, mg := math.Round(prompt/requests), math.Round(gen/requests)
		a.MeanPromptTokens, a.MeanGenTokens = &mp, &mg
	}
	return a
}

func (s *Scraper) refreshAggregates() {
	now := s.now()
	retention := s.retentionStart()
	intensity := carbon.BerkeleyIntensity

	aggs := make(map[string]map[string]*Aggregate, len(s.windows))
	avail := make(map[string]map[string]*Availability, len(s.windows))
	first := map[string]time.Time{}
	last := map[string]time.Time{}

	for _, win := range s.windows {
		covered := win.Dur
		if !retention.IsZero() && now.Sub(retention) < covered {
			covered = now.Sub(retention)
		}
		r := s.batch(s.aggregateQueries(win.Name))
		byID := map[string]*Aggregate{}
		for id := range r["serving_s"] {
			if a := buildAggregate(win.Name, covered, r, id, intensity); a != nil {
				byID[id] = a
			}
		}
		aggs[win.Name] = byID
		for id, ts := range r["first_seen"] {
			if t := time.Unix(int64(ts), 0); first[id].IsZero() || t.Before(first[id]) {
				first[id] = t
			}
		}
		for id, ts := range r["last_seen"] {
			if t := time.Unix(int64(ts), 0); t.After(last[id]) {
				last[id] = t
			}
		}
		avail[win.Name] = s.availability(win, now)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.aggs = aggs
	s.avail = avail
	s.retention = retention
	for id, t := range first {
		sn := s.seen[id]
		sn.first = t
		if last[id].After(sn.last) {
			sn.last = last[id]
		}
		s.seen[id] = sn
	}
}

// availability builds each model's strip for a window from two range
// queries: was the server exporting at all, and did it generate anything.
func (s *Scraper) availability(win Window, now time.Time) map[string]*Availability {
	c := s.cfg
	step := win.Step
	end := now.Truncate(step)
	start := end.Add(-win.Dur).Add(step)
	n := int(win.Dur / step)
	stepS := fmt.Sprintf("%ds", int(step.Seconds()))

	out := map[string]*Availability{}
	fill := func(expr string, state int) {
		series, err := s.client.RangeQuery(expr, start, end, step)
		if err != nil {
			log.Printf("scraper: availability query failed: %v", err)
			return
		}
		for _, sr := range series {
			id := s.modelID(sr.Metric)
			if id == "" {
				continue
			}
			a := out[id]
			if a == nil {
				a = &Availability{Start: start.Add(-step).Unix(), StepSeconds: int64(step.Seconds()), States: make([]int, n)}
				out[id] = a
			}
			for _, pt := range sr.Points {
				// A point at t covers (t-step, t]; bucket i covers
				// [Start + i*step, Start + (i+1)*step).
				i := int(pt.Time.Sub(start) / step)
				if i >= 0 && i < n && state > a.States[i] {
					a.States[i] = state
				}
			}
		}
	}
	fill(fmt.Sprintf(`max by (%s) (max_over_time(vllm:num_requests_running{%s}[%s]))`, modelLabels, c.vllmSelector(), stepS), 1)
	fill(c.vllm(fmt.Sprintf(`increase(vllm:generation_tokens_total{%%s}[%s])`, stepS))+" > 0", 2)
	return out
}

// --- assembly -------------------------------------------------------------

// Models returns one record per model seen in live state or in any window,
// generating first, then idle, then offline by most recently seen.
func (s *Scraper) Models() []*Model {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := map[string]bool{}
	for id := range s.live {
		ids[id] = true
	}
	for _, byID := range s.aggs {
		for id := range byID {
			ids[id] = true
		}
	}

	out := make([]*Model, 0, len(ids))
	for id := range ids {
		model, nodeName := splitID(id)
		n := s.cfg.node(nodeName)
		if n == nil {
			continue
		}
		info := s.cfg.info(model, nodeName)
		m := &Model{
			ID:              id,
			ModelName:       model,
			DisplayName:     info.DisplayName,
			Description:     info.Description,
			Node:            n.Name,
			Namespace:       n.Namespace,
			PowerHosts:      n.PowerHosts,
			GPUHardware:     n.GPUHardware,
			GPUCount:        n.GPUCount,
			CarbonIntensity: carbon.BerkeleyIntensity,
			Aggregates:      map[string]*Aggregate{},
			Availability:    map[string]*Availability{},
			Status:          "offline",
		}
		if l, ok := s.live[id]; ok {
			cp := *l
			m.Live = &cp
			m.Status = "idle"
			if l.RequestsRunning > 0 || l.GenerationTokensPerSec+l.PromptTokensPerSec > 0.5 {
				m.Status = "generating"
			}
		}
		for w, byID := range s.aggs {
			if a, ok := byID[id]; ok {
				m.Aggregates[w] = a
			}
		}
		for w, byID := range s.avail {
			if a, ok := byID[id]; ok {
				m.Availability[w] = a
			}
		}
		if sn, ok := s.seen[id]; ok {
			if !sn.first.IsZero() {
				f := sn.first
				m.FirstSeen = &f
			}
			if !sn.last.IsZero() {
				l := sn.last
				m.LastSeen = &l
			}
		}
		out = append(out, m)
	}

	rank := map[string]int{"generating": 0, "idle": 1, "offline": 2}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if rank[a.Status] != rank[b.Status] {
			return rank[a.Status] < rank[b.Status]
		}
		if a.LastSeen != nil && b.LastSeen != nil && !a.LastSeen.Equal(*b.LastSeen) {
			return a.LastSeen.After(*b.LastSeen)
		}
		return a.ID < b.ID
	})
	return out
}

func splitID(id string) (model, node string) {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '@' {
			return id[:i], id[i+1:]
		}
	}
	return id, ""
}

// --- cluster time series ----------------------------------------------------

// ClusterTimePoint is one time-step of cluster-wide LLM carbon data.
type ClusterTimePoint struct {
	Timestamp       int64   `json:"t"`
	PowerWatts      float64 `json:"power_watts"`
	CO2GramsPerHour float64 `json:"co2_grams_per_hour"`
	CO2MgPerToken   float64 `json:"co2_mg_per_token,omitempty"`
}

// ClusterTimeSeries returns cluster totals per step. Power is attributed
// power — a node's watts count only while a model is serving there — so GPU
// time spent on anything that is not LLM serving stays out of the total.
func (s *Scraper) ClusterTimeSeries(rangeBack, step time.Duration) ([]ClusterTimePoint, error) {
	c := s.cfg
	end := s.now()
	start := end.Add(-rangeBack)

	power, err := s.client.RangeQuery(fmt.Sprintf(`sum(%s)`, c.attributedPower("", c.presence())), start, end, step)
	if err != nil {
		return nil, err
	}
	tokens, err := s.client.RangeQuery(fmt.Sprintf(`sum(%s) + sum(%s)`,
		c.vllm(`rate(vllm:generation_tokens_total{%s}[5m])`),
		c.vllm(`rate(vllm:prompt_tokens_total{%s}[5m])`)), start, end, step)
	if err != nil {
		return nil, err
	}

	tok := map[int64]float64{}
	for _, sr := range tokens {
		for _, pt := range sr.Points {
			tok[pt.Time.Unix()] += pt.Value
		}
	}
	var out []ClusterTimePoint
	for _, sr := range power {
		for _, pt := range sr.Points {
			ts := pt.Time.Unix()
			co2 := carbon.GramsPerHour(pt.Value, carbon.BerkeleyIntensity)
			p := ClusterTimePoint{Timestamp: ts, PowerWatts: round(pt.Value, 1), CO2GramsPerHour: round(co2, 2)}
			if t := tok[ts]; t > 0.1 {
				p.CO2MgPerToken = round(carbon.MgPerToken(pt.Value, carbon.BerkeleyIntensity, t), 4)
			}
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp < out[j].Timestamp })
	return out, nil
}
