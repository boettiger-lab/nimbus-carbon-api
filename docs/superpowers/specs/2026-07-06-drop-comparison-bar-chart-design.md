# Drop the model-comparison bar chart

## Problem

The "CO₂ per token by model" chart (both its `co2` and `joules` toggle
modes) colors bars relative to the *current bar set's own maximum*:

```js
const maxCo2 = Math.max(...avg24h, 0.001);
const t = v / maxCo2;   // co2 mode
...
const maxJ = Math.max(...jPerToken, 1);
const t = j / maxJ;     // joules mode
```

With exactly one bar, `t` is always `value / value = 1` — the single bar
renders at the most intense color tier every time, regardless of whether
the actual value is 0.001 mg or 100 mg. This isn't a threshold to retune;
it's a structural property of self-relative normalization with N=1. And
nimbus will almost always show exactly one active model, since it's a
single-GPU personal box, not a multi-tenant cluster with many models
running side by side. A comparison chart's entire reason to exist is
comparing multiple things — with one thing, it can only produce a
misleading result, dressed up as data.

This was found live: qwen showed a solid red bar for a CO₂/token value of
0.0010 mg — a quantity with no meaningful definition of "high," rendered as
if it were the worst possible reading.

Root cause note: the previous redesign (fixing the frontier-comparison
verdict and the J/token chart's empty-data problem) removed the old
green/red judgment coloring from the `joules` mode but replaced it with a
new self-relative magnitude scale that has the exact same N=1 flaw, just
with a less alarming color. The mistake wasn't "wrong color," it was
building any per-bar-set-relative visualization for a metric that's
usually shown with one bar.

## Decision

Drop the entire "CO₂ per token by model" chart section — both toggle
modes. Move its most useful signal (J/token) onto the model card as one
more plain metric tile, matching how CO₂/hr, GPU Power, CO₂/token, and the
frontier-comparison footnote already render there: a number, with `—` when
there isn't enough data, no color judgment, no comparison framing.

This is not a loss of information: CO₂/token is *already* shown as a plain
tile on the card today (independent of the bar chart), and the bar chart's
scatter-point "current 5-min rate" overlay duplicates the same
`m.co2_mg_per_token` field the card tile already reads. Nothing on the
chart says anything the card doesn't already say — minus the misleading
color.

## What gets removed

From `cmd/static/dashboard.html`:
- The second `<div class="chart-wrap">` block (the one containing
  `#bar-title`, `#bar-mode-tabs`, `#bar-note`, `<canvas id="barChart">`).
- `renderBarChart()` in full (both `co2` and `joules` branches).
- `BAR_NOTES`, `BAR_TITLES`.
- The `#bar-mode-tabs` click-event-listener block.
- The `barChart` and `barMode` module-level `let` declarations.
- The call site `renderBarChart(models);` inside `render()`.
- `barChart` from the `applyChartTheme()` iteration array (it only ever
  themes `[barChart, lineChart]`; becomes just `[lineChart]`).
- The `.workload-note` CSS rule (used only by the removed chart's note).

## What gets added

To the model card's `.metrics` grid in `render()`: a J/token tile computed
the same way the removed chart computed it (`power_watts_avg_24h /
(prompt_tokens_per_sec_avg_24h + generation_tokens_per_sec_avg_24h)`),
gated the same way (needs `> 0.1` combined tok/s and `power_watts_avg_24h >
0`), rendering `—` when the gate fails — matching the existing
`Input tok/s`/`Output tok/s` tiles' `active?fmt(...):'—'` pattern.

## methodology.html updates

- "Dashboard visualizations" section (the bullet list describing the CO₂/token
  bar chart with its green→red coloring) gets rewritten: CO₂/token and
  J/token are both plain per-model card stats now, not chart bars, and
  neither is colored by a comparative scale.
- "Energy per token across models (J/token view)" section gets rewritten
  to describe the plain card tile instead of a chart/toggle.
- New Limitations bullet: history (24h/7d averages, the cumulative
  emissions chart) is in-memory only and resets on every pod restart,
  backfilled only from Prometheus's own ~2-day retention. Persistent
  storage is a possible future improvement, out of scope here.

## Out of scope

- Adding persistence for history across restarts (deferred; noted as a
  limitation instead).
- Any change to the cumulative-emissions line chart or the reference-frame
  tiles — both already avoid this self-relative-coloring problem (the line
  chart plots one series over time, not multiple bars against each other;
  the reference frames are absolute conversions, not comparisons).
- Any change to the frontier-comparison footnote or its gating — already
  fixed in the prior redesign, not touched here.
