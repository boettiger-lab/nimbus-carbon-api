package scraper

import (
	"math"
	"testing"
	"time"

	"github.com/boettiger-lab/nimbus-carbon-api/internal/prom"
)

// fakeProm answers instant queries from a map keyed by the exact expression,
// and range queries from a second map. Unknown queries return nothing, which
// is what Prometheus does for a series that does not exist.
type fakeProm struct {
	instant map[string][]prom.Result
	ranges  map[string][]prom.RangeSeries
}

func (f *fakeProm) Query(q string) ([]prom.Result, error) { return f.instant[q], nil }
func (f *fakeProm) RangeQuery(q string, _, _ time.Time, _ time.Duration) ([]prom.RangeSeries, error) {
	return f.ranges[q], nil
}

func res(model, node string, v float64) prom.Result {
	return prom.Result{Metric: map[string]string{"model_name": model, "node": node, "namespace": "vllm"}, Value: v}
}

func newTestScraper(now time.Time) (*Scraper, *fakeProm) {
	f := &fakeProm{instant: map[string][]prom.Result{}, ranges: map[string][]prom.RangeSeries{}}
	s := newScraper(f, time.Second, clusterConfig())
	s.now = func() time.Time { return now }
	return s, f
}

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6*math.Max(1, math.Abs(want)) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// TestLiveStatusAndCarbon: a model with traffic is "generating", one exporting
// with no traffic is "idle", and CO₂/token is withheld below 5 tok/s (a
// near-zero denominator gives absurd numbers).
func TestLiveStatusAndCarbon(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	s, f := newTestScraper(now)
	q := s.liveQueries()
	f.instant[q["running"]] = []prom.Result{res("deepseek-v4-flash", "nimbus2", 2), res("qwen", "nimbus", 0)}
	f.instant[q["power"]] = []prom.Result{res("deepseek-v4-flash", "nimbus2", 100), res("qwen", "nimbus", 12)}
	f.instant[q["prompt"]] = []prom.Result{res("deepseek-v4-flash", "nimbus2", 40), res("qwen", "nimbus", 0)}
	f.instant[q["gen"]] = []prom.Result{res("deepseek-v4-flash", "nimbus2", 60), res("qwen", "nimbus", 0)}
	// 60 tok/s over 0.8 s/s of inter-token time = 75 tok/s while decoding.
	f.instant[q["itl"]] = []prom.Result{res("deepseek-v4-flash", "nimbus2", 0.8)}

	s.refreshLive()
	models := s.Models()
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	ds, qw := models[0], models[1]
	if ds.ID != "deepseek-v4-flash" || ds.Status != "generating" {
		t.Errorf("first model = %s/%s, want deepseek generating first", ds.ID, ds.Status)
	}
	if qw.Status != "idle" {
		t.Errorf("qwen status = %s, want idle", qw.Status)
	}
	// 100 W at 0.198 kg/kWh = 19.8 g/h; over 100 tok/s = 0.055 mg/token.
	approx(t, "co2 g/h", ds.Live.CO2GramsPerHour, 19.8)
	if ds.Live.CO2MgPerToken == nil {
		t.Fatal("co2/token missing for an active model")
	}
	approx(t, "co2 mg/token", *ds.Live.CO2MgPerToken, 0.055)
	if ds.Live.DecodeTokensPerSec == nil {
		t.Fatal("decode speed missing")
	}
	approx(t, "decode tok/s", *ds.Live.DecodeTokensPerSec, 75)
	if qw.Live.CO2MgPerToken != nil {
		t.Errorf("co2/token should be withheld for an idle model, got %v", *qw.Live.CO2MgPerToken)
	}
	if qw.Live.DecodeTokensPerSec != nil {
		t.Error("decode speed should be absent for an idle model")
	}
	if ds.GPUCount != 2 || len(ds.PowerHosts) != 2 {
		t.Errorf("TP2 hardware not carried through: %d GPUs, hosts %v", ds.GPUCount, ds.PowerHosts)
	}
}

