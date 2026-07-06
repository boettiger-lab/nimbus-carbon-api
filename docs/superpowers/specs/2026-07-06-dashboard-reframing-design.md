# Dashboard reframing: fit nimbus's actual hardware and usage pattern

## Problem

The dashboard and its JS were forked from `nrp-carbon-api`, whose entire
narrative assumes a shared, always-busy, multi-tenant datacenter cluster: a
GPU sitting idle means wasted expensive shared capital other users are
queued for. nimbus is the opposite — one personal GB10, bursty single-user
traffic, and idling low is a hardware strength (power-gating working as
designed), not a failure.

Concretely, three problems trace back to this mismatch:

1. **Wrong framing.** The "vs. Commercial frontier" comparison drives card
   border color and an "efficiency verdict," and the J/token chart's own
   help text says "red = poor utilization (GPUs drawing power but
   producing few tokens)." But the frontier baseline assumes a 24-GPU
   cluster that draws 5,400 W just to keep models loaded — nimbus at full
   idle (11 W) already computes as ~480x more efficient under this
   comparison and always renders green. The comparison can't fail for
   nimbus, and it will make ordinary low-batch single-user traffic look
   "inefficient" for the wrong reason: personal single-stream inference is
   latency-bound, not throughput-bound, and no amount of "better
   utilization" changes that.
2. **J/token chart is always empty.** It filters for instantaneous
   `tokens_per_sec > 5` at the exact 30-second poll moment — a condition
   that's essentially never true for a personal box that's idle most of
   the time, even though it correctly generates real traffic in bursts.
3. **"Cluster CO₂ emissions over time" reads as empty.** Despite its title,
   it plots an instantaneous rate (kg CO₂/hr), not a cumulative total. For
   nimbus that rate is tiny (~2 g/hr) and nearly flat, so with
   `beginAtZero` the line hugs the bottom axis and looks blank.

Separately, the user wants relatable everyday reference frames (car miles,
appliance runtime, etc.) for the CO₂ numbers, in the style of CodeCarbon —
small absolute numbers like "2.2 g/hr" don't mean anything to most readers
on their own.

## Scope

Frontend-only: `cmd/static/dashboard.html` and `cmd/static/methodology.html`
in `boettiger-lab/nimbus-carbon-api`. No Go backend or API changes — every
field the redesign needs (`power_watts_avg_24h`,
`prompt_tokens_per_sec_avg_24h`, `generation_tokens_per_sec_avg_24h`, and
the `co2_grams_per_hour` timeseries) already exists in current API
responses.

## Changes

### 1. Demote the frontier comparison to a footnote

- Remove `energyRatioClass()`'s use as the model card's border/badge class.
  Drop the `ratio-great`/`ratio-good`/`ratio-warn`/`ratio-bad`/`ratio-idle`
  CSS classes and the colored left-border they drive — cards get a plain,
  neutral border. No efficiency verdict is rendered anywhere on the card.
- Keep `FRONTIER`, `frontierWatts()`, and the underlying math — they
  compute a real, favorable, technically defensible number. Render it as
  plain text only (no color, no judgment), e.g. "~480x less energy than a
  1.5T frontier model would use for the same output," and only when the
  model is actively generating (24h-avg prompt+generation tokens/sec above
  a small threshold, e.g. > 1 tok/s). While idle, this line is omitted
  entirely — not shown as good or bad, just not shown.
- The `token-cmp` power-comparison bar (nimbus watts vs. frontier watts)
  moves under the same "only while generating" gate, for the same reason.

### 2. Fix the J/token chart's data source and drop judgment coloring

- Change the bar-chart filter and computation from instantaneous
  `m.tokens_per_sec > 5` / `m.power_watts / m.tokens_per_sec` to the 24h
  averages: filter on `(m.prompt_tokens_per_sec_avg_24h +
  m.generation_tokens_per_sec_avg_24h) > 0.1`, compute
  `m.power_watts_avg_24h / (prompt_avg_24h + gen_avg_24h)`. This is the
  same data source the CO₂/token chart already uses successfully, so it's
  populated even when the model is idle at the exact poll moment.
- Recolor bars on a single-hue magnitude scale (the existing `--accent`
  indigo, opacity scaled by relative magnitude within the current bar set)
  instead of green-good/red-bad. Update `BAR_NOTES.joules` to describe
  J/token as a plain efficiency number, not a "poor utilization" verdict —
  something like: "Energy per token (J/token = watts ÷ tokens/sec) over
  the trailing 24h. Lower is more energy per output; this varies with
  batch size and workload, not just GPU health — a personal single-user
  box will naturally run higher J/token than a heavily-batched datacenter
  server, and that's expected, not a problem."

### 3. Switch the emissions chart from rate to cumulative total

- In `renderTimeSeries()`, before building `chartData`, integrate
  `co2_grams_per_hour` over the series using the rectangle rule: for each
  point, `grams_this_step = co2_grams_per_hour × (step_seconds / 3600)`,
  running-summed across all points in the selected range. `step_seconds`
  comes from the timeseries response's own `step` field (a Go-duration
  string like `"5m0s"`) via a small parser handling the `Xh`/`Xm`/`Xs`
  units the backend actually emits (`parseDuration`'s
  `step` values are always one of those three units — see
  `cmd/main.go`'s `handleTimeSeries`) — not from diffing consecutive
  points' `t` values, since a gap in the series (e.g. a pod restart) would
  silently produce a wrong per-step duration if derived that way.
- Plot the running cumulative sum as a monotonically non-decreasing line
  (it can only grow, never drop) instead of the current rate/mg-per-token
  fallback logic. Replace the `hasTokens` branch entirely — there's one
  dataset now, always populated as long as at least one timeseries point
  exists.
- Chart title/stat becomes "Total: `N` g CO₂e" (or kg, auto-scaled, for
  larger windows) for whichever range tab is active, replacing the
  "no active generation in range" fallback message.
- The existing mg/token-over-time signal (previously the chart's primary
  path when tokens were flowing) is dropped from this chart — it's now
  covered by the CO₂/token bar chart's 24h-avg bar instead, so this line
  chart has one clear job: cumulative emissions.

### 4. Add relatable reference frames

- New row of small stat tiles directly below the emissions chart, driven
  by the same cumulative total computed in change 3 (so switching the
  24h/7d/30d tabs updates both the chart and these tiles together).
- Four comparisons, each a simple division against a documented constant:
  - **Miles driven (gas car):** EPA average 404 g CO₂/mile.
  - **Smartphone charges:** ~8.22 g CO₂/charge.
  - **Refrigerator runtime:** ~150 W average draw → grams/hour, converted
    to minutes.
  - **Tree-months of absorption:** ~1.75 kg CO₂/month/mature tree —
    labeled in the UI and methodology page as the roughest of the four
    (real absorption varies significantly by species, age, and region).
- All four constants live as named JS constants near `FRONTIER` (same
  file), each with a one-line source comment, and get a corresponding
  entry in `methodology.html`'s reference table (mirroring how the EPA
  eGRID intensity is already cited there) so the numbers are traceable.

## Out of scope

- No backend/API changes — this is presentation-layer only.
- No all-time/lifetime cumulative counter — the reference frames use
  whichever window (24h/7d/30d) is currently selected, not a separate
  persistent total.
- No change to the model-card's absolute CO₂/hr coloring (`co2Color()`) —
  that's an absolute-magnitude indicator, not a comparison-based verdict,
  and isn't part of the framing problem this design addresses.
