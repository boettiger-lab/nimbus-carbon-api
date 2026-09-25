package scraper

import (
	"fmt"
	"regexp"
	"strings"
)

// PromQL builders. Every vLLM expression is filtered by BOTH namespace and
// node and grouped by (node, namespace, model_name), so each result belongs to
// exactly one model on one configured node. GPU power and utilisation come
// from DCGM, which labels series with `Hostname`; dcgmByNode relabels them to
// the node whose model they are attributed to.

// modelLabels is the grouping every per-model expression uses.
const modelLabels = "node, namespace, model_name"

// vllm substitutes the shared selector into expr (which must contain one %s
// per selector use, or %[1]s) and groups the result per model.
func (c Config) vllm(expr string) string {
	return fmt.Sprintf(`sum by (%s) (%s)`, modelLabels, strings.ReplaceAll(expr, "%s", c.vllmSelector()))
}

// dcgmByNode aggregates a DCGM metric across each node's PowerHosts and labels
// the result with `node`. rangeSel is "" for a raw read or e.g. "5m" for an
// avg_over_time-smoothed one. agg is "sum" for power, "avg" for utilisation.
//
// A tensor-parallel model's metrics carry only its head's node, but it draws
// power on every rank: listing the worker in the head's PowerHosts relabels
// the worker's series to the head's node so the two are summed.
func (c Config) dcgmByNode(metric, agg, rangeSel string) string {
	sel := fmt.Sprintf(`%s{Hostname=~%q}`, metric, alternation(c.powerHosts()))
	if rangeSel != "" {
		sel = fmt.Sprintf(`avg_over_time(%s[%s])`, sel, rangeSel)
	}
	expr := fmt.Sprintf(`label_replace(%s, "node", "$1", "Hostname", "(.*)")`, sel)
	for _, n := range c.Nodes {
		for _, h := range n.PowerHosts {
			if h == n.Name {
				continue
			}
			expr = fmt.Sprintf(`label_replace(%s, "node", %q, "Hostname", %q)`, expr, n.Name, regexp.QuoteMeta(h))
		}
	}
	return fmt.Sprintf(`%s by (node) (%s)`, agg, expr)
}

// presence is 1 for every model whose vLLM server is currently exporting
// metrics, labelled per model. It is the attribution key: a node's watts
// belong to a model only while that model is serving there.
func (c Config) presence() string {
	return fmt.Sprintf(`(max by (%s) (vllm:num_requests_running{%s}) * 0 + 1)`, modelLabels, c.vllmSelector())
}

// active is 1 for every model processing more than 5 tok/s (prompt +
// generation) over the last 5 minutes — the same "working, not idling"
// threshold the CO₂/token figures have always used.
func (c Config) active() string {
	// Parenthesised as a whole: it is spliced in as a join operand, where a
	// bare "x * 0 + 1" would swallow the product and evaluate to 1.
	return fmt.Sprintf(`(((%s + %s) > 5) * 0 + 1)`,
		c.vllm(`rate(vllm:generation_tokens_total{%s}[5m])`),
		c.vllm(`rate(vllm:prompt_tokens_total{%s}[5m])`))
}

// attributedPower is node GPU power (W) joined onto whichever model the
// indicator marks on that node; indicator is presence() or active().
func (c Config) attributedPower(rangeSel, indicator string) string {
	return fmt.Sprintf(`(%s * on (node) group_right () %s)`, c.dcgmByNode("DCGM_FI_DEV_POWER_USAGE", "sum", rangeSel), indicator)
}

// quantile builds a per-model histogram quantile over window w, from rates
// (live) or increases (aggregate) — identical up to a constant that cancels.
func (c Config) quantile(q float64, histogram, w string) string {
	return fmt.Sprintf(`histogram_quantile(%g, sum by (le, %s) (rate(%s_bucket{%s}[%s])))`,
		q, modelLabels, histogram, c.vllmSelector(), w)
}

// ratio divides two per-model counters' increases over w, scaled.
func (c Config) ratio(num, den, w string, scale float64) string {
	return fmt.Sprintf(`%g * %s / %s`, scale,
		c.vllm(fmt.Sprintf(`increase(%s{%%s}[%s])`, num, w)),
		c.vllm(fmt.Sprintf(`increase(%s{%%s}[%s])`, den, w)))
}

// increase is a per-model counter increase over w.
func (c Config) increase(counter, w string) string {
	return c.vllm(fmt.Sprintf(`increase(%s{%%s}[%s])`, counter, w))
}

// overTime evaluates fn(expr) over window w at a 1-minute resolution.
func overTime(fn, expr, w string) string {
	return fmt.Sprintf(`%s((%s)[%s:1m])`, fn, expr, w)
}