// TestUnconfiguredNodeIsDropped: metrics from a node we do not report on must
// not appear, let alone be folded into another model.
func TestUnconfiguredNodeIsDropped(t *testing.T) {
	s, f := newTestScraper(time.Unix(1_790_000_000, 0))
	f.instant[s.liveQueries()["running"]] = []prom.Result{res("mystery", "thelio", 1)}
	s.refreshLive()
	if n := len(s.Models()); n != 0 {
		t.Errorf("got %d models, want 0", n)
	}
}

// TestOfflineModelKeepsAggregates: a model scaled to 0 has no live state but
// must still get a card with its history.
func TestOfflineModelKeepsAggregates(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	s, f := newTestScraper(now)
	s.windows = []Window{{"24h", 24 * time.Hour, 30 * time.Minute}}
	q := s.aggregateQueries("24h")
	f.instant[q["serving_s"]] = []prom.Result{res("laguna", "nimbus3", 7200)}
	f.instant[q["energy_j"]] = []prom.Result{res("laguna", "nimbus3", 3.6e6)}
	f.instant[q["first_seen"]] = []prom.Result{res("laguna", "nimbus3", float64(now.Add(-3*time.Hour).Unix()))}
	f.instant[q["last_seen"]] = []prom.Result{res("laguna", "nimbus3", float64(now.Add(-time.Hour).Unix()))}

	s.refreshLive()
	s.refreshAggregates()
	models := s.Models()
	if len(models) != 1 {
		t.Fatalf("got %d models, want 1", len(models))
	}
	m := models[0]
	if m.Status != "offline" || m.Live != nil {
		t.Errorf("status = %s, live = %v; want offline with no live state", m.Status, m.Live)
	}
	if m.Aggregates["24h"] == nil {
		t.Fatal("offline model lost its aggregate")
	}
	if m.LastSeen == nil || !m.LastSeen.Equal(now.Add(-time.Hour)) {
		t.Errorf("last seen = %v, want an hour ago", m.LastSeen)
	}
	if m.FirstSeen == nil || !m.FirstSeen.Equal(now.Add(-3*time.Hour)) {
		t.Errorf("first seen = %v, want three hours ago", m.FirstSeen)
	}
}

// TestBuildAggregateArithmetic pins the carbon arithmetic: 1 kWh over 2 h of
// serving is 500 W mean and 198 g CO₂; over 36,000 tokens that is 5.5 mg and
// 100 J per token. Active energy is a quarter of it, so the marginal figure
// is a quarter too.
func TestBuildAggregateArithmetic(t *testing.T) {
	id := "qwen@nimbus"
	r := map[string]map[string]float64{
		"serving_s": {id: 7200},
		"energy_j":  {id: 3.6e6},
		"active_j":  {id: 0.9e6},
		"active_s":  {id: 1800},
		"prompt":    {id: 30000},
		"gen":       {id: 6000},
		"requests":  {id: 60},
	}
	a := buildAggregate("24h", 24*time.Hour, r, id, 0.198)
	approx(t, "energy kWh", a.EnergyKWh, 1)
	approx(t, "co2 g", a.CO2Grams, 198)
	approx(t, "mean W", a.PowerWatts, 500)
	approx(t, "active W", a.ActivePower, 500)
	approx(t, "co2 mg/token", *a.CO2MgPerToken, 5.5)
	approx(t, "co2 mg/token active", *a.CO2MgPerTokenActive, 1.375)
	approx(t, "J/token", *a.JoulesPerToken, 100)
	approx(t, "uptime %", a.UptimePercent, round(100*7200/86400.0, 1))
	approx(t, "mean prompt", *a.MeanPromptTokens, 500)
	approx(t, "mean gen", *a.MeanGenTokens, 100)

	// Retention shorter than the window: uptime is over what Prometheus has.
	short := buildAggregate("15d", 4*time.Hour, r, id, 0.198)
	approx(t, "uptime % clipped", short.UptimePercent, 50)

	// 60 requests clears the percentile floor; 5 does not.
	r["e2e95"] = map[string]float64{id: 40}
	if a := buildAggregate("24h", 24*time.Hour, r, id, 0.198); a.E2EP95Seconds == nil {
		t.Error("p95 withheld despite 60 requests")
	}
	r["requests"] = map[string]float64{id: 5}
	if a := buildAggregate("24h", 24*time.Hour, r, id, 0.198); a.E2EP95Seconds != nil {
		t.Errorf("p95 of 5 requests should be withheld, got %v", *a.E2EP95Seconds)
	}

	if buildAggregate("24h", 24*time.Hour, map[string]map[string]float64{}, id, 0.198) != nil {
		t.Error("a model that never served in the window should have no aggregate")
	}
}

