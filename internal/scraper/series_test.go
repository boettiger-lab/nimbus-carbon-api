package scraper

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/boettiger-lab/nimbus-carbon-api/internal/prom"
)

// rs is one range series for a model on a node, values at t0, t0+60, ...
// (NaN-free; a missing step is simply not listed).
func rs(model, node string, t0 int64, vals ...float64) prom.RangeSeries {
	pts := make([]prom.RangePoint, len(vals))
	for i, v := range vals {
		pts[i] = prom.RangePoint{Time: time.Unix(t0+int64(60*i), 0), Value: v}
	}
	return prom.RangeSeries{Metric: map[string]string{"model_name": model, "node": node, "namespace": "vllm"}, Points: pts}
}

func rsLe(model, node, le string, t0 int64, vals ...float64) prom.RangeSeries {
	r := rs(model, node, t0, vals...)
	r.Metric["le"] = le
	return r
}

// line finds a labelled line in a chart.
func line(t *testing.T, ms *ModelSeries, chart, label string) map[int64]float64 {
	t.Helper()
	for _, s := range ms.Metrics[chart] {
		if s.Label == label {
			out := map[int64]float64{}
			for _, p := range s.Points {
				out[int64(p[0])] = p[1]
			}
			return out
		}
	}
	t.Fatalf("chart %s has no line %q", chart, label)
	return nil
}

// TestSeriesTwoMemberCard: q36-mtp served on nimbus and nimbus3 is one card
// in the drill-down as on the overview. Power, tokens and requests add per
// step, KV takes the fuller member, ratios come from summed parts, latency
// percentiles from summed buckets, and another model's series stay out.
func TestSeriesTwoMemberCard(t *testing.T) {
	now := time.Unix(1_790_006_400, 0)
	s, f := newTestScraper(now)
	s.aggIDs = map[string]bool{"q36-mtp@nimbus": true, "q36-mtp@nimbus3": true, "qwen@nimbus": true}
	q := s.cfg.seriesQueries("5m")
	const t0 = 1_790_000_000
	// Step 0: only nimbus serves. Step 1: both. Step 2: only nimbus3.
	f.ranges[q["power"]] = []prom.RangeSeries{
		rs("q36-mtp", "nimbus", t0, 40, 41),
		{Metric: rs("q36-mtp", "nimbus3", 0).Metric, Points: []prom.RangePoint{{Time: time.Unix(t0+60, 0), Value: 39}, {Time: time.Unix(t0+120, 0), Value: 38}}},
		rs("qwen", "nimbus", t0, 999, 999, 999),
	}
	f.ranges[q["gen"]] = []prom.RangeSeries{rs("q36-mtp", "nimbus", t0, 20, 10), rs("q36-mtp", "nimbus3", t0, 0, 10, 2)}
	f.ranges[q["prompt"]] = []prom.RangeSeries{rs("q36-mtp", "nimbus", t0, 0, 0), rs("q36-mtp", "nimbus3", t0, 0, 0, 1)}
	f.ranges[q["running"]] = []prom.RangeSeries{rs("q36-mtp", "nimbus", t0, 1, 2), rs("q36-mtp", "nimbus3", t0, 0, 1, 0)}
	f.ranges[q["kv"]] = []prom.RangeSeries{rs("q36-mtp", "nimbus", t0, 10, 30), rs("q36-mtp", "nimbus3", t0, 0, 20, 5)}
	f.ranges[q["accepted"]] = []prom.RangeSeries{rs("q36-mtp", "nimbus", t0, 3, 3), rs("q36-mtp", "nimbus3", t0, 0, 1)}
	f.ranges[q["drafted"]] = []prom.RangeSeries{rs("q36-mtp", "nimbus", t0, 4, 4), rs("q36-mtp", "nimbus3", t0, 0, 4)}
	// Step 0: nimbus has 10 requests under 1 s; nimbus3 has 30 in (1, 2].
	f.ranges[q["e2e"]] = []prom.RangeSeries{
		rsLe("q36-mtp", "nimbus", "1", t0, 10), rsLe("q36-mtp", "nimbus", "2", t0, 10), rsLe("q36-mtp", "nimbus", "+Inf", t0, 10),
		rsLe("q36-mtp", "nimbus3", "1", t0, 0), rsLe("q36-mtp", "nimbus3", "2", t0, 30), rsLe("q36-mtp", "nimbus3", "+Inf", t0, 30),
		rsLe("qwen", "nimbus", "1", t0, 1000), rsLe("qwen", "nimbus", "+Inf", t0, 1000),
	}

	ms, err := s.ModelTimeSeries("q36-mtp", "24h")
	if err != nil {
		t.Fatal(err)
	}
	if ms.StepSeconds != 120 || ms.Window != "5m" || ms.End != now.Unix() || ms.Start != now.Add(-24*time.Hour).Unix() {
		t.Errorf("range = %d s step, %s window, %d..%d", ms.StepSeconds, ms.Window, ms.Start, ms.End)
	}
	p := line(t, ms, "power_watts", "GPU power")
	if len(p) != 3 {
		t.Fatalf("power = %v, want 3 steps", p)
	}
	approx(t, "power step 0", p[t0], 40)
	approx(t, "power step 1 (both members)", p[t0+60], 80)
	approx(t, "power step 2", p[t0+120], 38)
	approx(t, "co2 g/h step 1", line(t, ms, "co2_grams_per_hour", "CO₂")[t0+60], 80*0.198)
	approx(t, "gen step 1", line(t, ms, "tokens_per_sec", "Generation")[t0+60], 20)
	jpt := line(t, ms, "joules_per_token", "J/token")
	approx(t, "J/token step 0", jpt[t0], 2)
	approx(t, "J/token step 1", jpt[t0+60], 4)
	if _, ok := jpt[t0+120]; ok {
		t.Error("J/token at 3 tok/s should be withheld")
	}
	approx(t, "running step 1", line(t, ms, "requests", "Running")[t0+60], 3)
	approx(t, "kv step 1 (fuller member)", line(t, ms, "kv_cache_percent", "KV cache")[t0+60], 30)
	acc := line(t, ms, "spec_decode_accept_percent", "Accepted")
	approx(t, "accept step 0", acc[t0], 75)
	approx(t, "accept step 1 (summed parts)", acc[t0+60], 50)
	// Summed buckets {1: 10, 2: 40, +Inf: 40}: p50 is a third into (1, 2].
	approx(t, "e2e p50", line(t, ms, "latency_seconds", "E2E p50")[t0], 1.333)
	if got := line(t, ms, "prefix_cache_hit_percent", "Hit rate"); len(got) != 0 {
		t.Errorf("no prefix-cache data should give an empty line, got %v", got)
	}
	if len(ms.Errors) != 0 {
		t.Errorf("errors = %v", ms.Errors)
	}
}

