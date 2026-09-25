package scraper

import (
	"strings"
	"testing"
)

// clusterConfig mirrors the live cluster: a TP2 pair whose worker (nimbus4)
// exports no vLLM metrics of its own but draws power.
func clusterConfig() Config {
	nodes, err := ParseNodes(`[
	  {"name":"cirrus","gpu_hardware":"Quadro RTX 8000","gpu_count":2},
	  {"name":"nimbus","gpu_hardware":"NVIDIA GB10"},
	  {"name":"nimbus2","gpu_hardware":"NVIDIA GB10","gpu_count":2,"power_hosts":["nimbus2","nimbus4"]},
	  {"name":"nimbus3","gpu_hardware":"NVIDIA GB10"}
	]`)
	if err != nil {
		panic(err)
	}
	return Config{Nodes: nodes}
}

// TestKeyForRequiresBothLabels is the regression test for the misattribution
// bug: two nodes served models out of the SAME namespace, so keying state by
// namespace alone summed nimbus's tokens into cirrus's row.
func TestKeyForRequiresBothLabels(t *testing.T) {
	c := clusterConfig()

	if got := c.keyFor("cirrus", "vllm"); got != "cirrus" {
		t.Errorf("keyFor(cirrus, vllm) = %q, want cirrus", got)
	}
	if got := c.keyFor("nimbus", "vllm"); got != "nimbus" {
		t.Errorf("keyFor(nimbus, vllm) = %q, want nimbus", got)
	}
	// A node we do not report on must be dropped, not folded into another row.
	if got := c.keyFor("thelio", "vllm"); got != "" {
		t.Errorf("keyFor(thelio, vllm) = %q, want empty", got)
	}
	// Right node, wrong namespace: some other workload's metrics.
	if got := c.keyFor("cirrus", "jupyter"); got != "" {
		t.Errorf("keyFor(cirrus, jupyter) = %q, want empty", got)
	}
}

// TestVLLMSelectorFiltersNodes guards the selector every query shares.
func TestVLLMSelectorFiltersNodes(t *testing.T) {
	sel := clusterConfig().vllmSelector()
	if !strings.Contains(sel, `node=~"cirrus|nimbus|nimbus2|nimbus3"`) {
		t.Errorf("selector missing node filter: %s", sel)
	}
	if !strings.Contains(sel, `namespace=~"vllm"`) {
		t.Errorf("selector missing namespace filter: %s", sel)
	}
}

// TestVLLMQueryGroupsPerModel: without node AND model_name in the by-clause,
// two models (or two nodes) collapse into one series.
func TestVLLMQueryGroupsPerModel(t *testing.T) {
	q := clusterConfig().vllm(`rate(vllm:generation_tokens_total{%s}[2m])`)
	if !strings.HasPrefix(q, "sum by (node, namespace, model_name)") {
		t.Errorf("query does not group per model: %s", q)
	}
	if !strings.Contains(q, `node=~"cirrus|nimbus|nimbus2|nimbus3"`) {
		t.Errorf("query does not filter by node: %s", q)
	}
}

// TestPowerFoldsTPWorkerIntoHead: the TP2 worker's watts must be relabelled to
// the head's node, or DeepSeek's energy is half what it really is.
func TestPowerFoldsTPWorkerIntoHead(t *testing.T) {
	q := clusterConfig().dcgmByNode("DCGM_FI_DEV_POWER_USAGE", "sum", "5m")
	if !strings.Contains(q, `Hostname=~"cirrus|nimbus|nimbus2|nimbus4|nimbus3"`) {
		t.Errorf("power query does not read every power host: %s", q)
	}
	if !strings.Contains(q, `"node", "nimbus2", "Hostname", "nimbus4"`) {
		t.Errorf("worker power not relabelled to the head: %s", q)
	}
	if !strings.HasPrefix(q, "sum by (node)") || !strings.Contains(q, "avg_over_time") {
		t.Errorf("power query shape wrong: %s", q)
	}
	if raw := clusterConfig().dcgmByNode("DCGM_FI_DEV_POWER_USAGE", "sum", ""); strings.Contains(raw, "avg_over_time") {
		t.Errorf("unsmoothed power query should not wrap avg_over_time: %s", raw)
	}
}

// TestAttributedPowerJoinsOnNode: node power only counts toward a model while
// that model is serving, and the join must keep the model's labels.
func TestAttributedPowerJoinsOnNode(t *testing.T) {
	c := clusterConfig()
	q := c.attributedPower("", c.presence())
	if !strings.Contains(q, "* on (node) group_right ()") {
		t.Errorf("attribution join wrong: %s", q)
	}
	if !strings.Contains(q, "vllm:num_requests_running") {
		t.Errorf("attribution not keyed on the serving gauge: %s", q)
	}
}

// TestActiveIndicatorIsParenthesised: spliced into the attribution join
// unparenthesised, "x * 0 + 1" makes the whole product 1 — active energy
// then reads as 1 W forever. (Shipped once; caught against live data.)
func TestActiveIndicatorIsParenthesised(t *testing.T) {
	a := clusterConfig().active()
	if !strings.HasPrefix(a, "(") || !strings.HasSuffix(a, "* 0 + 1)") {
		t.Errorf("active() must be fully parenthesised: %s", a)
	}
	p := clusterConfig().presence()
	if !strings.HasPrefix(p, "(") || !strings.HasSuffix(p, "* 0 + 1)") {
		t.Errorf("presence() must be fully parenthesised: %s", p)
	}
}

// TestParseNodesDefaults: omitted fields get sane values, bad input is loud.
func TestParseNodesDefaults(t *testing.T) {
	nodes, err := ParseNodes(`[{"name":"cirrus","gpu_hardware":"Quadro RTX 8000","gpu_count":2,"node_power":true}]`)
	if err != nil {
		t.Fatalf("ParseNodes: %v", err)
	}
	n := nodes[0]
	if n.Namespace != "vllm" || n.Container != "vllm" {
		t.Errorf("defaults not applied: %+v", n)
	}
	if len(n.PowerHosts) != 1 || n.PowerHosts[0] != "cirrus" {
		t.Errorf("power_hosts should default to the node itself: %+v", n.PowerHosts)
	}
	if _, err := ParseNodes(`[]`); err == nil {
		t.Error("expected an error for an empty node list")
	}
	if _, err := ParseNodes(`[{"namespace":"vllm"}]`); err == nil {
		t.Error("expected an error for a node with no name")
	}
}

// TestParseNodesRejectsDoubleClaim: one host's watts counted for two nodes
// would double-count its energy.
func TestParseNodesRejectsDoubleClaim(t *testing.T) {
	_, err := ParseNodes(`[{"name":"nimbus2","power_hosts":["nimbus2","nimbus4"]},{"name":"nimbus4"}]`)
	if err == nil {
		t.Error("expected an error when two nodes claim nimbus4's power")
	}
}

// TestModelInfoPrefersNodeSpecific: "model@node" beats a bare model name.
func TestModelInfoPrefersNodeSpecific(t *testing.T) {
	c := Config{Models: map[string]ModelInfo{
		"qwen":        {DisplayName: "Qwen generic"},
		"qwen@nimbus": {DisplayName: "Qwen3.8-Flash-Next"},
	}}
	if got := c.info("qwen", "nimbus").DisplayName; got != "Qwen3.8-Flash-Next" {
		t.Errorf("info(qwen, nimbus) = %q", got)
	}
	if got := c.info("qwen", "cirrus").DisplayName; got != "Qwen generic" {
		t.Errorf("info(qwen, cirrus) = %q", got)
	}
}
