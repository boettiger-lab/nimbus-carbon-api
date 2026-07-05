// Package carbon provides carbon-emission calculations for nimbus.
//
// nimbus is a single, fixed-location GB10 DGX Spark hosted in Berkeley, CA,
// on the CAMX eGRID subregion — the same California grid nrp-carbon-api
// uses as its own California/CAMX default. Unlike nrp-carbon-api (which
// spans institutions across the US and looks up intensity per node),
// nimbus has exactly one node, so the intensity is a fixed constant, not
// a lookup table.
//
// Reference: https://www.epa.gov/egrid
package carbon

// BerkeleyIntensity is the grid carbon intensity for Berkeley, CA (CAMX
// eGRID 2022 subregion).
const BerkeleyIntensity = 0.198 // kg CO2/kWh — CAMX (California)

// GramsPerHour returns grams of CO2 emitted per hour for a given
// power draw (watts) and grid carbon intensity (kg CO2/kWh).
//
//	g/hr = W / 1000 kW  ×  intensity kg/kWh  ×  1000 g/kg
//	     = W × intensity × 1.0
func GramsPerHour(powerWatts, intensityKgPerKWh float64) float64 {
	return powerWatts * intensityKgPerKWh
}

// MgPerToken returns milligrams of CO2 per token (total: prompt + generation).
//
//	mg/token = (W / 3.6e6 kWh/s) × intensity kg/kWh × 1e6 mg/kg / (tokens/s)
//	         = W × intensity × (1e6 / 3.6e6) / tokensPerSec
//	         = W × intensity × 0.2778 / tokensPerSec
func MgPerToken(powerWatts, intensityKgPerKWh, tokensPerSec float64) float64 {
	if tokensPerSec <= 0 {
		return 0
	}
	return powerWatts * intensityKgPerKWh * 0.2778 / tokensPerSec
}