// TestAvailabilityBuckets: serving beats nothing, generating beats serving,
// and each point lands in the bucket that ends at its timestamp.
func TestAvailabilityBuckets(t *testing.T) {
	now := time.Unix(1_790_006_400, 0) // on a 30-minute boundary
	s, f := newTestScraper(now)
	win := Window{"2h", 2 * time.Hour, 30 * time.Minute}
	step := win.Step
	end := now.Truncate(step)
	start := end.Add(-win.Dur).Add(step)
	pt := func(i int) prom.RangePoint {
		return prom.RangePoint{Time: start.Add(time.Duration(i) * step), Value: 1}
	}
	lbl := map[string]string{"model_name": "qwen", "node": "nimbus", "namespace": "vllm"}

	servingQ := "max by (node, namespace, model_name) (max_over_time(vllm:num_requests_running{" + s.cfg.vllmSelector() + "}[1800s]))"
	genQ := s.cfg.vllm(`increase(vllm:generation_tokens_total{%s}[1800s])`) + " > 0"
	f.ranges[servingQ] = []prom.RangeSeries{{Metric: lbl, Points: []prom.RangePoint{pt(1), pt(2), pt(3)}}}
	f.ranges[genQ] = []prom.RangeSeries{{Metric: lbl, Points: []prom.RangePoint{pt(2)}}}

	a := s.availability(win, now)["qwen@nimbus"]
	if a == nil {
		t.Fatal("no strip for qwen")
	}
	want := []int{0, 1, 2, 1}
	if len(a.States) != len(want) {
		t.Fatalf("states = %v, want %v", a.States, want)
	}
	for i := range want {
		if a.States[i] != want[i] {
			t.Errorf("states = %v, want %v", a.States, want)
			break
		}
	}
}

// resLe is one histogram bucket series for a model on a node.
func resLe(model, node, le string, v float64) prom.Result {
	r := res(model, node, v)
	r.Metric["le"] = le
	return r
}

