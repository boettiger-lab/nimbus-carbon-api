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

// overrideConfig is the cluster after the TP2 pair was replaced by a single-GPU
// model on nimbus2: the node owns only itself, and the pair's worker is carried
// on the pair's own "model@node" entry so its history keeps both ranks.
func overrideConfig() Config {
	nodes, err := ParseNodes(`[
	  {"name":"nimbus","gpu_hardware":"NVIDIA GB10"},
	  {"name":"nimbus2","gpu_hardware":"NVIDIA GB10"}
	]`)
	if err != nil {
		panic(err)
	}
	models, err := ParseModels(`{
	  "deepseek-v4-flash@nimbus2": {"display_name":"DeepSeek","power_hosts":["nimbus2","nimbus4"],"gpu_count":2},
	  "nemotron-3.5-super@nimbus2": {"display_name":"Nemotron"}
	}`)
	if err != nil {
		panic(err)
	}
	return Config{Nodes: nodes, Models: models}
}

// TestModelPowerHostsOverride: the worker's watts go to the pair alone, joined
// on the model, and the node term no longer reads nimbus4 at all.
func TestModelPowerHostsOverride(t *testing.T) {
	c := overrideConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	q := c.attributedPower("5m", c.presence())
	if !strings.HasPrefix(q, "(sum by (node, namespace, model_name) (") {
		t.Errorf("override terms not summed per model: %s", q)
	}
	if !strings.Contains(q, `DCGM_FI_DEV_POWER_USAGE{Hostname=~"nimbus|nimbus2"}`) {
		t.Errorf("node term should read only the nodes' own hosts: %s", q)
	}
	if !strings.Contains(q, `avg_over_time(DCGM_FI_DEV_POWER_USAGE{Hostname=~"nimbus4"}[5m])`) {
		t.Errorf("extra host not read (smoothed): %s", q)
	}
	if !strings.Contains(q, `"node", "nimbus2", "", ""), "model_name", "deepseek-v4-flash", "", "")`) ||
		!strings.Contains(q, "* on (node, model_name) group_right ()") {
		t.Errorf("extra power not pinned to the overriding model: %s", q)
	}
	if strings.Count(q, `"part"`) != 2 {
		t.Errorf("want one node term and one override term: %s", q)
	}
	if got := c.powerHostsFor("nemotron-3.5-super", "nimbus2"); len(got) != 1 || got[0] != "nimbus2" {
		t.Errorf("nemotron should draw only nimbus2: %v", got)
	}
	if got := c.gpuCountFor("deepseek-v4-flash", "nimbus2"); got != 2 {
		t.Errorf("deepseek gpu_count = %d, want 2", got)
	}
	if got := c.gpuCountFor("nemotron-3.5-super", "nimbus2"); got != 1 {
		t.Errorf("nemotron gpu_count = %d, want 1", got)
	}
}

// TestNoOverrideKeepsQueryShape: with no overrides the query is byte-identical
// to the pre-override form, so existing deployments see no change.
func TestNoOverrideKeepsQueryShape(t *testing.T) {
	c := clusterConfig()
	q := c.attributedPower("", c.presence())
	want := "(" + c.dcgmByNode("DCGM_FI_DEV_POWER_USAGE", "sum", "") + " * on (node) group_right () " + c.presence() + ")"
	if q != want {
		t.Errorf("query changed without overrides:\n got %s\nwant %s", q, want)
	}
}

// TestValidateRejectsBadOverrides: each way an override could double-count or
// mean nothing is refused at startup.
func TestValidateRejectsBadOverrides(t *testing.T) {
	base, _ := ParseNodes(`[{"name":"nimbus2"},{"name":"nimbus4"}]`)
	cases := map[string]string{
		"bare key":           `{"deepseek": {"power_hosts":["nimbus2","nimbus9"]}}`,
		"unknown node":       `{"deepseek@nimbus7": {"power_hosts":["nimbus7"]}}`,
		"drops node host":    `{"deepseek@nimbus2": {"power_hosts":["nimbus9"]}}`,
		"host owned by node": `{"deepseek@nimbus2": {"power_hosts":["nimbus2","nimbus4"]}}`,
	}
	for name, raw := range cases {
		models, err := ParseModels(raw)
		if err != nil {
			t.Fatalf("%s: ParseModels: %v", name, err)
		}
		if err := (Config{Nodes: base, Models: models}).Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	// Two models on DIFFERENT nodes claiming one extra host could overlap.
	two, _ := ParseNodes(`[{"name":"a"},{"name":"b"}]`)
	models, _ := ParseModels(`{"m@a": {"power_hosts":["a","w"]}, "n@b": {"power_hosts":["b","w"]}}`)
	if err := (Config{Nodes: two, Models: models}).Validate(); err == nil {
		t.Error("expected an error when models on two nodes share an extra host")
	}
	// Two models on the SAME node may: only one serves there at a time.
	models, _ = ParseModels(`{"m@a": {"power_hosts":["a","w"]}, "n@a": {"power_hosts":["a","w"]}}`)
	if err := (Config{Nodes: two, Models: models}).Validate(); err != nil {
		t.Errorf("same-node models sharing an extra host should be allowed: %v", err)
	}
}

// TestModelUtilAveragesAllHosts: a tensor-parallel model's utilisation is the
// mean over every rank's GPU, replacing (not adding to) its node's own figure.
func TestModelUtilAveragesAllHosts(t *testing.T) {
	c := overrideConfig()
	q := c.attributedUtil("5m", c.presence())
	if !strings.Contains(q, `avg(avg_over_time(DCGM_FI_DEV_GPU_UTIL{Hostname=~"nimbus2|nimbus4"}[5m]))`) {
		t.Errorf("override should average the pair's hosts: %s", q)
	}
	if !strings.Contains(q, `"node", "nimbus2", "", ""), "model_name", "deepseek-v4-flash", "", "")`) {
		t.Errorf("override not pinned to the overriding model: %s", q)
	}
	base := "(" + c.dcgmByNode("DCGM_FI_DEV_GPU_UTIL", "avg", "5m") + " * on (node) group_right () " + c.presence() + ")"
	if !strings.HasSuffix(q, " or "+base+")") {
		t.Errorf("node term must come last so the override wins: %s", q)
	}
	plain := clusterConfig()
	if got := plain.attributedUtil("5m", plain.presence()); got != "("+plain.dcgmByNode("DCGM_FI_DEV_GPU_UTIL", "avg", "5m")+" * on (node) group_right () "+plain.presence()+")" {
		t.Errorf("query changed without overrides: %s", got)
	}
}
