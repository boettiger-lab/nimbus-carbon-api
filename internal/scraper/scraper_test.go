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
	f.instant[q["decode"]] = []prom.Result{res("deepseek-v4-flash", "nimbus2", 71.7)}

	s.refreshLive()
	models := s.Models()
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	ds, qw := models[0], models[1]
	if ds.ID != "deepseek-v4-flash@nimbus2" || ds.Status != "generating" {
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