// TestSameModelTwoNodesIsOneCard: q36-mtp served on nimbus and then nimbus3
// is ONE card. Additive fields sum, ratios are recomputed from the sums,
// peaks take the max, percentiles come from the summed buckets, and seen
// times span both. Meanwhile "qwen@nimbus", configured with its own display
// name, stays apart from plain qwen on nimbus3.
func TestSameModelTwoNodesIsOneCard(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	s, f := newTestScraper(now)
	s.cfg.Models = map[string]ModelInfo{"qwen@nimbus": {DisplayName: "Qwen Next"}}
	s.windows = []Window{{"24h", 24 * time.Hour, 30 * time.Minute}}
	q := s.aggregateQueries("24h")
	two := func(a, b float64) []prom.Result {
		return []prom.Result{res("q36-mtp", "nimbus", a), res("q36-mtp", "nimbus3", b)}
	}
	f.instant[q["serving_s"]] = append(two(3600, 7200), res("qwen", "nimbus", 600), res("qwen", "nimbus3", 600))
	f.instant[q["energy_j"]] = append(two(0.36e6, 1.44e6), res("qwen", "nimbus", 6e4), res("qwen", "nimbus3", 6e4))
	f.instant[q["prompt"]] = two(1000, 2000)
	f.instant[q["gen"]] = two(500, 1500)
	f.instant[q["requests"]] = two(10, 30)
	f.instant[q["peak"]] = two(3, 5)
	f.instant[q["itl"]] = two(10, 20)
	f.instant[q["hits"]] = two(100, 300)
	f.instant[q["lookups"]] = two(200, 400)
	f.instant[q["first_seen"]] = two(float64(now.Add(-10*time.Hour).Unix()), float64(now.Add(-3*time.Hour).Unix()))
	f.instant[q["last_seen"]] = two(float64(now.Add(-5*time.Hour).Unix()), float64(now.Add(-time.Hour).Unix()))
	// nimbus: 10 requests all under 1 s; nimbus3: 30 between 1 and 2 s.
	f.instant[s.cfg.histQueries("24h")["e2e"]] = []prom.Result{
		resLe("q36-mtp", "nimbus", "1", 10), resLe("q36-mtp", "nimbus", "2", 10), resLe("q36-mtp", "nimbus", "+Inf", 10),
		resLe("q36-mtp", "nimbus3", "1", 0), resLe("q36-mtp", "nimbus3", "2", 30), resLe("q36-mtp", "nimbus3", "+Inf", 30),
	}

	s.refreshLive()
	s.refreshAggregates()
	models := s.Models()
	if len(models) != 3 {
		ids := []string{}
		for _, m := range models {
			ids = append(ids, m.ID)
		}
		t.Fatalf("got cards %v, want q36-mtp, qwen and Qwen Next", ids)
	}
	var m *Model
	for _, x := range models {
		if x.ID == "q36-mtp" {
			m = x
		}
	}
	if m == nil {
		t.Fatal("no q36-mtp card")
	}
	if len(m.Nodes) != 2 || m.Nodes[0] != "nimbus" || m.Nodes[1] != "nimbus3" {
		t.Errorf("nodes = %v", m.Nodes)
	}
	if m.Node != "nimbus3" {
		t.Errorf("node = %s, want nimbus3 (seen most recently)", m.Node)
	}
	a := m.Aggregates["24h"]
	if a == nil {
		t.Fatal("no merged aggregate")
	}
	approx(t, "serving h", a.ServingHours, 3)
	approx(t, "energy kWh", a.EnergyKWh, 0.5)
	approx(t, "mean W", a.PowerWatts, round(1.8e6/10800, 1))
	approx(t, "J/token", *a.JoulesPerToken, 360)
	approx(t, "requests", a.Requests, 40)
	approx(t, "peak", a.PeakConcurrent, 5)
	approx(t, "decode", *a.DecodeTokensPerSec, round(2000.0/30, 1))
	approx(t, "prefix", *a.PrefixHitPercent, round(100*400.0/600, 1))
	approx(t, "mean gen", *a.MeanGenTokens, 50)
	approx(t, "covered", a.CoveredHours, 24)
	// Summed buckets {1: 10, 2: 40, +Inf: 40}: rank 20 lands a third of the
	// way into (1, 2]. Averaging the two nodes' medians (0.5, 1.5) would say 1.
	if a.E2EP50Seconds == nil || a.E2EP95Seconds == nil {
		t.Fatal("percentiles withheld despite 40 requests")
	}
	approx(t, "e2e p50", *a.E2EP50Seconds, 1.33)
	approx(t, "e2e p95", *a.E2EP95Seconds, 1.93)
	if !m.FirstSeen.Equal(now.Add(-10*time.Hour)) || !m.LastSeen.Equal(now.Add(-time.Hour)) {
		t.Errorf("seen = %v .. %v", m.FirstSeen, m.LastSeen)
	}
}

