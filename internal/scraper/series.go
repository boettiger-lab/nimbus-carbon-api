package scraper

import (
	"errors"
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

// Per-card time series for the dashboard's drill-down. Stateless like the
// rest: every request is a handful of Prometheus range queries.
//
// Every query is one of the card queries' own per-"model@node" builders
// (attributedPower, rate, buckets, ...) evaluated over a range, with the
// config's FULL node set: the gate on a pair's claimed host depends on knowing
// which node owns that host, so the expression must be byte-for-byte the one
// the cards use. The card's members are then picked out of the results in Go
// and combined exactly as the cards combine them: rates and power add,
// ratios are recomputed from summed parts, KV cache takes the fuller member,
// and latency percentiles come from buckets summed le by le.

// SeriesRange is one drill-down range: how far back, the query step, and the
// window rates and averages are taken over (never shorter than the step, so
// a point summarises everything since the previous one).
type SeriesRange struct {
	Name   string
	Dur    time.Duration
	Step   time.Duration
	Window string
}

// SeriesRanges stop at 15 days, the cluster's Prometheus retention.
var SeriesRanges = []SeriesRange{
	{"3h", 3 * time.Hour, 30 * time.Second, "2m"},
	{"24h", 24 * time.Hour, 2 * time.Minute, "5m"},
	{"7d", 7 * 24 * time.Hour, 15 * time.Minute, "15m"},
	{"15d", 15 * 24 * time.Hour, time.Hour, "1h"},
}

// ErrUnknownModel is returned for a card id the scraper has not seen.
var ErrUnknownModel = errors.New("unknown model")

// ErrUnknownRange is returned for a range not in SeriesRanges.
var ErrUnknownRange = errors.New("unknown range")

// Series is one line on a chart: [unix seconds, value] pairs, oldest first.
// Steps with no data (the model was not serving) are absent, not zero.
type Series struct {
	Label  string       `json:"label"`
	Points [][2]float64 `json:"points"`
}

// ModelSeries is the drill-down payload for one card.
type ModelSeries struct {
	ID          string `json:"id"`
	Range       string `json:"range"`
	Start       int64  `json:"start"`
	End         int64  `json:"end"`
	StepSeconds int64  `json:"step_seconds"`
	Window      string `json:"window"`
	// Metrics maps a chart name to its lines. Every name is always present;
	// a chart with no data has lines with no points.
	Metrics map[string][]Series `json:"metrics"`
	// Errors names any query that failed, so a blank chart can be told apart
	// from a model that simply had no data.
	Errors map[string]string `json:"errors,omitempty"`
}

// seriesQueries are the range queries behind one card's charts, keyed by
// name. Each returns every model's series; the caller keeps the card's.
func (c Config) seriesQueries(w string) map[string]string {
	return map[string]string{
		"power":    c.attributedPower(w, c.presence()),
		"gen":      c.rate("vllm:generation_tokens_total", w),
		"prompt":   c.rate("vllm:prompt_tokens_total", w),
		"running":  c.vllm(fmt.Sprintf(`avg_over_time(vllm:num_requests_running{%%s}[%s])`, w)),
		"waiting":  c.vllm(fmt.Sprintf(`avg_over_time(vllm:num_requests_waiting{%%s}[%s])`, w)),
		"kv":       fmt.Sprintf(`avg by (%s) (avg_over_time(vllm:kv_cache_usage_perc{%s}[%s])) * 100`, modelLabels, c.vllmSelector(), w),
		"accepted": c.rate("vllm:spec_decode_num_accepted_tokens_total", w),
		"drafted":  c.rate("vllm:spec_decode_num_draft_tokens_total", w),
		"hits":     c.rate("vllm:prefix_cache_hits_total", w),
		"lookups":  c.rate("vllm:prefix_cache_queries_total", w),
		"e2e":      c.buckets(latencyHistograms["e2e"], w),
		"ttft":     c.buckets(latencyHistograms["ttft"], w),
	}
}

// seriesHistograms are the seriesQueries whose results are bucket series.
var seriesHistograms = map[string]bool{"e2e": true, "ttft": true}

// seriesKinds are the plain series that do not add across members.
var seriesKinds = map[string]merge{"kv": maxOf}

// members returns the "model@node" ids that make up the card with this id:
// every member serving now or in any aggregation window.
func (s *Scraper) members(id string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := map[string]bool{}
	for m := range s.liveIDs {
		set[m] = true
	}
	for m := range s.aggIDs {
		set[m] = true
	}
	var out []string
	for m := range set {
		if cardID(s.cardOf(m)) == id {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// ModelTimeSeries returns the drill-down charts for card id over the named
// range (see SeriesRanges).
func (s *Scraper) ModelTimeSeries(id, rangeName string) (*ModelSeries, error) {
	var rng *SeriesRange
	for i := range SeriesRanges {
		if SeriesRanges[i].Name == rangeName {
			rng = &SeriesRanges[i]
		}
	}
	if rng == nil {
		return nil, ErrUnknownRange
	}
	members := s.members(id)
	if len(members) == 0 {
		return nil, ErrUnknownModel
	}
	keep := make(map[string]bool, len(members))
	for _, m := range members {
		keep[m] = true
	}

	end := s.now().Truncate(rng.Step)
	start := end.Add(-rng.Dur)

	plain := map[string]map[int64]float64{}            // name -> t -> value
	hist := map[string]map[int64]map[float64]float64{} // name -> t -> le -> rate
	errs := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, expr := range s.cfg.seriesQueries(rng.Window) {
		wg.Add(1)
		go func(name, expr string) {
			defer wg.Done()
			res, err := s.client.RangeQuery(expr, start, end, rng.Step)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				log.Printf("scraper: series %s for %s failed: %v", name, id, err)
				errs[name] = err.Error()
				return
			}
			if seriesHistograms[name] {
				hist[name] = s.foldBuckets(res, keep)
			} else {
				plain[name] = s.foldSeries(res, keep, seriesKinds[name])
			}
		}(name, expr)
	}
	wg.Wait()

	intensity := carbon.BerkeleyIntensity
	power := plain["power"]
	co2 := mapValues(power, func(w float64) float64 { return carbon.GramsPerHour(w, intensity) })
	tokens := addSeries(plain["gen"], plain["prompt"])
	jpt := map[int64]float64{}
	for t, tok := range tokens {
		// The live card's threshold: below 5 tok/s the denominator is noise.
		if p, ok := power[t]; ok && tok > 5 {
			jpt[t] = p / tok
		}
	}

	out := &ModelSeries{
		ID:          id,
		Range:       rng.Name,
		Start:       start.Unix(),
		End:         end.Unix(),
		StepSeconds: int64(rng.Step.Seconds()),
		Window:      rng.Window,
		Metrics: map[string][]Series{
			"power_watts":                {series("GPU power", power, 1)},
			"co2_grams_per_hour":         {series("CO₂", co2, 2)},
			"tokens_per_sec":             {series("Generation", plain["gen"], 2), series("Prompt", plain["prompt"], 2)},
			"joules_per_token":           {series("J/token", jpt, 3)},
			"requests":                   {series("Running", plain["running"], 2), series("Waiting", plain["waiting"], 2)},
			"kv_cache_percent":           {series("KV cache", plain["kv"], 1)},
			"spec_decode_accept_percent": {series("Accepted", ratio(plain["accepted"], plain["drafted"], 100), 1)},
			"prefix_cache_hit_percent":   {series("Hit rate", ratio(plain["hits"], plain["lookups"], 100), 1)},
			"latency_seconds": {
				series("E2E p50", quantiles(hist["e2e"], 0.5), 3),
				series("E2E p95", quantiles(hist["e2e"], 0.95), 3),
				series("E2E p99", quantiles(hist["e2e"], 0.99), 3),
				series("TTFT p50", quantiles(hist["ttft"], 0.5), 3),
			},
		},
	}
	if len(errs) > 0 {
		out.Errors = errs
	}
	return out, nil
}

// foldSeries keeps the card's members' series and combines them per
// timestamp: summed, or by kind (maxOf for KV cache).
func (s *Scraper) foldSeries(res []prom.RangeSeries, keep map[string]bool, kind merge) map[int64]float64 {
	out := map[int64]float64{}
	for _, sr := range res {
		if !keep[s.modelID(sr.Metric)] {
			continue
		}
		for _, pt := range sr.Points {
			if math.IsNaN(pt.Value) || math.IsInf(pt.Value, 0) {
				continue
			}
			t := pt.Time.Unix()
			prev, ok := out[t]
			switch {
			case !ok:
				out[t] = pt.Value
			case kind == maxOf:
				out[t] = math.Max(prev, pt.Value)
			default:
				out[t] = prev + pt.Value
			}
		}
	}
	return out
}

// foldBuckets keeps the card's members' bucket series and sums them per
// timestamp, le by le, as histCards does for the cards.
func (s *Scraper) foldBuckets(res []prom.RangeSeries, keep map[string]bool) map[int64]map[float64]float64 {
	out := map[int64]map[float64]float64{}
	for _, sr := range res {
		le, err := strconv.ParseFloat(sr.Metric["le"], 64)
		if err != nil || !keep[s.modelID(sr.Metric)] {
			continue
		}
		for _, pt := range sr.Points {
			if math.IsNaN(pt.Value) || math.IsInf(pt.Value, 0) {
				continue
			}
			t := pt.Time.Unix()
			if out[t] == nil {
				out[t] = map[float64]float64{}
			}
			out[t][le] += pt.Value
		}
	}
	return out
}

// quantiles applies histQuantile at every timestamp.
func quantiles(h map[int64]map[float64]float64, q float64) map[int64]float64 {
	out := map[int64]float64{}
	for t, bk := range h {
		if v, ok := histQuantile(q, bk); ok {
			out[t] = v
		}
	}
	return out
}

// ratio is scale × num/den wherever both exist and den > 0.
func ratio(num, den map[int64]float64, scale float64) map[int64]float64 {
	out := map[int64]float64{}
	for t, d := range den {
		if n, ok := num[t]; ok && d > 0 {
			out[t] = scale * n / d
		}
	}
	return out
}

// addSeries sums two series over the union of their timestamps.
func addSeries(a, b map[int64]float64) map[int64]float64 {
	out := make(map[int64]float64, len(a))
	for t, v := range a {
		out[t] = v
	}
	for t, v := range b {
		out[t] += v
	}
	return out
}

func mapValues(m map[int64]float64, f func(float64) float64) map[int64]float64 {
	out := make(map[int64]float64, len(m))
	for t, v := range m {
		out[t] = f(v)
	}
	return out
}

// series sorts a timestamp map into a labelled line.
func series(label string, m map[int64]float64, digits int) Series {
	ts := make([]int64, 0, len(m))
	for t := range m {
		ts = append(ts, t)
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
	pts := make([][2]float64, 0, len(ts))
	for _, t := range ts {
		pts = append(pts, [2]float64{float64(t), round(m[t], digits)})
	}
	return Series{Label: label, Points: pts}
}
