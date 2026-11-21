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
//
// A model with a power_hosts override (models.json, "model@node") also gets
// its extra hosts' power, joined on (node, model_name) so it lands on that
// model alone and only while it is marked. The terms are tagged with a `part`
// label so `or` keeps all of them, then summed per model.
//
// An extra host that is another node's own host (nimbus3, the worker of a
// pair headed on nimbus, but also a node serving its own models) gets a term
// of its own, gated with `unless on ()` on ANY model serving on that node:
// at a minute when its node serves something, the node term already gives
// the host's watts to that model, so the pair's term must be empty. The gate
// uses serving (presence), not the indicator, so active energy obeys the
// same split. Extra hosts no node owns (nimbus4) keep the ungated term,
// byte-identical to before this gate existed.
func (c Config) attributedPower(rangeSel, indicator string) string {
	const metric = "DCGM_FI_DEV_POWER_USAGE"
	base := fmt.Sprintf(`(%s * on (node) group_right () %s)`, c.dcgmByNode(metric, "sum", rangeSel), indicator)
	overrides := c.modelOverrides()
	if len(overrides) == 0 {
		return base
	}
	terms := []string{fmt.Sprintf(`label_replace(%s, "part", "node", "", "")`, base)}
	// pinned is the hosts' summed power joined onto one model on one node.
	pinned := func(hosts []string, model, node string) string {
		sel := fmt.Sprintf(`%s{Hostname=~%q}`, metric, alternation(hosts))
		if rangeSel != "" {
			sel = fmt.Sprintf(`avg_over_time(%s[%s])`, sel, rangeSel)
		}
		power := fmt.Sprintf(`label_replace(label_replace(sum(%s), "node", %q, "", ""), "model_name", %q, "", "")`,
			sel, node, model)
		return fmt.Sprintf(`(%s * on (node, model_name) group_right () %s)`, power, indicator)
	}
	for i, o := range overrides {
		model, node := o[0], o[1]
		free, gated := c.splitExtras(model, node)
		if len(free) > 0 {
			terms = append(terms, fmt.Sprintf(`label_replace(%s, "part", "x%d", "", "")`, pinned(free, model, node), i))
		}
		for _, g := range gated {
			gate := fmt.Sprintf(`vllm:num_requests_running{namespace=%q,node=%q}`, g.owner.Namespace, g.owner.Name)
			terms = append(terms, fmt.Sprintf(`label_replace((%s unless on () %s), "part", "x%d-%s", "", "")`,
				pinned(g.hosts, model, node), gate, i, g.owner.Name))
		}
	}
	return fmt.Sprintf(`(sum by (%s) (%s))`, modelLabels, strings.Join(terms, " or "))
}

// attributedUtil is node GPU utilisation (%) joined onto whichever model the
// indicator marks on that node, like attributedPower. A model with a
// power_hosts override is instead averaged over ALL its hosts (its node's own
// plus the extra ones): utilisation averages rather than adds, so the extra
// hosts cannot simply be summed on. The override term comes first in the `or`
// so it replaces the node term for that model, label set for label set.
func (c Config) attributedUtil(rangeSel, indicator string) string {
	const metric = "DCGM_FI_DEV_GPU_UTIL"
	base := fmt.Sprintf(`(%s * on (node) group_right () %s)`, c.dcgmByNode(metric, "avg", rangeSel), indicator)
	overrides := c.modelOverrides()
	if len(overrides) == 0 {
		return base
	}
	var terms []string
	for _, o := range overrides {
		model, node := o[0], o[1]
		sel := fmt.Sprintf(`%s{Hostname=~%q}`, metric, alternation(c.powerHostsFor(model, node)))
		if rangeSel != "" {
			sel = fmt.Sprintf(`avg_over_time(%s[%s])`, sel, rangeSel)
		}
		util := fmt.Sprintf(`label_replace(label_replace(avg(%s), "node", %q, "", ""), "model_name", %q, "", "")`,
			sel, node, model)
		terms = append(terms, fmt.Sprintf(`(%s * on (node, model_name) group_right () %s)`, util, indicator))
	}
	return fmt.Sprintf(`(%s or %s)`, strings.Join(terms, " or "), base)
}

// buckets is a per-model histogram's bucket rates over window w, one series
// per `le`. The quantile is NOT taken in PromQL: a card can span several
// "model@node" series (see cardKey), and which ones is decided by models.json,
// so the scraper sums the buckets per card and interpolates them itself with
// Prometheus's own algorithm (histQuantile). For a one-node card the result
// is what histogram_quantile would return. Rates rather than increases: the
// two differ by a constant that cancels.
func (c Config) buckets(histogram, w string) string {
	return fmt.Sprintf(`sum by (le, %s) (rate(%s_bucket{%s}[%s]))`,
		modelLabels, histogram, c.vllmSelector(), w)
}

// rate is a per-model counter rate over w.
func (c Config) rate(counter, w string) string {
	return c.vllm(fmt.Sprintf(`rate(%s{%%s}[%s])`, counter, w))
}

// increase is a per-model counter increase over w.
func (c Config) increase(counter, w string) string {
	return c.vllm(fmt.Sprintf(`increase(%s{%%s}[%s])`, counter, w))
}

// overTime evaluates fn(expr) over window w at a 1-minute resolution.
func overTime(fn, expr, w string) string {
	return fmt.Sprintf(`%s((%s)[%s:1m])`, fn, expr, w)
}
