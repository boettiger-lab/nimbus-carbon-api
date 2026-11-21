package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/boettiger-lab/nimbus-carbon-api/internal/scraper"
)

// stubSeries answers for one card id and the ranges the scraper knows.
type stubSeries struct{ gotID, gotRange string }

func (s *stubSeries) ModelTimeSeries(id, rng string) (*scraper.ModelSeries, error) {
	s.gotID, s.gotRange = id, rng
	known := false
	for _, r := range scraper.SeriesRanges {
		known = known || r.Name == rng
	}
	if !known {
		return nil, scraper.ErrUnknownRange
	}
	if id != "GLM-5.3-Flash-b0092d58" {
		return nil, scraper.ErrUnknownModel
	}
	return &scraper.ModelSeries{ID: id, Range: rng, Metrics: map[string][]scraper.Series{
		"power_watts": {{Label: "GPU power", Points: [][2]float64{{1790000000, 84.1}}}},
	}}, nil
}

// TestModelTimeSeriesRoute: the drill-down endpoint routes by path id,
// defaults to 24h, and maps unknown ids and ranges to 404 and 400.
func TestModelTimeSeriesRoute(t *testing.T) {
	stub := &stubSeries{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleDashboard)
	mux.Handle("GET /api/v1/models/{id}/timeseries", modelTimeSeriesHandler(stub))

	cases := []struct {
		url       string
		code      int
		wantRange string
	}{
		{"/api/v1/models/GLM-5.3-Flash-b0092d58/timeseries", 200, "24h"},
		{"/api/v1/models/GLM-5.3-Flash-b0092d58/timeseries?range=3h", 200, "3h"},
		{"/api/v1/models/GLM-5.3-Flash-b0092d58/timeseries?range=30d", 400, "30d"},
		{"/api/v1/models/nope/timeseries?range=7d", 404, "7d"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", c.url, nil))
		if rec.Code != c.code {
			t.Errorf("%s: code %d, want %d (%s)", c.url, rec.Code, c.code, rec.Body.String())
		}
		if stub.gotRange != c.wantRange {
			t.Errorf("%s: range %q, want %q", c.url, stub.gotRange, c.wantRange)
		}
		if c.code != 200 {
			continue
		}
		var body scraper.ModelSeries
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v", c.url, err)
		}
		if body.ID != "GLM-5.3-Flash-b0092d58" || len(body.Metrics["power_watts"][0].Points) != 1 {
			t.Errorf("%s: body %+v", c.url, body)
		}
	}
	if rec := httptest.NewRecorder(); true {
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/models/GLM-5.3-Flash-b0092d58/timeseries", nil))
		if rec.Code == 200 {
			t.Error("POST should not reach the endpoint")
		}
	}
}