// TestSeriesPairWithGatedHost: a TP2 pair headed on nimbus claims nimbus3,
// which also serves its own models. The drill-down must use the cards' own
// gated power expression, not a new one, and keep only the pair's series.
func TestSeriesPairWithGatedHost(t *testing.T) {
	now := time.Unix(1_790_006_400, 0)
	s, f := newTestScraper(now)
	s.cfg.Models = map[string]ModelInfo{
		"glm-5.3-flash@nimbus": {DisplayName: "GLM-5.3-Flash FP4 · TP2", PowerHosts: []string{"nimbus", "nimbus3"}, GPUCount: 2},
	}
	if err := s.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s.liveIDs = map[string]bool{"glm-5.3-flash@nimbus": true}
	s.aggIDs = map[string]bool{"laguna@nimbus3": true}
	id := cardID("GLM-5.3-Flash FP4 · TP2")

	q := s.cfg.seriesQueries("5m")
	if q["power"] != s.liveQueries()["power"] {
		t.Errorf("24h power expression differs from the live card's:\n%s\n%s", q["power"], s.liveQueries()["power"])
	}
	if !strings.Contains(q["power"], `unless on () vllm:num_requests_running{namespace="vllm",node="nimbus3"}`) {
		t.Errorf("pair's claim on nimbus3 is not gated: %s", q["power"])
	}
	const t0 = 1_790_000_000
	// What Prometheus returns for that expression: step 0, nimbus3 idle, so
	// the pair gets both boxes; step 1, laguna serves on nimbus3, the gate
	// empties the pair's nimbus3 term and laguna gets nimbus3's watts.
	f.ranges[q["power"]] = []prom.RangeSeries{
		rs("glm-5.3-flash", "nimbus", t0, 85, 43),
		{Metric: rs("laguna", "nimbus3", 0).Metric, Points: []prom.RangePoint{{Time: time.Unix(t0+60, 0), Value: 41}}},
	}
	ms, err := s.ModelTimeSeries(id, "24h")
	if err != nil {
		t.Fatal(err)
	}
	p := line(t, ms, "power_watts", "GPU power")
	approx(t, "pair power, nimbus3 idle", p[t0], 85)
	approx(t, "pair power, nimbus3 serving", p[t0+60], 43)
	if len(p) != 2 {
		t.Errorf("power = %v; laguna's series leaked into the pair's card", p)
	}

	if _, err := s.ModelTimeSeries("nope", "24h"); !errors.Is(err, ErrUnknownModel) {
		t.Errorf("unknown id: err = %v", err)
	}
	if _, err := s.ModelTimeSeries(id, "30d"); !errors.Is(err, ErrUnknownRange) {
		t.Errorf("30d: err = %v", err)
	}
}

// TestSeriesRangesStopAtRetention: no range may outrun Prometheus's 15 days,
// and no rate window may be shorter than its step (points would skip data).
func TestSeriesRangesStopAtRetention(t *testing.T) {
	for _, r := range SeriesRanges {
		w, err := time.ParseDuration(r.Window)
		if err != nil {
			t.Fatalf("%s window %q: %v", r.Name, r.Window, err)
		}
		if w < r.Step {
			t.Errorf("%s: window %s shorter than step %s", r.Name, r.Window, r.Step)
		}
		if r.Dur > 15*24*time.Hour {
			t.Errorf("%s exceeds retention", r.Name)
		}
		if n := r.Dur / r.Step; n > 11000 {
			t.Errorf("%s: %d points exceeds Prometheus's 11,000-point limit", r.Name, n)
		}
	}
}
