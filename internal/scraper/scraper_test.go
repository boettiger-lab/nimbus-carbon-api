package scraper

import (
	"testing"
	"time"
)

// TestAverage24h7d_HighSampleRate reproduces the live-confirmed bug: at a
// 30s scrape interval there are ~120 samples per hour bucket. The 24h power
// mean must equal the constant per-sample power, not be inflated by the
// number of samples accumulated into each hour's PowerSum.
func TestAverage24h7d_HighSampleRate(t *testing.T) {
	now := time.Now()
	const power = 11.0
	const promptTok = 2.0
	const genTok = 3.0
	const samplesPerHour = 120 // ~30s interval

	var buckets []avgBucket
	for hoursAgo := 0; hoursAgo < 3; hoursAgo++ {
		hour := now.Add(-time.Duration(hoursAgo) * time.Hour).Truncate(time.Hour).Unix()
		buckets = append(buckets, avgBucket{
			Hour:         hour,
			PowerSum:     power * samplesPerHour,
			PromptTokSum: promptTok * samplesPerHour,
			GenTokSum:    genTok * samplesPerHour,
			SampleCount:  samplesPerHour,
		})
	}

	avgs := average24h7d(now, buckets)

	if avgs.PowerAvg24h != power {
		t.Errorf("powerAvg24h = %v, want %v (bug: dividing by bucket count instead of sample count)", avgs.PowerAvg24h, power)
	}
	if avgs.PromptAvg24h != promptTok {
		t.Errorf("promptAvg24h = %v, want %v", avgs.PromptAvg24h, promptTok)
	}
	if avgs.GenAvg24h != genTok {
		t.Errorf("genAvg24h = %v, want %v", avgs.GenAvg24h, genTok)
	}
}

// TestAverage24h7d_NoData covers the zero-buckets case: no data yet should
// yield 0 for every field.
func TestAverage24h7d_NoData(t *testing.T) {
	now := time.Now()
	avgs := average24h7d(now, nil)

	if avgs != (windowAverages{}) {
		t.Errorf("expected all zeros for empty buckets, got %+v", avgs)
	}
}

// TestAverage24h7d_OneSamplePerBucket covers the case that accidentally
// worked before the fix: exactly one sample per hour bucket, where
// dividing by bucket count and dividing by sample count coincide. This
// guards against a regression in the other direction.
func TestAverage24h7d_OneSamplePerBucket(t *testing.T) {
	now := time.Now()
	const power = 42.0
	const promptTok = 5.0
	const genTok = 7.0

	var buckets []avgBucket
	for hoursAgo := 0; hoursAgo < 4; hoursAgo++ {
		hour := now.Add(-time.Duration(hoursAgo) * time.Hour).Truncate(time.Hour).Unix()
		buckets = append(buckets, avgBucket{
			Hour:         hour,
			PowerSum:     power,
			PromptTokSum: promptTok,
			GenTokSum:    genTok,
			SampleCount:  1,
		})
	}

	avgs := average24h7d(now, buckets)

	if avgs.PowerAvg24h != power {
		t.Errorf("powerAvg24h = %v, want %v", avgs.PowerAvg24h, power)
	}
	if avgs.PromptAvg24h != promptTok {
		t.Errorf("promptAvg24h = %v, want %v", avgs.PromptAvg24h, promptTok)
	}
	if avgs.GenAvg24h != genTok {
		t.Errorf("genAvg24h = %v, want %v", avgs.GenAvg24h, genTok)
	}
}

// TestAverage24h7d_CO2WeightedMeansUnaffected sanity-checks that the
// token-weighted CO2/token means (which were never buggy -- both numerator
// and denominator scale with sample count identically) still compute
// correctly through the extracted helper, across both the 24h and 7d
// windows.
func TestAverage24h7d_CO2WeightedMeansUnaffected(t *testing.T) {
	now := time.Now()

	buckets := []avgBucket{
		// within 24h window
		{
			Hour:        now.Truncate(time.Hour).Unix(),
			WeightedSum: 100, // e.g. 10 mg/tok * 10 tok, summed over many samples
			TokenSum:    10,
			SampleCount: 5,
		},
		// within 7d but outside 24h window
		{
			Hour:        now.Add(-48 * time.Hour).Truncate(time.Hour).Unix(),
			WeightedSum: 400,
			TokenSum:    20,
			SampleCount: 5,
		},
	}

	avgs := average24h7d(now, buckets)

	if avgs.CO2Avg24h != 10 {
		t.Errorf("co2Avg24h = %v, want 10", avgs.CO2Avg24h)
	}
	wantCO2Avg7d := 500.0 / 30.0 // (100+400) / (10+20), rounded to 3 decimals
	wantCO2Avg7d = float64(int(wantCO2Avg7d*1000+0.5)) / 1000
	if avgs.CO2Avg7d != wantCO2Avg7d {
		t.Errorf("co2Avg7d = %v, want %v", avgs.CO2Avg7d, wantCO2Avg7d)
	}
}

// TestAddSample_ActiveOnlyAveragesExcludeIdle checks that PowerWattsActive*
// and DecodeTokensPerSec* only average over non-idle samples: an idle sample
// (no tokens, no decode reading) must not drag down either mean, unlike the
// existing all-samples PowerWattsAvg24h.
func TestAddSample_ActiveOnlyAveragesExcludeIdle(t *testing.T) {
	now := time.Now()
	h := &modelHistory{}

	// One idle sample (low power, no traffic, no decode reading) ...
	h.addSample(now, 13.5, 0, 0, 0, 0.2)
	// ... and one active sample (higher power, real traffic, real decode speed).
	h.addSample(now, 140.0, 2, 50, 138.0, 0.2)

	avgs := average24h7d(now, h.AvgBuckets)

	if avgs.PowerAvg24h != 76.8 { // (13.5+140)/2 = 76.75, rounded to 1 decimal -- diluted by the idle sample
		t.Errorf("PowerAvg24h = %v, want 76.8 (all-samples mean should include idle)", avgs.PowerAvg24h)
	}
	if avgs.PowerActiveAvg24h != 140.0 {
		t.Errorf("PowerActiveAvg24h = %v, want 140 (idle sample must be excluded)", avgs.PowerActiveAvg24h)
	}
	if avgs.PromptActiveAvg24h != 2.0 {
		t.Errorf("PromptActiveAvg24h = %v, want 2 (idle sample must be excluded)", avgs.PromptActiveAvg24h)
	}
	if avgs.GenActiveAvg24h != 50.0 {
		t.Errorf("GenActiveAvg24h = %v, want 50 (idle sample must be excluded)", avgs.GenActiveAvg24h)
	}
	if avgs.DecodeAvg24h != 138.0 {
		t.Errorf("DecodeAvg24h = %v, want 138 (idle sample has no decode reading and must be excluded)", avgs.DecodeAvg24h)
	}
}
