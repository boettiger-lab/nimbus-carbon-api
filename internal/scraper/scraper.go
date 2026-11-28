package scraper

import (
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/boettiger-lab/nimbus-carbon-api/internal/carbon"
	"github.com/boettiger-lab/nimbus-carbon-api/internal/prom"
)

// Everything here is computed from Prometheus on a timer; the service keeps no
// history of its own, so a restart loses nothing and needs no backfill. The
// unit of reporting is a MODEL, not a node: a node that switches models gets
// one card per model, and a model that is scaled to 0 keeps its card, with
// aggregate stats, for as long as Prometheus retains its series.
//
// Every query is per "model@node" (a vLLM served name on the node whose API
// server exports it): that is the grain at which power is attributed. Cards
// are one level up: every "model@node" with the same cardKey (its configured
// display name, else its served name) is folded into one card, so a model
// served on nimbus one week and nimbus3 the next is one card, not two.

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

// Model is one card on the dashboard. A card may span several "model@node"
// members (the same model served on several nodes); the single-valued
// hardware fields (ModelName, Node, Namespace, PowerHosts, GPUHardware,
// GPUCount, Description) describe its primary member: the one serving now,
// else the one seen most recently. Nodes and ModelNames list every member.
type Model struct {
	// ID is stable and URL-safe: the card key itself when it is URL-safe,
	// else a slug of it plus a short hash (see cardID).
	ID          string   `json:"id"`
	ModelName   string   `json:"model_name"`
	ModelNames  []string `json:"model_names"`
	DisplayName string   `json:"display_name,omitempty"`
	Description string   `json:"description,omitempty"`
	Node        string   `json:"node"`
	Nodes       []string `json:"nodes"`
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
	live      map[string]*Live                    // card -> merged live state
	liveIDs   map[string]bool                     // model@node members serving now
	aggs      map[string]map[string]*Aggregate    // window -> card -> agg
	aggIDs    map[string]bool                     // model@node members serving in any window
	avail     map[string]map[string]*Availability // window -> card -> strip
	seen      map[string]seen                     // model@node -> first/last seen
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
		liveIDs:     map[string]bool{},
		aggs:        map[string]map[string]*Aggregate{},
		aggIDs:      map[string]bool{},
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

// histograms runs bucket queries (see Config.buckets) concurrently and keys
// each result "model@node" and `le`: name -> id -> upper bound -> rate.
func (s *Scraper) histograms(queries map[string]string) map[string]map[string]map[float64]float64 {
	out := make(map[string]map[string]map[float64]float64, len(queries))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, expr := range queries {
		wg.Add(1)
		go func(name, expr string) {
			defer wg.Done()
			byID := map[string]map[float64]float64{}
			res, err := s.client.Query(expr)
			if err != nil {
				log.Printf("scraper: %s query failed: %v", name, err)
			}
			for _, r := range res {
				le, err := strconv.ParseFloat(r.Metric["le"], 64)
				id := s.modelID(r.Metric)
				if err != nil || id == "" || math.IsNaN(r.Value) || math.IsInf(r.Value, 0) {
					continue
				}
				if byID[id] == nil {
					byID[id] = map[float64]float64{}
				}
				byID[id][le] += r.Value
			}
			mu.Lock()
			out[name] = byID
			mu.Unlock()
		}(name, expr)
	}
	wg.Wait()
	return out
}

// --- cards ------------------------------------------------------------------

// cardOf is the card a "model@node" id belongs to.
func (s *Scraper) cardOf(id string) string {
	model, node := splitID(id)
	return s.cfg.cardKey(model, node)
}

// merge says how one field's per-"model@node" values fold into a card.
type merge int

const (
	sumOf  merge = iota // additive: energy, tokens, requests, hours, rates
	maxOf               // peaks, last seen
	minOf               // first seen
	meanOf              // a gauge two live members both report (GPU util)
)

// toCards folds per-"model@node" results into per-card ones, over the member
// ids keep admits. Fields default to sumOf. Ratios are NOT merged here:
// setRatio recomputes them from the summed numerator and denominator.
func (s *Scraper) toCards(r map[string]map[string]float64, keep map[string]bool, kinds map[string]merge) map[string]map[string]float64 {
	out := make(map[string]map[string]float64, len(r))
	for name, byID := range r {
		kind := kinds[name]
		vals := map[string]float64{}
		counts := map[string]int{}
		for id, v := range byID {
			if !keep[id] {
				continue
			}
			k := s.cardOf(id)
			prev, ok := vals[k]
			switch {
			case !ok:
				vals[k] = v
			case kind == maxOf:
				vals[k] = math.Max(prev, v)
			case kind == minOf:
				vals[k] = math.Min(prev, v)
			default:
				vals[k] = prev + v
			}
			counts[k]++
		}
		if kind == meanOf {
			for k := range vals {
				vals[k] /= float64(counts[k])
			}
		}
		out[name] = vals
	}
	return out
}

// histCards sums each histogram's buckets over a card's members, le by le —
// exactly what `sum by (le)` would do in PromQL.
func (s *Scraper) histCards(h map[string]map[string]map[float64]float64, keep map[string]bool) map[string]map[string]map[float64]float64 {
	out := make(map[string]map[string]map[float64]float64, len(h))
	for name, byID := range h {
		byCard := map[string]map[float64]float64{}
		for id, bk := range byID {
			if !keep[id] {
				continue
			}
			k := s.cardOf(id)
			if byCard[k] == nil {
				byCard[k] = map[float64]float64{}
			}
			for le, v := range bk {
				byCard[k][le] += v
			}
		}
		out[name] = byCard
	}
	return out
}

// setRatio writes scale × num/den per card into rc[name], wherever both
// were reported and den > 0 (PromQL's division drops the same cases).
func setRatio(rc map[string]map[string]float64, name, num, den string, scale float64) {
	out := map[string]float64{}
	for k, d := range rc[den] {
		if n, ok := rc[num][k]; ok && d > 0 {
			out[k] = scale * n / d
		}
	}
	rc[name] = out
}

// latencyHistograms are the vLLM histograms whose percentiles a card shows.
var latencyHistograms = map[string]string{
	"ttft":  "vllm:time_to_first_token_seconds",
	"e2e":   "vllm:e2e_request_latency_seconds",
	"queue": "vllm:request_queue_time_seconds",
}

// percentiles are the fields taken from latencyHistograms.
var percentiles = []struct {
	field, hist string
	q           float64
}{
	{"ttft50", "ttft", 0.5}, {"ttft95", "ttft", 0.95},
	{"e2e50", "e2e", 0.5}, {"e2e95", "e2e", 0.95},
	{"queue50", "queue", 0.5},
}

// histQueries is one bucket query per latency histogram over window w.
func (c Config) histQueries(w string) map[string]string {
	out := make(map[string]string, len(latencyHistograms))
	for name, h := range latencyHistograms {
		out[name] = c.buckets(h, w)
	}
	return out
}

// setPercentiles writes each latency percentile per card into rc, from the
// card's summed buckets. A percentile of several nodes' requests is exact
// this way: the buckets add, the percentiles would not.
func setPercentiles(rc map[string]map[string]float64, hc map[string]map[string]map[float64]float64) {
	for _, p := range percentiles {
		out := map[string]float64{}
		for k, bk := range hc[p.hist] {
			if v, ok := histQuantile(p.q, bk); ok {
				out[k] = v
			}
		}
		rc[p.field] = out
	}
}

// histQuantile is Prometheus's histogram_quantile for one classic histogram:
// cumulative bucket counts (or rates) by upper bound, interpolated linearly
// within the bucket the rank falls in. ok is false wherever Prometheus would
// return NaN: no +Inf bucket, fewer than two buckets, no observations.
func histQuantile(q float64, buckets map[float64]float64) (float64, bool) {
	if len(buckets) < 2 || q < 0 || q > 1 {
		return 0, false
	}
	ub := make([]float64, 0, len(buckets))
	for le := range buckets {
		ub = append(ub, le)
	}
	sort.Float64s(ub)
	n := len(ub)
	if !math.IsInf(ub[n-1], 1) {
		return 0, false
	}
	// Counts are cumulative; force them monotonic, as Prometheus does, in
	// case a scrape landed between two buckets' updates.
	counts := make([]float64, n)
	for i, le := range ub {
		counts[i] = buckets[le]
		if i > 0 && counts[i] < counts[i-1] {
			counts[i] = counts[i-1]
		}
	}
	total := counts[n-1]
	if total <= 0 {
		return 0, false
	}
	rank := q * total
	b := sort.Search(n-1, func(i int) bool { return counts[i] >= rank })
	if b == n-1 {
		return ub[n-2], true
	}
	if b == 0 && ub[0] <= 0 {
		return ub[0], true
	}
	start, end, count := 0.0, ub[b], counts[b]
	if b > 0 {
		start = ub[b-1]
		count -= counts[b-1]
		rank -= counts[b-1]
	}
	return start + (end-start)*(rank/count), true
}

// stripsByCard folds members' availability strips into one per card, taking
// the higher state at every step (generating > serving > not serving).
func (s *Scraper) stripsByCard(byID map[string]*Availability) map[string]*Availability {
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := map[string]*Availability{}
	for _, id := range ids {
		a, k := byID[id], s.cardOf(id)
		m := out[k]
		if m == nil {
			cp := *a
			cp.States = append([]int(nil), a.States...)
			out[k] = &cp
			continue
		}
		for i := range m.States {
			if i < len(a.States) && a.States[i] > m.States[i] {
				m.States[i] = a.States[i]
			}
		}
	}
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
		"running": c.vllm(`vllm:num_requests_running{%s}`),
		"waiting": c.vllm(`vllm:num_requests_waiting{%s}`),
		"kv":      fmt.Sprintf(`avg by (%s) (vllm:kv_cache_usage_perc{%s}) * 100`, modelLabels, c.vllmSelector()),
		// The displayed run/wait/KV values are peaks over the same 2-minute
		// window as the token rates that set "generating": a single-stream
		// client reads 0/0 between requests on the instant gauges while its
		// token rate still says it is generating. The instant "running"
		// above still decides which members are live.
		"running_peak": c.vllm(`max_over_time(vllm:num_requests_running{%s}[2m])`),
		"waiting_peak": c.vllm(`max_over_time(vllm:num_requests_waiting{%s}[2m])`),
		"kv_peak":      fmt.Sprintf(`avg by (%s) (max_over_time(vllm:kv_cache_usage_perc{%s}[2m])) * 100`, modelLabels, c.vllmSelector()),
		"power":        c.attributedPower("5m", c.presence()),
		"util":         c.attributedUtil("5m", c.presence()),
		"prompt":       c.rate("vllm:prompt_tokens_total", "2m"),
		"gen":          c.rate("vllm:generation_tokens_total", "2m"),
		"itl":          c.rate("vllm:inter_token_latency_seconds_sum", "2m"),
		"reqs":         c.rate("vllm:request_success_total", "15m") + " * 3600",
		"hits":         c.rate("vllm:prefix_cache_hits_total", "15m"),
		"lookups":      c.rate("vllm:prefix_cache_queries_total", "15m"),
		"accepted":     c.rate("vllm:spec_decode_num_accepted_tokens_total", "15m"),
		"drafted":      c.rate("vllm:spec_decode_num_draft_tokens_total", "15m"),
		"lastseen":     fmt.Sprintf(`max by (%s) (timestamp(vllm:num_requests_running{%s}))`, modelLabels, c.vllmSelector()),
	}
}