// TestLiveCardUsesServingMembers: only the member serving now feeds the live
// view, even though the one that stopped still has 15-minute rates; if two
// serve at once, their rates and power add.
func TestLiveCardUsesServingMembers(t *testing.T) {
	s, f := newTestScraper(time.Unix(1_790_000_000, 0))
	q := s.liveQueries()
	f.instant[q["running"]] = []prom.Result{res("q36-mtp", "nimbus3", 1)}
	f.instant[q["power"]] = []prom.Result{res("q36-mtp", "nimbus3", 80)}
	f.instant[q["reqs"]] = []prom.Result{res("q36-mtp", "nimbus", 600), res("q36-mtp", "nimbus3", 120)}
	s.refreshLive()
	ms := s.Models()
	if len(ms) != 1 || ms[0].Live == nil {
		t.Fatalf("want one live card, got %d", len(ms))
	}
	approx(t, "requests/h", ms[0].Live.RequestsPerHour, 120)
	if ms[0].Node != "nimbus3" || len(ms[0].Nodes) != 1 {
		t.Errorf("node = %s, nodes = %v", ms[0].Node, ms[0].Nodes)
	}

	f.instant[q["running"]] = []prom.Result{res("q36-mtp", "nimbus3", 1), res("q36-mtp", "nimbus", 2)}
	f.instant[q["power"]] = []prom.Result{res("q36-mtp", "nimbus3", 80), res("q36-mtp", "nimbus", 40)}
	s.refreshLive()
	ms = s.Models()
	if len(ms) != 1 {
		t.Fatalf("two live members should still be one card, got %d", len(ms))
	}
	approx(t, "power", ms[0].Live.PowerWatts, 120)
	approx(t, "running", ms[0].Live.RequestsRunning, 3)
	approx(t, "requests/h", ms[0].Live.RequestsPerHour, 720)
	if ms[0].Node != "nimbus" {
		t.Errorf("primary of two live members = %s, want the first sorted (nimbus)", ms[0].Node)
	}
}

// TestStripsMergeByMax: a card's availability is the higher state of its
// members at every step.
func TestStripsMergeByMax(t *testing.T) {
	s, _ := newTestScraper(time.Unix(1_790_000_000, 0))
	out := s.stripsByCard(map[string]*Availability{
		"q36-mtp@nimbus":  {Start: 1, StepSeconds: 60, States: []int{2, 1, 0, 0}},
		"q36-mtp@nimbus3": {Start: 1, StepSeconds: 60, States: []int{0, 0, 1, 2}},
		"qwen@nimbus":     {Start: 1, StepSeconds: 60, States: []int{1, 1, 1, 1}},
	})
	want := []int{2, 1, 1, 2}
	got := out["q36-mtp"].States
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("states = %v, want %v", got, want)
		}
	}
	if len(out) != 2 {
		t.Errorf("want 2 strips, got %d", len(out))
	}
}

// TestHistQuantileMatchesPrometheus pins the interpolation to what
// histogram_quantile returns for the same buckets.
func TestHistQuantileMatchesPrometheus(t *testing.T) {
	inf := math.Inf(1)
	b := map[float64]float64{0.5: 10, 1: 30, 2: 40, inf: 40}
	cases := []struct{ q, want float64 }{
		{0.5, 0.75}, // rank 20: halfway through (0.5, 1]
		{0.25, 0.5}, // rank 10: top of the first bucket
		{0.1, 0.2},  // rank 4: inside the first bucket, from 0
		{0.95, 1.8}, // rank 38: 8/10 into (1, 2]
	}
	for _, c := range cases {
		got, ok := histQuantile(c.q, b)
		if !ok {
			t.Fatalf("q=%v: no result", c.q)
		}
		approx(t, "quantile", got, c.want)
	}
	// Rank in the +Inf bucket: the highest finite bound, as Prometheus does.
	if got, _ := histQuantile(0.99, map[float64]float64{1: 1, inf: 10}); got != 1 {
		t.Errorf("+Inf bucket quantile = %v, want 1", got)
	}
	if _, ok := histQuantile(0.5, map[float64]float64{1: 0, inf: 0}); ok {
		t.Error("no observations should give no quantile")
	}
	if _, ok := histQuantile(0.5, map[float64]float64{1: 3, 2: 5}); ok {
		t.Error("no +Inf bucket should give no quantile")
	}
}
