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

	_, _, powerAvg24h, promptAvg24h, genAvg24h := average24h7d(now, buckets)

	if powerAvg24h != power {
		t.Errorf("powerAvg24h = %v, want %v (bug: dividing by bucket count instead of sample count)", powerAvg24h, power)
	}
	if promptAvg24h != promptTok {
		t.Errorf("promptAvg24h = %v, want %v", promptAvg24h, promptTok)
	}
	if genAvg24h != genTok {
		t.Errorf("genAvg24h = %v, want %v", genAvg24h, genTok)
	}
}

// TestAverage24h7d_NoData covers the zero-buckets case: no data yet should
// yield 0 for all five return values.
func TestAverage24h7d_NoData(t *testing.T) {
	now := time.Now()
	co2Avg24h, co2Avg7d, powerAvg24h, promptAvg24h, genAvg24h := average24h7d(now, nil)

	if co2Avg24h != 0 || co2Avg7d != 0 || powerAvg24h != 0 || promptAvg24h != 0 || genAvg24h != 0 {
		t.Errorf("expected all zeros for empty buckets, got co2Avg24h=%v co2Avg7d=%v powerAvg24h=%v promptAvg24h=%v genAvg24h=%v",
			co2Avg24h, co2Avg7d, powerAvg24h, promptAvg24h, genAvg24h)
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

	_, _, powerAvg24h, promptAvg24h, genAvg24h := average24h7d(now, buckets)

	if powerAvg24h != power {
		t.Errorf("powerAvg24h = %v, want %v", powerAvg24h, power)
	}
	if promptAvg24h != promptTok {
		t.Errorf("promptAvg24h = %v, want %v", promptAvg24h, promptTok)
	}
	if genAvg24h != genTok {
		t.Errorf("genAvg24h = %v, want %v", genAvg24h, genTok)
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

	co2Avg24h, co2Avg7d, _, _, _ := average24h7d(now, buckets)

	if co2Avg24h != 10 {
		t.Errorf("co2Avg24h = %v, want 10", co2Avg24h)
	}
	wantCO2Avg7d := 500.0 / 30.0 // (100+400) / (10+20), rounded to 3 decimals
	wantCO2Avg7d = float64(int(wantCO2Avg7d*1000+0.5)) / 1000
	if co2Avg7d != wantCO2Avg7d {
		t.Errorf("co2Avg7d = %v, want %v", co2Avg7d, wantCO2Avg7d)
	}
}