// liveKinds are the live fields that do not simply add across a card's
// serving members. Normally one member serves at a time and these never
// matter; if two serve at once, their rates and power add, the fuller KV
// cache is shown, and GPU utilisation is averaged.
var liveKinds = map[string]merge{"kv": maxOf, "kv_peak": maxOf, "util": meanOf, "lastseen": maxOf}

func (s *Scraper) refreshLive() {
	r := s.batch(s.liveQueries())
	h := s.histograms(s.cfg.histQueries("15m"))
	// The running gauge is exported for as long as the server is up, so it
	// defines which members are live; a member that stopped a minute ago
	// still has 15-minute rates, and must not add them to the card.
	ids := make(map[string]bool, len(r["running"]))
	for id := range r["running"] {
		ids[id] = true
	}
	rc := s.toCards(r, ids, liveKinds)
	setRatio(rc, "decode", "gen", "itl", 1)
	setRatio(rc, "prefix", "hits", "lookups", 100)
	setRatio(rc, "accept", "accepted", "drafted", 100)
	setPercentiles(rc, s.histCards(h, ids))

	intensity := carbon.BerkeleyIntensity
	live := make(map[string]*Live)
	for card := range rc["running"] {
		live[card] = buildLive(rc, card, intensity)
	}

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = live
	s.liveIDs = ids
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

// buildLive turns one card's merged live results into a Live.
func buildLive(r map[string]map[string]float64, id string, intensity float64) *Live {
	// Peak over 2 minutes, never below the instant reading (the peak series
	// can lag a fresh server by a scrape).
	running := math.Max(r["running"][id], r["running_peak"][id])
	waiting := math.Max(r["waiting"][id], r["waiting_peak"][id])
	kv := ptr(r["kv_peak"], id, 1)
	if kv == nil {
		kv = ptr(r["kv"], id, 1)
	}
	power := r["power"][id]
	prompt, gen := r["prompt"][id], r["gen"][id]
	l := &Live{
		PowerWatts:             round(power, 1),
		CO2GramsPerHour:        round(carbon.GramsPerHour(power, intensity), 2),
		PromptTokensPerSec:     round(prompt, 1),
		GenerationTokensPerSec: round(gen, 1),
		RequestsRunning:        running,
		RequestsWaiting:        waiting,
		KVCachePercent:         kv,
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
	return l
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
		"itl":        c.increase("vllm:inter_token_latency_seconds_sum", w),
		"hits":       c.increase("vllm:prefix_cache_hits_total", w),
		"lookups":    c.increase("vllm:prefix_cache_queries_total", w),
		"accepted":   c.increase("vllm:spec_decode_num_accepted_tokens_total", w),
		"drafted":    c.increase("vllm:spec_decode_num_draft_tokens_total", w),
		"first_seen": overTime("min_over_time", fmt.Sprintf(`min by (%s) (timestamp(vllm:num_requests_running{%s}))`, modelLabels, c.vllmSelector()), w),
		"last_seen":  overTime("max_over_time", fmt.Sprintf(`max by (%s) (timestamp(vllm:num_requests_running{%s}))`, modelLabels, c.vllmSelector()), w),
	}
}

// aggKinds are the aggregate fields that do not simply add across a card's
// members. Everything else (serving/active seconds, energy, tokens, requests
// and the ratio parts) is summed, and buildAggregate recomputes every mean
// and ratio from the sums. Peak concurrency is the larger member's peak.
var aggKinds = map[string]merge{"peak": maxOf, "first_seen": minOf, "last_seen": maxOf}

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
	// covered is the window's length in retention, NOT summed over a card's
	// members: uptime is the card's serving time over the window. Members
	// serving one after another (the normal case) make that exact; two
	// serving at once overstate it, hence the cap.
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
	aggIDs := map[string]bool{}
	first := map[string]time.Time{}
	last := map[string]time.Time{}

	for _, win := range s.windows {
		covered := win.Dur
		if !retention.IsZero() && now.Sub(retention) < covered {
			covered = now.Sub(retention)
		}
		r := s.batch(s.aggregateQueries(win.Name))
		h := s.histograms(s.cfg.histQueries(win.Name))
		// Only members that served in the window count toward a card, as
		// before cards existed: tokens with no serving time had no record.
		ids := map[string]bool{}
		for id, v := range r["serving_s"] {
			if v > 0 {
				ids[id] = true
				aggIDs[id] = true
			}
		}
		rc := s.toCards(r, ids, aggKinds)
		setRatio(rc, "decode", "gen", "itl", 1)
		setRatio(rc, "prefix", "hits", "lookups", 100)
		setRatio(rc, "accept", "accepted", "drafted", 100)
		setPercentiles(rc, s.histCards(h, ids))
		byCard := map[string]*Aggregate{}
		for card := range rc["serving_s"] {
			if a := buildAggregate(win.Name, covered, rc, card, intensity); a != nil {
				byCard[card] = a
			}
		}
		aggs[win.Name] = byCard
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
		avail[win.Name] = s.stripsByCard(s.availability(win, now))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.aggs = aggs
	s.aggIDs = aggIDs
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

// Models returns one card per model (see cardKey) seen in live state or in
// any window, generating first, then idle, then offline by most recently seen.
func (s *Scraper) Models() []*Model {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cards := map[string]bool{}
	for k := range s.live {
		cards[k] = true
	}
	for _, byCard := range s.aggs {
		for k := range byCard {
			cards[k] = true
		}
	}
	members := map[string][]string{}
	addMember := func(id string) {
		if k := s.cardOf(id); cards[k] {
			for _, x := range members[k] {
				if x == id {
					return
				}
			}
			members[k] = append(members[k], id)
		}
	}
	for id := range s.liveIDs {
		addMember(id)
	}
	for id := range s.aggIDs {
		addMember(id)
	}

	out := make([]*Model, 0, len(cards))
	for key := range cards {
		ms := members[key]
		sort.Strings(ms)
		primary := s.primary(ms)
		if primary == "" {
			continue
		}
		model, nodeName := splitID(primary)
		n := s.cfg.node(nodeName)
		if n == nil {
			continue
		}
		info := s.cfg.info(model, nodeName)
		m := &Model{
			ID:              cardID(key),
			ModelName:       model,
			ModelNames:      distinct(ms, func(x string) string { name, _ := splitID(x); return name }),
			DisplayName:     info.DisplayName,
			Description:     info.Description,
			Node:            n.Name,
			Nodes:           distinct(ms, func(x string) string { _, node := splitID(x); return node }),
			Namespace:       n.Namespace,
			PowerHosts:      s.cfg.powerHostsFor(model, nodeName),
			GPUHardware:     n.GPUHardware,
			GPUCount:        s.cfg.gpuCountFor(model, nodeName),
			CarbonIntensity: carbon.BerkeleyIntensity,
			Aggregates:      map[string]*Aggregate{},
			Availability:    map[string]*Availability{},
			Status:          "offline",
		}
		if l, ok := s.live[key]; ok {
			cp := *l
			m.Live = &cp
			m.Status = "idle"
			if l.RequestsRunning > 0 || l.GenerationTokensPerSec+l.PromptTokensPerSec > 0.5 {
				m.Status = "generating"
			}
		}
		for w, byCard := range s.aggs {
			if a, ok := byCard[key]; ok {
				m.Aggregates[w] = a
			}
		}
		for w, byCard := range s.avail {
			if a, ok := byCard[key]; ok {
				m.Availability[w] = a
			}
		}
		for _, member := range ms {
			sn := s.seen[member]
			if !sn.first.IsZero() && (m.FirstSeen == nil || sn.first.Before(*m.FirstSeen)) {
				f := sn.first
				m.FirstSeen = &f
			}
			if !sn.last.IsZero() && (m.LastSeen == nil || sn.last.After(*m.LastSeen)) {
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

// primary picks the member whose hardware a card shows: the first (sorted)
// member serving now, else the one seen most recently. Caller holds s.mu.
func (s *Scraper) primary(sorted []string) string {
	best := ""
	for _, id := range sorted {
		if s.liveIDs[id] {
			return id
		}
		if best == "" || s.seen[id].last.After(s.seen[best].last) {
			best = id
		}
	}
	return best
}

// distinct maps ids through f and returns the distinct results, sorted.
func distinct(ids []string, f func(string) string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if v := f(id); !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
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
