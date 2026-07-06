# Dashboard Reframing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the dashboard's framing and two empty charts to fit nimbus's actual hardware and bursty single-user usage pattern, add relatable everyday CO2 reference frames, and fix the live token-rate window so reported tok/s matches reality.

**Architecture:** Five independent changes — four in the static dashboard HTML/JS (`cmd/static/dashboard.html`, `cmd/static/methodology.html`) and one in the Go backend (`internal/scraper/scraper.go`). No API shape changes: every frontend change consumes fields the API already returns.

**Tech Stack:** Vanilla JS + Chart.js (dashboard), Go 1.22 stdlib (backend).

## Global Constraints

- Repo: `/home/cboettig/Documents/github/boettiger-lab/nimbus-carbon-api`, branch `main` (this repo has used direct-to-main throughout, no feature branches).
- Tasks 1-4 touch only `cmd/static/dashboard.html` and `cmd/static/methodology.html` — no Go changes, no API shape changes.
- Task 5 touches only `internal/scraper/scraper.go` — no HTML/JS changes. Must keep `go build ./...`, `go vet ./...`, and the existing `internal/carbon`/`internal/scraper` test suites green.
- No new JS test framework exists or should be added — verify frontend changes via direct `curl`/browser checks against the live dashboard once deployed (Task 6), matching how the dashboard rebrand was verified in the prior plan.
- Drop all "efficiency verdict" coloring tied to the frontier comparison (`ratio-great`/`ratio-good`/`ratio-warn`/`ratio-bad`/`ratio-idle` CSS classes and `energyRatioClass()`) — no card border or badge should render green/red based on utilization anywhere after this plan.
- The J/token bar chart's color scheme becomes a single-hue magnitude scale (existing `--accent` indigo `#6366f1`, opacity-scaled) — never green/red.
- Live Prometheus fact, verified before writing this plan: `rate(vllm:generation_tokens_total[5m])` reads real instantaneous decode speeds of 30-133 tok/s as 0-5 tok/s during and shortly after a burst. The fix is `[2m]` in `queryTokens()` only — leave `backfill()` and `ClusterTimeSeries()`'s `[5m]` windows untouched (they feed long-run averages, not live snapshots).
- Reference-frame conversion factors (cite these exact values, not re-derived ones): 404 g CO₂/mile (EPA, gas car), 8.22 g CO₂/phone charge, 150 W average refrigerator draw (converted using nimbus's own 0.198 kg CO₂/kWh grid intensity), 1.75 kg CO₂/month/tree.

---

### Task 1: Demote the frontier comparison to a conditional footnote

**Files:**
- Modify: `cmd/static/dashboard.html`

**Interfaces:**
- Consumes: `m.power_watts_avg_24h`, `m.prompt_tokens_per_sec_avg_24h`, `m.generation_tokens_per_sec_avg_24h` (already in the API response).
- Produces: nothing later tasks depend on — this task only removes/reworks existing rendering logic.

- [ ] **Step 1: Remove the `ratio-*` CSS rules**

Find this block:

```css
  .card{background:var(--surface);border:1px solid var(--border);border-radius:.75rem;padding:1.1rem;border-left:3px solid var(--border);}
  .card.ratio-great{border-left-color:var(--green);}   /* < 50% of commercial */
  .card.ratio-good {border-left-color:#86efac;}         /* 50–80% of commercial */
  .card.ratio-warn {border-left-color:var(--orange);}   /* 80–100%: at commercial level */
  .card.ratio-bad  {border-left-color:var(--red);}      /* > 100%: worse than commercial */
  .card.ratio-idle {border-left-color:var(--border);}   /* no token data (idle) */
```

Replace with just the base rule (delete the five `ratio-*` overrides — cards get a plain neutral border, no efficiency verdict):

```css
  .card{background:var(--surface);border:1px solid var(--border);border-radius:.75rem;padding:1.1rem;border-left:3px solid var(--border);}
```

- [ ] **Step 2: Remove the `token-cmp-ratio` color classes**

Find:

```css
  .token-cmp-ratio{font-size:.72rem;margin-top:.3rem;font-weight:600;}
  .token-cmp-ratio.better{color:var(--green);}
  .token-cmp-ratio.worse {color:var(--orange);}
```

Replace with (keep the base rule, drop the judgment colors — this text becomes plain, informational, colored the same as normal body text):

```css
  .token-cmp-ratio{font-size:.72rem;margin-top:.3rem;font-weight:600;color:var(--text);}
```

- [ ] **Step 3: Delete `energyRatioClass()`**

Find and delete this whole function (nothing else calls it after this task):

```js
// Card border: energy ratio (nimbus / Commercial frontier equivalent).
// Uses 24h averages to match the CO₂/token bar chart window; falls back to
// instantaneous 5-min values for pods that don't yet have 24h history.
function energyRatioClass(m) {
  const nrpW = m.power_watts_avg_24h || m.power_watts;
  if (nrpW < 1) return 'ratio-idle';
  const prompt = m.prompt_tokens_per_sec_avg_24h ?? m.prompt_tokens_per_sec ?? 0;
  const gen = m.generation_tokens_per_sec_avg_24h ?? m.generation_tokens_per_sec ?? 0;
  const fw = frontierWatts(prompt, gen);
  const r = nrpW / fw;
  if (r < 0.15) return 'ratio-great';
  if (r < 0.35) return 'ratio-good';
  if (r < 0.70) return 'ratio-warn';
  return 'ratio-bad';
}
```

- [ ] **Step 4: Remove the `ic` variable and its use on the card**

In `render()`, find:

```js
  document.getElementById('cards').innerHTML = models.map(m=>{
    const active = m.tokens_per_sec > 0.1;
    const ic  = energyRatioClass(m);
    const cc  = co2Color(m.co2_grams_per_hour);
```

Replace with (drop the `ic` line — `cc`, the absolute CO₂/hr color, is unrelated and unaffected):

```js
  document.getElementById('cards').innerHTML = models.map(m=>{
    const active = m.tokens_per_sec > 0.1;
    const cc  = co2Color(m.co2_grams_per_hour);
```

Then find:

```js
    return `
    <div class="card ${ic}">
```

Replace with:

```js
    return `
    <div class="card">
```

- [ ] **Step 5: Gate the frontier comparison on active generation, drop color judgment**

Find this whole block:

```js
    // Watts comparison: nimbus measured vs Commercial frontier equivalent for the same tokens.
    // Use 24h averages so this matches the CO₂/token bar chart window; fall back to
    // instantaneous 5-min values for pods without 24h history yet.
    const avgPrompt = m.prompt_tokens_per_sec_avg_24h ?? m.prompt_tokens_per_sec ?? 0;
    const avgGen    = m.generation_tokens_per_sec_avg_24h ?? m.generation_tokens_per_sec ?? 0;
    const fw   = frontierWatts(avgPrompt, avgGen);
    const nrpW = m.power_watts_avg_24h || m.power_watts;
    const has24h = m.power_watts_avg_24h != null && m.power_watts_avg_24h > 0;
    const windowLabel = has24h ? '24h avg' : '5-min';
    let wattsCmp = '';
    if (nrpW > 0) {
      const ratio = fw / nrpW;
      const nrpPct = Math.min(nrpW / fw * 100, 100).toFixed(1);
      const ratioText = ratio >= 1.05
        ? `${ratio.toFixed(1)}× less energy than ${FRONTIER.label} for same tokens`
        : ratio < 0.95
          ? `${(1/ratio).toFixed(1)}× more energy than ${FRONTIER.label}`
          : `≈ same energy as ${FRONTIER.label}`;
      const ratioClass = ratio >= 1.05 ? 'better' : 'worse';
      const fIdleW = FRONTIER_IDLE_W;
      const fMargW = fw - fIdleW;
      wattsCmp = `
        <div class="token-cmp">
          <div class="token-cmp-label">Power (${windowLabel}): nimbus vs. ${FRONTIER.label} for same tokens</div>
          <div class="token-cmp-track">
            <div class="token-cmp-bar" style="width:${nrpPct}%;background:#22c55e;top:2px;height:14px;border-radius:2px"></div>
            <div style="position:absolute;top:2px;left:0;width:100%;height:14px;border-radius:2px;background:rgba(99,102,241,0.18)"></div>
          </div>
          <div class="token-cmp-legend">
            <span style="color:#22c55e">nimbus: ${nrpW.toLocaleString(undefined,{maximumFractionDigits:0})} W</span>
            <span style="color:#6366f1">${FRONTIER.label}: ${fw.toLocaleString(undefined,{maximumFractionDigits:0})} W <span style="opacity:.6">(${fIdleW.toLocaleString()} idle + ${fMargW.toLocaleString(undefined,{maximumFractionDigits:0})} marginal)</span></span>
          </div>
          <div class="token-cmp-ratio ${ratioClass}">${ratioText}</div>
        </div>`;
    }
```

Replace with (adds the `isGenerating24h` gate, drops `ratioClass`, and produces `frontierMetric` for the metrics-grid tile in Step 6):

```js
    // Frontier-model comparison: shown only while actively generating (24h-avg
    // combined tok/s > 1) — while idle this is omitted entirely, not shown as
    // good or bad. Plain informational text/visualization only, no color
    // verdict: nimbus's absolute power draw is real regardless of how it
    // compares to a hypothetical 24-GPU commercial cluster, and idling low is
    // expected behavior for this hardware, not something to judge either way.
    const avgPrompt = m.prompt_tokens_per_sec_avg_24h ?? m.prompt_tokens_per_sec ?? 0;
    const avgGen    = m.generation_tokens_per_sec_avg_24h ?? m.generation_tokens_per_sec ?? 0;
    const isGenerating24h = (avgPrompt + avgGen) > 1;
    const fw   = frontierWatts(avgPrompt, avgGen);
    const nrpW = m.power_watts_avg_24h || m.power_watts;
    const has24h = m.power_watts_avg_24h != null && m.power_watts_avg_24h > 0;
    const windowLabel = has24h ? '24h avg' : '5-min';
    let wattsCmp = '';
    let frontierMetric = '';
    if (isGenerating24h && nrpW > 0) {
      const ratio = fw / nrpW;
      const nrpPct = Math.min(nrpW / fw * 100, 100).toFixed(1);
      const ratioText = ratio >= 1.05
        ? `${ratio.toFixed(1)}× less energy than ${FRONTIER.label} for same tokens`
        : ratio < 0.95
          ? `${(1/ratio).toFixed(1)}× more energy than ${FRONTIER.label}`
          : `≈ same energy as ${FRONTIER.label}`;
      const fIdleW = FRONTIER_IDLE_W;
      const fMargW = fw - fIdleW;
      wattsCmp = `
        <div class="token-cmp">
          <div class="token-cmp-label">Power (${windowLabel}): nimbus vs. ${FRONTIER.label} for same tokens</div>
          <div class="token-cmp-track">
            <div class="token-cmp-bar" style="width:${nrpPct}%;background:#6366f1;top:2px;height:14px;border-radius:2px"></div>
            <div style="position:absolute;top:2px;left:0;width:100%;height:14px;border-radius:2px;background:rgba(99,102,241,0.18)"></div>
          </div>
          <div class="token-cmp-legend">
            <span style="color:var(--text)">nimbus: ${nrpW.toLocaleString(undefined,{maximumFractionDigits:0})} W</span>
            <span style="color:#6366f1">${FRONTIER.label}: ${fw.toLocaleString(undefined,{maximumFractionDigits:0})} W <span style="opacity:.6">(${fIdleW.toLocaleString()} idle + ${fMargW.toLocaleString(undefined,{maximumFractionDigits:0})} marginal)</span></span>
          </div>
          <div class="token-cmp-ratio">${ratioText}</div>
        </div>`;
      frontierMetric = `<div class="metric"><div class="metric-label">vs. ${FRONTIER.label}</div><div class="metric-value">${fmt(fw/nrpW,1)}×</div><div class="metric-unit">less energy · ${windowLabel}</div></div>`;
    }
```

- [ ] **Step 6: Splice `frontierMetric` into the metrics grid**

Find:

```html
      <div class="metrics">
        <div class="metric"><div class="metric-label">CO₂ / hr</div><div class="metric-value ${cc}">${fmt(m.co2_grams_per_hour)}</div><div class="metric-unit">grams</div></div>
        <div class="metric"><div class="metric-label">GPU Power</div><div class="metric-value">${fmt(m.power_watts)}</div><div class="metric-unit">watts</div></div>
        <div class="metric"><div class="metric-label">CO₂ / token</div><div class="metric-value">${m.co2_mg_per_token?fmt(m.co2_mg_per_token,3):'—'}</div><div class="metric-unit">mg · 5 min${m.co2_mg_per_token_avg_24h ? ` · <span style="color:var(--muted)">24h avg: ${fmt(m.co2_mg_per_token_avg_24h,3)}</span>` : ''}</div></div>
        <div class="metric"><div class="metric-label">Input tok/s</div><div class="metric-value">${active?fmt(m.prompt_tokens_per_sec):'—'}</div><div class="metric-unit">tok/s prompt</div></div>
        <div class="metric"><div class="metric-label">Output tok/s</div><div class="metric-value">${active?fmt(m.generation_tokens_per_sec):'—'}</div><div class="metric-unit">tok/s generated</div></div>
        <div class="metric"><div class="metric-label">vs. ${FRONTIER.label}</div><div class="metric-value ${fw/nrpW>=1.05?'green':fw/nrpW<0.95?'red':''}">${nrpW>0?fmt(fw/nrpW,1)+'×':'—'}</div><div class="metric-unit">${fw/nrpW>=1.05?'less energy':'more energy'} · ${windowLabel}</div></div>
      </div>
      ${wattsCmp}
```

Replace with:

```html
      <div class="metrics">
        <div class="metric"><div class="metric-label">CO₂ / hr</div><div class="metric-value ${cc}">${fmt(m.co2_grams_per_hour)}</div><div class="metric-unit">grams</div></div>
        <div class="metric"><div class="metric-label">GPU Power</div><div class="metric-value">${fmt(m.power_watts)}</div><div class="metric-unit">watts</div></div>
        <div class="metric"><div class="metric-label">CO₂ / token</div><div class="metric-value">${m.co2_mg_per_token?fmt(m.co2_mg_per_token,3):'—'}</div><div class="metric-unit">mg · 5 min${m.co2_mg_per_token_avg_24h ? ` · <span style="color:var(--muted)">24h avg: ${fmt(m.co2_mg_per_token_avg_24h,3)}</span>` : ''}</div></div>
        <div class="metric"><div class="metric-label">Input tok/s</div><div class="metric-value">${active?fmt(m.prompt_tokens_per_sec):'—'}</div><div class="metric-unit">tok/s prompt</div></div>
        <div class="metric"><div class="metric-label">Output tok/s</div><div class="metric-value">${active?fmt(m.generation_tokens_per_sec):'—'}</div><div class="metric-unit">tok/s generated</div></div>
        ${frontierMetric}
      </div>
      ${wattsCmp}
```

- [ ] **Step 7: Update methodology.html's "Dashboard visualizations" section**

In `cmd/static/methodology.html`, find:

```html
<h3>Dashboard visualizations</h3>
<p>
  The dashboard presents the frontier comparison in two ways:
</p>
<ul style="color:#cbd5e1; padding-left:1.5rem; margin:0.5rem 0 1rem;">
  <li><strong>CO₂/token bar chart:</strong> Bars show the <strong>token-weighted</strong>
  24-hour average CO₂ per token (mg) for each active nimbus model. Each 30-second sample is
  weighted by its throughput (tok/s), so periods of high utilization contribute
  proportionally more to the average than brief low-throughput transitions. This prevents
  ramp-up/ramp-down periods (where CO₂/token is transiently extreme due to idle power
  divided by few tokens) from dominating the long-run average. Green diamond (◆) markers
  show the current 5-minute rate.
  Bar color ranges from <span style="color:#22c55e">green</span> (low carbon per token) to
  <span style="color:#ef4444">red</span> (high) — reflecting both energy efficiency and
  grid carbon intensity at the model's location.</li>
  <li><strong>Model card "N× less energy" metric:</strong> Each model card shows how many
  times more power the commercial frontier would require to serve the same tokens
  (e.g. "7.0× less energy"). This is <code>frontier_watts / nrp_measured_watts</code>.</li>
  <li><strong>Power comparison bar (per card):</strong> A horizontal bar comparing nimbus
  measured watts (green) against the frontier equivalent (purple band), with a breakdown
  of idle floor vs. marginal power.</li>
</ul>
```

Replace with (adds the "only while generating" caveat to the two frontier-specific bullets; the CO₂/token bar chart bullet is unaffected by this task and stays as-is):

```html
<h3>Dashboard visualizations</h3>
<p>
  The dashboard presents the frontier comparison in two ways, both shown only
  while the model is actively generating (24h-avg combined tok/s > 1) — while
  idle, neither is shown, since there is nothing to compare and idling low is
  expected behavior for this hardware, not something to judge either way:
</p>
<ul style="color:#cbd5e1; padding-left:1.5rem; margin:0.5rem 0 1rem;">
  <li><strong>CO₂/token bar chart:</strong> Bars show the <strong>token-weighted</strong>
  24-hour average CO₂ per token (mg) for each active nimbus model. Each 30-second sample is
  weighted by its throughput (tok/s), so periods of high utilization contribute
  proportionally more to the average than brief low-throughput transitions. This prevents
  ramp-up/ramp-down periods (where CO₂/token is transiently extreme due to idle power
  divided by few tokens) from dominating the long-run average. Green diamond (◆) markers
  show the current 5-minute rate.
  Bar color ranges from <span style="color:#22c55e">green</span> (low carbon per token) to
  <span style="color:#ef4444">red</span> (high) — reflecting both energy efficiency and
  grid carbon intensity at the model's location. (Unlike the two comparisons below, this
  bar always renders — it's an absolute CO₂/token measurement, not a frontier comparison.)</li>
  <li><strong>Model card "N× less energy" metric:</strong> Shown only while actively
  generating. Each model card shows how many times more power the commercial frontier
  would require to serve the same tokens (e.g. "7.0× less energy"). This is
  <code>frontier_watts / nimbus_measured_watts</code>. There is no color judgment on this
  number — it's informational, not a verdict.</li>
  <li><strong>Power comparison bar (per card):</strong> Shown only while actively
  generating. A horizontal bar comparing nimbus measured watts against the frontier
  equivalent (purple band), with a breakdown of idle floor vs. marginal power.</li>
</ul>
```

- [ ] **Step 8: Manually verify locally**

```bash
cd /home/cboettig/Documents/github/boettiger-lab/nimbus-carbon-api
grep -n "ratio-great\|ratio-good\|ratio-warn\|ratio-bad\|ratio-idle\|energyRatioClass" cmd/static/dashboard.html
```

Expected: no output (all removed).

```bash
grep -n "class=\"token-cmp-ratio" cmd/static/dashboard.html
```

Expected: one line, `<div class="token-cmp-ratio">${ratioText}</div>` — no `better`/`worse` suffix.

- [ ] **Step 9: Commit**

```bash
git add cmd/static/dashboard.html cmd/static/methodology.html
git commit -m "dashboard: demote frontier comparison to a conditional footnote

Drops the ratio-*/energyRatioClass card-color verdict entirely -- it
structurally could never render red for nimbus (baseline assumes a
24-GPU cluster drawing 5,400W idle) and mis-framed ordinary low-batch
single-user traffic as inefficient. The frontier comparison is now
shown only while actively generating, as plain informational text with
no color judgment either way."
git push
```

---

### Task 2: Fix the J/token chart's data source and drop judgment coloring

**Files:**
- Modify: `cmd/static/dashboard.html`
- Modify: `cmd/static/methodology.html`

**Interfaces:**
- Consumes: `m.power_watts_avg_24h`, `m.prompt_tokens_per_sec_avg_24h`, `m.generation_tokens_per_sec_avg_24h` (already in the API response). Independent of Task 1 — touches a different function (`renderBarChart`'s `else` branch), safe to implement in either order, but this plan runs it second.

- [ ] **Step 1: Replace the J/token branch of `renderBarChart()`**

Find this whole block (the `else` branch — the `if (barMode === 'co2')` branch above it is untouched):

```js
  } else {
    // J/token mode: power_watts / total_tokens_per_sec
    const bm = models.filter(m => m.tokens_per_sec > 5).slice().sort((a,b) => {
      return (a.power_watts / a.tokens_per_sec) - (b.power_watts / b.tokens_per_sec);
    });
    const labels = bm.map(m => shortModel(m.model_name) || m.container);
    const jPerToken = bm.map(m => +(m.power_watts / m.tokens_per_sec).toFixed(3));
    // Color by efficiency: lower J/token = greener
    const maxJ = Math.max(...jPerToken, 1);
    const colors = jPerToken.map(j => {
      const t = Math.min(j / maxJ, 1);
      if (t < 0.25) return 'rgba(34,197,94,0.8)';
      if (t < 0.50) return 'rgba(134,239,172,0.8)';
      if (t < 0.75) return 'rgba(249,115,22,0.7)';
      return 'rgba(239,68,68,0.7)';
    });
    // GPU type annotation for context
    const gpuLabels = bm.map(m => `${m.gpu_count}× ${m.gpu_hardware||'?'}`);
    barChart = new Chart(document.getElementById('barChart'), {
      type: 'bar',
      data: { labels, datasets: [
        { label:'J / token', data:jPerToken, backgroundColor:colors, borderRadius:4 },
      ]},
      options: {
        responsive:true,
        plugins: {
          legend:{ display:false },
          tooltip:{ callbacks:{
            title: (items) => { const i=items[0].dataIndex; return `${labels[i]} (${gpuLabels[i]})`; },
            label: c => ` ${c.parsed.y.toFixed(3)} J/token  ·  ${bm[c.dataIndex].power_watts.toFixed(0)} W  ·  ${bm[c.dataIndex].tokens_per_sec.toFixed(0)} tok/s`
          }}
        },
        scales: {
          x:{ ticks:{color:cc0.tick,font:{size:10}}, grid:{color:cc0.grid} },
          y:{ ticks:{color:cc0.tick,font:{size:10}}, grid:{color:cc0.grid}, title:{display:true,text:'Joules / token',color:cc0.tick,font:{size:10}}, beginAtZero:true }
        }
      }
    });
  }
```

Replace with:

```js
  } else {
    // J/token mode: power_watts_avg_24h / (prompt+gen)_tokens_per_sec_avg_24h.
    // Uses 24h averages (same source the CO₂/token chart uses) rather than
    // instantaneous tokens_per_sec, which is almost never >5 for bursty
    // personal-use traffic even though real generation happened in the window.
    const bm = models.filter(m => {
      const tok = (m.prompt_tokens_per_sec_avg_24h||0) + (m.generation_tokens_per_sec_avg_24h||0);
      return tok > 0.1 && m.power_watts_avg_24h > 0;
    }).slice().sort((a,b) => {
      const jA = a.power_watts_avg_24h / ((a.prompt_tokens_per_sec_avg_24h||0)+(a.generation_tokens_per_sec_avg_24h||0));
      const jB = b.power_watts_avg_24h / ((b.prompt_tokens_per_sec_avg_24h||0)+(b.generation_tokens_per_sec_avg_24h||0));
      return jA - jB;
    });
    const labels = bm.map(m => shortModel(m.model_name) || m.container);
    const jPerToken = bm.map(m => {
      const tok = (m.prompt_tokens_per_sec_avg_24h||0) + (m.generation_tokens_per_sec_avg_24h||0);
      return +(m.power_watts_avg_24h / tok).toFixed(3);
    });
    // Neutral magnitude scale (single accent hue, no green/red judgment) —
    // J/token reflects batch size and workload as much as GPU health; a
    // personal single-user box naturally runs higher J/token than a
    // heavily-batched datacenter server, and that's expected, not a problem.
    const maxJ = Math.max(...jPerToken, 1);
    const colors = jPerToken.map(j => {
      const t = Math.min(j / maxJ, 1);
      const alpha = 0.35 + t * 0.5;
      return `rgba(99,102,241,${alpha.toFixed(2)})`;
    });
    // GPU type annotation for context
    const gpuLabels = bm.map(m => `${m.gpu_count}× ${m.gpu_hardware||'?'}`);
    barChart = new Chart(document.getElementById('barChart'), {
      type: 'bar',
      data: { labels, datasets: [
        { label:'J / token (24h avg)', data:jPerToken, backgroundColor:colors, borderRadius:4 },
      ]},
      options: {
        responsive:true,
        plugins: {
          legend:{ display:false },
          tooltip:{ callbacks:{
            title: (items) => { const i=items[0].dataIndex; return `${labels[i]} (${gpuLabels[i]})`; },
            label: c => {
              const mm = bm[c.dataIndex];
              const tok = (mm.prompt_tokens_per_sec_avg_24h||0) + (mm.generation_tokens_per_sec_avg_24h||0);
              return ` ${c.parsed.y.toFixed(3)} J/token  ·  ${mm.power_watts_avg_24h.toFixed(1)} W avg  ·  ${tok.toFixed(1)} tok/s avg (24h)`;
            }
          }}
        },
        scales: {
          x:{ ticks:{color:cc0.tick,font:{size:10}}, grid:{color:cc0.grid} },
          y:{ ticks:{color:cc0.tick,font:{size:10}}, grid:{color:cc0.grid}, title:{display:true,text:'Joules / token (24h avg)',color:cc0.tick,font:{size:10}}, beginAtZero:true }
        }
      }
    });
  }
```

- [ ] **Step 2: Update the J/token help text**

Find:

```js
  joules: 'Energy per token (J/token = watts ÷ tokens/sec), independent of grid location. Bar color: <span style="color:var(--green)">green</span> = efficient (high utilization, good tok/s per watt), <span style="color:var(--red)">red</span> = poor utilization (GPUs drawing power but producing few tokens). <a href="/methodology" style="color:var(--accent)">Methodology</a>',
```

Replace with:

```js
  joules: 'Energy per token (J/token = watts ÷ tokens/sec) over the trailing 24h, independent of grid location. Lower means more output per watt — but this reflects batch size and workload as much as GPU health: a personal single-user box naturally runs higher J/token than a heavily-batched datacenter server, and that is expected, not a problem. <a href="/methodology" style="color:var(--accent)">Methodology</a>',
```

- [ ] **Step 3: Rewrite methodology.html's J/token section**

Find this whole block:

```html
<h3>Energy efficiency across models (J/token view)</h3>
<p>
  The bar chart can be toggled to show <strong>J/token</strong> (joules per token) —
  a hardware- and grid-neutral measure of how efficiently each model converts GPU power
  into tokens:
</p>
<pre><code>J_per_token = power_watts / total_tokens_per_sec</code></pre>
<p>
  Unlike CO₂/token, this metric is independent of grid carbon intensity, making it
  directly comparable across models regardless of where they are hosted. A model
  with high J/token is underutilized relative to its GPU allocation — the GPUs draw
  near-idle power while producing few tokens. Key drivers of high J/token include:
</p>
<ul style="color:#cbd5e1; padding-left:1.5rem; margin:0.5rem 0 1rem;">
  <li><strong>Low traffic:</strong> GPU idle power dominates when few tokens are being processed</li>
  <li><strong>Oversized allocation:</strong> More GPUs than needed for the model's actual demand</li>
  <li><strong>Model architecture:</strong> Dense models activate all parameters on every token,
  while Mixture-of-Experts (MoE) models route each token through a subset (~20%) of total
  weights. A dense 31B model may therefore have higher J/token than a 397B MoE, because the
  MoE only activates ~60B parameters per token</li>
</ul>
<div class="callout">
  <p><strong>Note:</strong> J/token only appears for models with &gt;5 tok/s total throughput.
  Below this threshold the metric is dominated by idle power and not meaningful as an
  efficiency measure. Bar color ranges from green (most efficient) to red (least efficient)
  relative to the other active nimbus models. Tooltips show the underlying watts, tok/s, and
  GPU hardware for context.</p>
</div>
```

Replace with:

```html
<h3>Energy per token across models (J/token view)</h3>
<p>
  The bar chart can be toggled to show <strong>J/token</strong> (joules per token) —
  a hardware- and grid-neutral measure of energy spent per token, computed from
  24-hour averages (the same window the CO₂/token chart uses):
</p>
<pre><code>J_per_token = power_watts_avg_24h / (prompt_tokens_per_sec_avg_24h + generation_tokens_per_sec_avg_24h)</code></pre>
<p>
  Unlike CO₂/token, this metric is independent of grid carbon intensity, making it
  directly comparable across models regardless of where they are hosted. J/token reflects
  batch size and workload as much as it reflects GPU health — it is <strong>not</strong> a
  "utilization" verdict. A personal, single-user box naturally runs higher J/token than a
  heavily-batched datacenter server, because personal single-stream inference is
  latency-bound, not throughput-bound: there's no queue of other requests to batch
  alongside, and there shouldn't be. Key drivers of J/token include:
</p>
<ul style="color:#cbd5e1; padding-left:1.5rem; margin:0.5rem 0 1rem;">
  <li><strong>Batch size:</strong> more concurrent requests amortize the same idle power
  over more tokens — a difference in workload, not a problem to fix on a personal box</li>
  <li><strong>Traffic level:</strong> idle power is a larger share of the total when fewer
  tokens are being processed in a given window</li>
  <li><strong>Model architecture:</strong> Dense models activate all parameters on every token,
  while Mixture-of-Experts (MoE) models route each token through a subset (~20%) of total
  weights. A dense 31B model may therefore have higher J/token than a 397B MoE, because the
  MoE only activates ~60B parameters per token</li>
</ul>
<div class="callout">
  <p><strong>Note:</strong> J/token only appears for models with combined 24h-avg
  prompt+generation throughput above 0.1 tok/s. Bars are colored on a single-hue magnitude
  scale (no green/red judgment) — lower is more energy per output, but is not itself
  "better" or "worse" without knowing the workload. Tooltips show the underlying 24h-avg
  watts, tok/s, and GPU hardware for context.</p>
</div>
```

- [ ] **Step 4: Manually verify locally**

```bash
cd /home/cboettig/Documents/github/boettiger-lab/nimbus-carbon-api
grep -n "poor utilization\|underutilized" cmd/static/dashboard.html cmd/static/methodology.html
```

Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add cmd/static/dashboard.html cmd/static/methodology.html
git commit -m "dashboard: fix J/token chart data source, drop utilization judgment

Switches from instantaneous tokens_per_sec>5 (almost never true for
bursty personal-use traffic) to the same 24h-avg fields the CO2/token
chart already uses successfully. Recolors on a neutral single-hue
magnitude scale -- J/token reflects batch size and workload, not GPU
health, and a personal single-user box legitimately runs higher
J/token than a heavily-batched datacenter server."
git push
```

---

### Task 3: Switch the emissions chart from rate to cumulative total

**Files:**
- Modify: `cmd/static/dashboard.html`
- Modify: `cmd/static/methodology.html`

**Interfaces:**
- Consumes: `/api/v1/carbon/timeseries`'s existing `points[].co2_grams_per_hour` and top-level `step` (Go-duration string, e.g. `"5m0s"`) fields — both already returned by the current API, no backend changes.
- Produces: a module-level `lastEmissionsTotalGrams` (number, grams) updated every time `renderTimeSeries()` runs. Task 4 reads this to render its reference-frame tiles.

- [ ] **Step 1: Add the `lastEmissionsTotalGrams` module variable**

Find:

```js
let barChart  = null;
let lineChart = null;
let currentRange = '7d';
let barMode = 'co2';
let lastData = null;
```

Replace with:

```js
let barChart  = null;
let lineChart = null;
let currentRange = '7d';
let barMode = 'co2';
let lastData = null;
let lastEmissionsTotalGrams = 0;
```

- [ ] **Step 2: Add a step-duration parser**

Add this function directly above `renderTimeSeries` (i.e., right after the closing `}` of `loadTimeSeries`, before the `// CO₂/token over time` comment):

```js
// The backend always emits step as one of "5m0s", "1h0m0s", "6h0m0s"
// (Go's time.Duration.String()) -- parse the h/m/s components present.
function parseStepSeconds(stepStr) {
  const h = /(\d+)h/.exec(stepStr);
  const m = /(\d+)m/.exec(stepStr);
  const s = /(\d+)s/.exec(stepStr);
  return (h?+h[1]*3600:0) + (m?+m[1]*60:0) + (s?+s[1]:0);
}
```

- [ ] **Step 3: Replace `renderTimeSeries()`**

Find the whole function, from its leading comment through its closing `}`:

```js
// CO₂/token over time — only points where tokens were being generated.
function renderTimeSeries(data) {
  const pts = data.points || [];
  if (!pts.length) return;

  const rangeLabel = {'24h':'past 24 hours','7d':'past 7 days','30d':'past 30 days'}[currentRange]||currentRange;

  const mkLabel = p => {
    const d = new Date(p.t * 1000);
    if (currentRange === '24h') return d.toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'});
    if (currentRange === '7d')  return d.toLocaleDateString([],{month:'short',day:'numeric'}) + ' ' + d.toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'});
    return d.toLocaleDateString([],{month:'short',day:'numeric'});
  };

  // Use CO₂/token where available; show null (gap) during idle periods
  const tokenPts  = pts.filter(p => p.co2_mg_per_token > 0);
  const hasTokens = tokenPts.length > 0;

  const datasets = hasTokens ? [
    {
      label: 'Cluster avg CO₂/token (mg)',
      data: pts.map(p => p.co2_mg_per_token > 0 ? +p.co2_mg_per_token.toFixed(3) : null),
      borderColor: '#22c55e',
      backgroundColor: 'rgba(34,197,94,0.08)',
      fill: true, tension: 0.3, pointRadius: 0, spanGaps: false,
    },
  ] : [
    // Fallback: show CO₂/hr when no token data in range
    {
      label: 'CO₂ rate (kg/hr)',
      data: pts.map(p => +(p.co2_grams_per_hour/1000).toFixed(4)),
      borderColor: '#6366f1', backgroundColor: 'rgba(99,102,241,0.1)',
      fill: true, tension: 0.3, pointRadius: 0,
    }
  ];

  const avgToken = hasTokens
    ? (tokenPts.reduce((s,p)=>s+p.co2_mg_per_token,0)/tokenPts.length).toFixed(2) + ' mg CO₂/token avg'
    : 'no active generation in range — showing CO₂/hr';

  const yTitle = hasTokens ? 'mg CO₂ / token' : 'kg CO₂ / hr';
  const tooltipFmt = hasTokens
    ? c => ` ${c.dataset.label}: ${c.parsed.y != null ? c.parsed.y.toFixed(3) : '—'} mg/token`
    : c => ` ${c.parsed.y.toFixed(3)} kg CO₂/hr`;

  const chartData = { labels: pts.map(mkLabel), datasets };
  const lc = chartColors();
  const options = {
    responsive: true,
    interaction: { mode:'index', intersect:false },
    plugins: {
      legend: { labels:{ color:lc.legend, font:{size:10}, boxWidth:12 } },
      title: { display:true, text:`${rangeLabel} — ${avgToken}`,
               color:lc.tick, font:{size:11, weight:'normal'}, padding:{bottom:8} },
      tooltip: { callbacks: { label: tooltipFmt } }
    },
    scales: {
      x: { ticks:{color:lc.tick,font:{size:10},maxTicksLimit:10}, grid:{color:lc.grid} },
      y: { ticks:{color:lc.tick,font:{size:10}}, grid:{color:lc.grid},
           title:{display:true, text:yTitle, color:lc.tick, font:{size:10}},
           beginAtZero:true }
    }
  };

  if (lineChart) {
    lineChart.data = chartData;
    lineChart.options.plugins.title.text = `${rangeLabel} — ${avgToken}`;
    lineChart.update('none');
  } else {
    lineChart = new Chart(document.getElementById('lineChart'), { type:'line', data:chartData, options });
  }
}
```

Replace with:

```js
// Cumulative CO₂ emissions over the selected window — integrates the
// co2_grams_per_hour rate series using the rectangle rule (rate × step
// duration), running-summed. Always populated as long as the range has any
// points, even during idle periods (the total simply grows slowly).
function renderTimeSeries(data) {
  const pts = data.points || [];
  if (!pts.length) return;

  const rangeLabel = {'24h':'past 24 hours','7d':'past 7 days','30d':'past 30 days'}[currentRange]||currentRange;

  const mkLabel = p => {
    const d = new Date(p.t * 1000);
    if (currentRange === '24h') return d.toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'});
    if (currentRange === '7d')  return d.toLocaleDateString([],{month:'short',day:'numeric'}) + ' ' + d.toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'});
    return d.toLocaleDateString([],{month:'short',day:'numeric'});
  };

  const stepHours = parseStepSeconds(data.step) / 3600;
  let running = 0;
  const cumulativeGrams = pts.map(p => {
    running += p.co2_grams_per_hour * stepHours;
    return +running.toFixed(2);
  });
  lastEmissionsTotalGrams = running;

  const totalLabel = running >= 1000
    ? (running/1000).toFixed(2) + ' kg CO₂e'
    : running.toFixed(1) + ' g CO₂e';

  const chartData = {
    labels: pts.map(mkLabel),
    datasets: [{
      label: 'Cumulative CO₂ (g)',
      data: cumulativeGrams,
      borderColor: '#6366f1',
      backgroundColor: 'rgba(99,102,241,0.1)',
      fill: true, tension: 0.3, pointRadius: 0,
    }],
  };
  const lc = chartColors();
  const options = {
    responsive: true,
    interaction: { mode:'index', intersect:false },
    plugins: {
      legend: { labels:{ color:lc.legend, font:{size:10}, boxWidth:12 } },
      title: { display:true, text:`${rangeLabel} — Total: ${totalLabel}`,
               color:lc.tick, font:{size:11, weight:'normal'}, padding:{bottom:8} },
      tooltip: { callbacks: { label: c => ` ${c.parsed.y.toFixed(2)} g CO₂ cumulative` } }
    },
    scales: {
      x: { ticks:{color:lc.tick,font:{size:10},maxTicksLimit:10}, grid:{color:lc.grid} },
      y: { ticks:{color:lc.tick,font:{size:10}}, grid:{color:lc.grid},
           title:{display:true, text:'g CO₂ (cumulative)', color:lc.tick, font:{size:10}},
           beginAtZero:true }
    }
  };

  if (lineChart) {
    lineChart.data = chartData;
    lineChart.options.plugins.title.text = `${rangeLabel} — Total: ${totalLabel}`;
    lineChart.update('none');
  } else {
    lineChart = new Chart(document.getElementById('lineChart'), { type:'line', data:chartData, options });
  }
}
```

- [ ] **Step 4: Rewrite methodology.html's cumulative-emissions section**

Find:

```html
<h3>Cumulative CO₂ and cluster CO₂/token (time-series view)</h3>
<p>
  For the time-series charts, we query the Prometheus range API at adaptive resolution
  (5-minute steps for 24 h, hourly for 7 d, 6-hourly for 30 d),
  apply the same per-node intensity to each sample, and integrate:
</p>
<pre><code>CO₂_kg_cumulative = Σ (P_watts_i × intensity_i × Δt_hrs / 1000)

-- Total token rate used as denominator for cluster-wide CO₂/token:
sum by (namespace, container) (
  rate(vllm:generation_tokens_total[5m]) + rate(vllm:prompt_tokens_total[5m])
)</code></pre>
```

Replace with:

```html
<h3>Cumulative CO₂ emissions (time-series view)</h3>
<p>
  The backend's timeseries endpoint queries the Prometheus range API at
  adaptive resolution (5-minute steps for 24 h, hourly for 7 d, 6-hourly for
  30 d) and returns <code>co2_grams_per_hour</code> at each step (already
  computed server-side as <code>power_watts × 0.198</code>, nimbus's fixed
  grid intensity). The dashboard then integrates that rate client-side using
  the rectangle rule, running-summed over the selected window:
</p>
<pre><code>total_g = Σ (co2_grams_per_hour_i × step_hours)</code></pre>
<p>
  This total only grows — it never resets within a window — so the chart
  climbs steadily even while nimbus is mostly idle, rather than looking flat
  or empty the way a tiny, nearly-constant rate does.
</p>
```

- [ ] **Step 5: Manually verify locally**

```bash
cd /home/cboettig/Documents/github/boettiger-lab/nimbus-carbon-api
go run ./cmd &
APP_PID=$!
sleep 2
curl -s 'http://localhost:8080/api/v1/carbon/timeseries?range=24h' | python3 -c "
import json,sys
d=json.load(sys.stdin)
print('step:', d.get('step'), 'points:', len(d.get('points') or []))
"
kill $APP_PID
```

Expected: `step` prints a Go-duration string like `5m0s`, `points` is a positive number — this confirms the endpoint still returns exactly the shape `parseStepSeconds`/`renderTimeSeries` expect. (This starts the app pointed at the default `PROMETHEUS_URL`, which will fail to reach it from your local machine — that's fine, the point of this check is just the JSON shape via the app's own code path, not live data. If you want to see it against real data, port-forward Prometheus first as in earlier tasks.)

- [ ] **Step 6: Commit**

```bash
git add cmd/static/dashboard.html cmd/static/methodology.html
git commit -m "dashboard: switch emissions chart from instantaneous rate to cumulative total

The chart was titled \"emissions over time\" but plotted an instantaneous
rate (kg CO2/hr) -- tiny and nearly flat for nimbus, so with
beginAtZero it visually read as empty. Now integrates the rate
client-side (rectangle rule) into a running total that climbs steadily
even while idle."
git push
```

---

### Task 4: Add relatable everyday reference frames

**Files:**
- Modify: `cmd/static/dashboard.html`
- Modify: `cmd/static/methodology.html`

**Interfaces:**
- Consumes: `lastEmissionsTotalGrams` (Task 3) and `lastData` (already existing module variable, used to read the live grid intensity for the fridge conversion).
- Produces: nothing later tasks depend on.

- [ ] **Step 1: Add the reference-frame conversion constants**

Find the `FRONTIER_IDLE_W` line:

```js
const FRONTIER_IDLE_W = FRONTIER.numGPUs * FRONTIER.idleWattsPerGPU; // 5400 W
```

Add directly below it:

```js

// Everyday reference frames for CO2 totals, in the style of CodeCarbon.
// Sources cited in /methodology.
const REF_CAR_G_PER_MILE = 404;       // EPA: average gasoline passenger vehicle, g CO2/mile
const REF_PHONE_CHARGE_G = 8.22;      // g CO2 per full smartphone charge (commonly cited estimate)
const REF_FRIDGE_WATTS = 150;         // average household refrigerator draw, W
const REF_TREE_KG_PER_MONTH = 1.75;   // kg CO2 absorbed per mature tree per month (rough estimate)
```

- [ ] **Step 2: Add the `renderReferenceFrames()` function**

Add directly above the `document.querySelectorAll('#time-range-tabs ...` event-listener block (i.e., right after the closing `}` of `renderTimeSeries`):

```js
// Converts a cumulative CO2 total (grams) into a few widely-cited everyday
// equivalents. The fridge comparison intentionally uses nimbus's own grid
// intensity (from the current model data) rather than a separate reference
// intensity, so it's an apples-to-apples "same grid, different appliance"
// comparison.
function renderReferenceFrames(totalGrams) {
  const el = document.getElementById('ref-frames');
  if (!el) return;

  const gridIntensity = (lastData && lastData.models && lastData.models[0])
    ? lastData.models[0].carbon_intensity_kg_per_kwh
    : 0.198;

  const miles = totalGrams / REF_CAR_G_PER_MILE;
  const charges = totalGrams / REF_PHONE_CHARGE_G;
  const fridgeGPerHour = (REF_FRIDGE_WATTS / 1000) * gridIntensity * 1000; // kW × kg/kWh × 1000 g/kg
  const fridgeMinutes = fridgeGPerHour > 0 ? (totalGrams / fridgeGPerHour) * 60 : 0;
  const treeMonths = totalGrams / (REF_TREE_KG_PER_MONTH * 1000);

  const tiles = [
    { label: 'Miles driven', value: fmt(miles, miles < 1 ? 3 : 1), unit: 'gas car (EPA avg)' },
    { label: 'Phone charges', value: fmt(charges, charges < 1 ? 2 : 0), unit: 'full charges' },
    { label: 'Fridge runtime', value: fmt(fridgeMinutes, fridgeMinutes < 1 ? 2 : 0), unit: 'minutes' },
    { label: 'Tree absorption', value: fmt(treeMonths, 3), unit: 'tree-months' },
  ];
  el.innerHTML = tiles.map(t => `
    <div class="stat">
      <div class="stat-label">${t.label}</div>
      <div class="stat-value">${t.value}</div>
      <div class="stat-unit">${t.unit}</div>
    </div>`).join('');
}
```

- [ ] **Step 3: Call it from `renderTimeSeries()`**

Find (this is the line Task 3 added):

```js
  lastEmissionsTotalGrams = running;
```

Replace with:

```js
  lastEmissionsTotalGrams = running;
  renderReferenceFrames(running);
```

- [ ] **Step 4: Add the `<div id="ref-frames">` container to the HTML**

Find:

```html
  <div class="chart-wrap">
    <div class="section-head">
      <h2>Cluster CO₂ emissions over time</h2>
      <div class="range-tabs" id="time-range-tabs">
        <button class="range-tab" data-range="24h">24 h</button>
        <button class="range-tab active" data-range="7d">7 d</button>
        <button class="range-tab" data-range="30d">30 d</button>
      </div>
    </div>
    <canvas id="lineChart"></canvas>
  </div>

  <div class="chart-wrap">
    <div class="section-head">
      <h2 id="bar-title">CO₂ per token by model</h2>
```

Replace with:

```html
  <div class="chart-wrap">
    <div class="section-head">
      <h2>Cluster CO₂ emissions over time</h2>
      <div class="range-tabs" id="time-range-tabs">
        <button class="range-tab" data-range="24h">24 h</button>
        <button class="range-tab active" data-range="7d">7 d</button>
        <button class="range-tab" data-range="30d">30 d</button>
      </div>
    </div>
    <canvas id="lineChart"></canvas>
    <div class="ref-frames" id="ref-frames"></div>
  </div>

  <div class="chart-wrap">
    <div class="section-head">
      <h2 id="bar-title">CO₂ per token by model</h2>
```

- [ ] **Step 5: Add CSS for `.ref-frames`**

Find:

```css
  .chart-wrap{background:var(--surface);border:1px solid var(--border);border-radius:.75rem;padding:1.2rem;margin-bottom:1.75rem;}
  .chart-wrap canvas{max-height:240px;}
```

Replace with:

```css
  .chart-wrap{background:var(--surface);border:1px solid var(--border);border-radius:.75rem;padding:1.2rem;margin-bottom:1.75rem;}
  .chart-wrap canvas{max-height:240px;}
  .ref-frames{display:grid;grid-template-columns:repeat(auto-fit,minmax(130px,1fr));gap:.65rem;margin-top:1rem;}
```

- [ ] **Step 6: Add the reference-frames documentation to methodology.html**

Find:

```html
<h2>Source Code</h2>
```

Replace with (this adds a new section directly before it; the existing "Source Code" heading and everything after it is untouched):

```html
<h2>Everyday Reference Frames</h2>
<p>
  Absolute CO₂ figures for a single, efficient GB10 node are small (grams,
  not kilograms) and don't mean much in isolation. The dashboard converts
  the cumulative total for the selected window (see "Cumulative CO₂
  emissions" above) into a few widely-cited everyday equivalents, in the
  style of <a href="https://github.com/mlco2/codecarbon" target="_blank">CodeCarbon</a>:
</p>
<table>
  <tr><th>Reference</th><th>Conversion factor</th><th>Source</th></tr>
  <tr><td>Miles driven (gas car)</td><td>404 g CO₂/mile</td><td><a href="https://www.epa.gov/greenvehicles/greenhouse-gas-emissions-typical-passenger-vehicle" target="_blank">EPA, average passenger vehicle</a></td></tr>
  <tr><td>Smartphone charges</td><td>8.22 g CO₂/charge</td><td>Commonly cited estimate (a full charge is roughly 0.012 kWh)</td></tr>
  <tr><td>Refrigerator runtime</td><td>~150 W average draw</td><td>Typical household refrigerator; converted to grams using nimbus's own 0.198 kg CO₂/kWh grid intensity, for a like-for-like "same grid, different appliance" comparison</td></tr>
  <tr><td>Tree-months of absorption</td><td>~1.75 kg CO₂/month/tree</td><td>Common carbon-calculator estimate — the roughest of the four; real absorption varies significantly by species, age, and region</td></tr>
</table>
<div class="callout">
  <p><strong>Note:</strong> these are meant to build intuition for very small
  numbers, not as precise offsets.</p>
</div>

<h2>Source Code</h2>
```

- [ ] **Step 7: Manually verify locally**

```bash
cd /home/cboettig/Documents/github/boettiger-lab/nimbus-carbon-api
grep -n "ref-frames\|renderReferenceFrames\|REF_CAR_G_PER_MILE" cmd/static/dashboard.html
```

Expected: matches for the CSS rule, the HTML container, the constants, the function definition, and the call site — five or more matching lines total, none missing.

- [ ] **Step 8: Commit**

```bash
git add cmd/static/dashboard.html cmd/static/methodology.html
git commit -m "dashboard: add everyday CO2 reference frames (car miles, phone charges, fridge, trees)

CodeCarbon-style equivalents computed from the cumulative emissions
total for whichever window (24h/7d/30d) is selected, so small absolute
numbers like grams of CO2 build some intuition."
git push
```

---

### Task 5: Narrow the live token-rate window

**Files:**
- Modify: `internal/scraper/scraper.go`

**Interfaces:**
- Consumes: nothing from Tasks 1-4 (independent file/concern).
- Produces: nothing later tasks depend on.

- [ ] **Step 1: Narrow `queryTokens()`'s rate window**

Find:

```go
func (s *Scraper) queryTokens() (genTokens, promptTokens map[string]float64, names map[string]string, err error) {
	genResults, err := s.client.Query(
		`sum by (namespace, model_name) (rate(vllm:generation_tokens_total{namespace="default"}[5m]))`,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	promptResults, err := s.client.Query(
		`sum by (namespace, model_name) (rate(vllm:prompt_tokens_total{namespace="default"}[5m]))`,
	)
```

Replace with:

```go
func (s *Scraper) queryTokens() (genTokens, promptTokens map[string]float64, names map[string]string, err error) {
	// [2m], not [5m]: nimbus's traffic is bursty single-user generation, not
	// steady multi-tenant load. A 5-minute window dilutes a 20-second burst
	// across 280 seconds of surrounding idle time and goes fully stale within
	// minutes of the burst ending. [2m] is still >=4x the 30s scrape interval
	// (Prometheus's own recommended minimum for a stable rate() over a
	// counter) while tracking real bursts far more closely. Confirmed live:
	// a real burst measured via raw counter deltas ran 30-133 tok/s, while
	// rate(...[5m]) read 0-5 tok/s during and shortly after it.
	genResults, err := s.client.Query(
		`sum by (namespace, model_name) (rate(vllm:generation_tokens_total{namespace="default"}[2m]))`,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	promptResults, err := s.client.Query(
		`sum by (namespace, model_name) (rate(vllm:prompt_tokens_total{namespace="default"}[2m]))`,
	)
```

- [ ] **Step 2: Verify the build and existing tests still pass**

```bash
cd /home/cboettig/Documents/github/boettiger-lab/nimbus-carbon-api
go build ./...
go vet ./...
go test ./... -v
```

Expected: all exit 0; the existing `internal/carbon` and `internal/scraper` tests (from prior work) are unaffected by this change — none of them exercise `queryTokens()`'s PromQL string directly.

- [ ] **Step 3: Verify against the live Prometheus**

```bash
kubectl -n monitoring port-forward svc/prometheus-server 9090:80 &
sleep 3
echo "=== old [5m] window ==="
curl -s 'http://localhost:9090/api/v1/query?query=sum+by+(namespace)+(rate(vllm:generation_tokens_total%7Bnamespace%3D%22default%22%7D%5B5m%5D))' | python3 -m json.tool
echo "=== new [2m] window ==="
curl -s 'http://localhost:9090/api/v1/query?query=sum+by+(namespace)+(rate(vllm:generation_tokens_total%7Bnamespace%3D%22default%22%7D%5B2m%5D))' | python3 -m json.tool
kill %1
```

Expected: both return a result (possibly both 0 if nimbus happens to be fully idle right now — that's fine and correct). This step is about confirming the query is syntactically valid and returns a metric with the same label shape (`namespace="default"`) as before, not about forcing a specific nonzero value — if you want to see the `[2m]` window actually diverge from `[5m]`, you'd need to run this again while a real generation request is in flight (not required for this task; Task 6's end-to-end deploy verification is the better place to catch a live burst if one happens to occur).

- [ ] **Step 4: Update methodology.html's Step 2 PromQL snippet**

This snippet has two pre-existing staleness issues unrelated to this specific window change (a stale `container` in the group-by that no longer matches the actual query, inherited from before nimbus-carbon-api's namespace-only rewrite) — fix both while updating the window, since this step is already touching this exact snippet.

Find:

```html
<pre><code>-- Output (decode) tokens generated per second:
sum by (namespace, container, model_name) (
  rate(vllm:generation_tokens_total[5m])
)

-- Input (prefill) tokens processed per second:
sum by (namespace, container, model_name) (
  rate(vllm:prompt_tokens_total[5m])
)</code></pre>
```

Replace with:

```html
<pre><code>-- Output (decode) tokens generated per second (2-minute window --
-- short enough to track nimbus's bursty single-user traffic without
-- diluting a burst across mostly-idle time on either side of it):
sum by (namespace, model_name) (
  rate(vllm:generation_tokens_total{namespace="default"}[2m])
)

-- Input (prefill) tokens processed per second:
sum by (namespace, model_name) (
  rate(vllm:prompt_tokens_total{namespace="default"}[2m])
)</code></pre>
```

- [ ] **Step 5: Commit**

```bash
git add internal/scraper/scraper.go cmd/static/methodology.html
git commit -m "scraper: narrow live token-rate window from [5m] to [2m]

nimbus's traffic is bursty single-user generation, not steady
multi-tenant load. [5m] dilutes a real burst (confirmed 30-133 tok/s
via raw counter deltas) down to 0-5 tok/s and goes stale within
minutes of the burst ending. [2m] stays >=4x the 30s scrape interval
while tracking bursts far more closely. Also fixes methodology.html's
Step 2 snippet, which still showed the pre-namespace-only-rewrite
container group-by."
git push
```

---

### Task 6: Deploy and verify all five changes end-to-end

**Files:**
- None (deploys already-committed work)

**Interfaces:**
- Consumes: all of Tasks 1-5.

- [ ] **Step 1: Wait for CI**

```bash
cd /home/cboettig/Documents/github/boettiger-lab/nimbus-carbon-api
gh run list --limit 3
gh run watch $(gh run list --limit 1 --json databaseId --jq '.[0].databaseId')
```

Expected: the run for the last commit from Task 5 completes with all jobs green.

- [ ] **Step 2: Restart the deployment to pick up the new image**

```bash
kubectl -n default rollout restart deployment/nimbus-carbon-api
kubectl -n default rollout status deployment/nimbus-carbon-api --timeout=120s
kubectl -n default get pods -l app=nimbus-carbon-api
kubectl -n default logs -l app=nimbus-carbon-api --tail=20
```

Expected: rollout completes, pod `1/1 Running`, no error logs.

- [ ] **Step 3: Verify Task 1 (frontier comparison demoted)**

```bash
curl -s https://carbon-nimbus.carlboettiger.info/api/v1/carbon | python3 -m json.tool
```

Confirm the response is unchanged in shape (this task is presentation-only — the API contract doesn't change). Then open `https://carbon-nimbus.carlboettiger.info` in a browser: confirm no model card shows a colored left border, and the "vs. Commercial frontier" line only appears on cards where `prompt_tokens_per_sec_avg_24h + generation_tokens_per_sec_avg_24h > 1` (check the current API response's values against this threshold to know what to expect).

- [ ] **Step 4: Verify Task 2 (J/token chart populated)**

In the browser, click the "J / token" tab on the second chart. Expected: if any model has `prompt_tokens_per_sec_avg_24h + generation_tokens_per_sec_avg_24h > 0.1`, a bar renders in a single indigo hue (not green/red) — check this against the live `/api/v1/carbon` response's 24h-avg fields to confirm the chart matches.

- [ ] **Step 5: Verify Task 3 (cumulative emissions chart)**

In the browser, look at the top "Cluster CO₂ emissions over time" chart. Expected: a monotonically non-decreasing line, and the chart's title area reads "Total: `N` g CO₂e" (or kg for larger totals) rather than "no active generation in range — showing CO₂/hr". Cross-check the displayed total against the API:

```bash
curl -s 'https://carbon-nimbus.carlboettiger.info/api/v1/carbon/timeseries?range=7d' | python3 -c "
import json,sys
d=json.load(sys.stdin)
pts=d['points']
step_s = 300 if 'm' in d['step'] and d['step'].startswith('5') else None
# quick sanity sum assuming 5m steps for a 7d-window response (adjust if step differs)
print('step:', d['step'], 'num points:', len(pts))
"
```

Expected: the browser-displayed total is in the same ballpark as manually summing `co2_grams_per_hour × step_hours` across the returned points (a rough sanity check, not an exact match requirement, since the browser's total reflects the live 7d window at the moment you load the page).

- [ ] **Step 6: Verify Task 4 (reference frames)**

In the browser, confirm four small tiles ("Miles driven", "Phone charges", "Fridge runtime", "Tree absorption") appear directly below the emissions chart, with plausible small values (given nimbus's total CO₂ for any window is in the tens of grams, expect very small fractional numbers, e.g. well under 1 mile, a few phone charges, tens of minutes of fridge runtime).

- [ ] **Step 7: Verify Task 5 (live tok/s window)**

If possible, send a real chat request to the deployed model (e.g. via the vLLM endpoint directly) and, while it's generating, compare the dashboard's "Output tok/s" against what you'd expect from watching the response stream. If no live traffic is available at verification time, confirm instead that the API still returns valid data with the new window:

```bash
curl -s https://carbon-nimbus.carlboettiger.info/api/v1/carbon | python3 -m json.tool
```

Expected: valid JSON, same shape as before, `generation_tokens_per_sec`/`prompt_tokens_per_sec` present (0 if idle, which is correct).

## Self-Review Notes

- **Spec coverage**: all five spec changes (1-5) have a corresponding task; Task 6 (deploy+verify) isn't a spec item but follows the established pattern from the prior plan of not leaving fixes undeployed.
- **Placeholder scan**: no TBDs; every step has literal file content, exact commands, and expected output.
- **Type/interface consistency**: `renderTimeSeries()`'s `lastEmissionsTotalGrams` (Task 3) and the `renderReferenceFrames(totalGrams)` call site (Task 4) use matching names; `frontierMetric`/`wattsCmp` (Task 1) are spliced into the exact template literals shown, verified against the file's actual current content before writing this plan.
