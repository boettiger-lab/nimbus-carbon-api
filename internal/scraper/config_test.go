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
	base, _ := ParseNodes(`[{"name":"nimbus2"},{"name":"nimbus3"},{"name":"nimbus4"}]`)
	cases := map[string]string{
		"bare key":        `{"deepseek": {"power_hosts":["nimbus2","nimbus9"]}}`,
		"unknown node":    `{"deepseek@nimbus7": {"power_hosts":["nimbus7"]}}`,
		"drops node host": `{"deepseek@nimbus2": {"power_hosts":["nimbus9"]}}`,
		// A node's host claimed by models on two different nodes: both
		// could serve while its own node is idle.
		"node host on two nodes": `{"glm@nimbus2": {"power_hosts":["nimbus2","nimbus3"]},
		                            "mimo@nimbus4": {"power_hosts":["nimbus4","nimbus3"]}}`,
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

// pairConfig is the cluster with nimbus3 a node of its own AND (withPair) the
// worker of a TP2 pair headed on nimbus, next to the nimbus2+nimbus4 pair
// whose worker is no node.
func pairConfig(withPair bool) Config {
	nodes, err := ParseNodes(`[
	  {"name":"nimbus","gpu_hardware":"NVIDIA GB10"},
	  {"name":"nimbus2","gpu_hardware":"NVIDIA GB10"},
	  {"name":"nimbus3","gpu_hardware":"NVIDIA GB10"}
	]`)
	if err != nil {
		panic(err)
	}
	raw := `{"deepseek-v4-flash@nimbus2": {"display_name":"DeepSeek","power_hosts":["nimbus2","nimbus4"],"gpu_count":2}`
	if withPair {
		raw += `, "glm-5.3-flash@nimbus": {"display_name":"GLM-5.3-Flash","power_hosts":["nimbus","nimbus3"],"gpu_count":2}`
	}
	models, err := ParseModels(raw + "}")
	if err != nil {
		panic(err)
	}
	return Config{Nodes: nodes, Models: models}
}

// deepseekGolden is attributedPower("", presence()) for pairConfig(false),
// captured from the code BEFORE node-owned hosts could be claimed. Prometheus
// computes every DeepSeek number from this expression, so while it is
// byte-identical, DeepSeek's energy cannot have moved.
const deepseekGolden = `(sum by (node, namespace, model_name) (label_replace((sum by (node) (label_replace(DCGM_FI_DEV_POWER_USAGE{Hostname=~"nimbus|nimbus2|nimbus3"}, "node", "$1", "Hostname", "(.*)")) * on (node) group_right () (max by (node, namespace, model_name) (vllm:num_requests_running{namespace=~"vllm",node=~"nimbus|nimbus2|nimbus3"}) * 0 + 1)), "part", "node", "", "") or label_replace((label_replace(label_replace(sum(DCGM_FI_DEV_POWER_USAGE{Hostname=~"nimbus4"}), "node", "nimbus2", "", ""), "model_name", "deepseek-v4-flash", "", "") * on (node, model_name) group_right () (max by (node, namespace, model_name) (vllm:num_requests_running{namespace=~"vllm",node=~"nimbus|nimbus2|nimbus3"}) * 0 + 1)), "part", "x0", "", "")))`

// TestDeepseekAttributionUnchanged: the nimbus2+nimbus4 override (nimbus4 is
// no node) keeps its ungated term, and adding a pair that claims a node's
// host leaves that term, and the node term, exactly as they were.
func TestDeepseekAttributionUnchanged(t *testing.T) {
	c := pairConfig(false)
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := c.attributedPower("", c.presence()); got != deepseekGolden {
		t.Errorf("DeepSeek's power expression changed:\n got %s\nwant %s", got, deepseekGolden)
	}
	withPair := pairConfig(true)
	if err := withPair.Validate(); err != nil {
		t.Fatalf("Validate with pair: %v", err)
	}
	q := withPair.attributedPower("", withPair.presence())
	inner := strings.TrimSuffix(strings.TrimPrefix(deepseekGolden, "(sum by (node, namespace, model_name) ("), "))")
	terms := strings.Split(inner, " or ")
	if len(terms) != 2 {
		t.Fatalf("golden split into %d terms", len(terms))
	}
	for _, term := range terms {
		if !strings.Contains(q, term) {
			t.Errorf("term lost or altered by the nimbus3 claim:\n%s\nin %s", term, q)
		}
	}
	if strings.Contains(terms[1], "unless") {
		t.Error("nimbus4 is no node; its term must not be gated")
	}
}

// TestNodeHostClaimIsGated: nimbus3's watts go to the pair only while the
// pair serves AND nothing serves on nimbus3; otherwise the node term keeps
// them. Both halves are needed, or a minute of nimbus3 counts twice.
func TestNodeHostClaimIsGated(t *testing.T) {
	c := pairConfig(true)
	for _, ind := range []string{c.presence(), c.active()} {
		q := c.attributedPower("", ind)
		want := `label_replace(((label_replace(label_replace(sum(DCGM_FI_DEV_POWER_USAGE{Hostname=~"nimbus3"}), "node", "nimbus", "", ""), "model_name", "glm-5.3-flash", "", "") * on (node, model_name) group_right () ` +
			ind + `) unless on () vllm:num_requests_running{namespace="vllm",node="nimbus3"}), "part", "x1-nimbus3", "", "")`
		if !strings.Contains(q, want) {
			t.Errorf("gated nimbus3 term missing:\nwant %s\nin   %s", want, q)
		}
		// nimbus3 is still read by the node term, for its own models.
		if !strings.Contains(q, `DCGM_FI_DEV_POWER_USAGE{Hostname=~"nimbus|nimbus2|nimbus3"}`) {
			t.Errorf("node term no longer reads nimbus3: %s", q)
		}
	}
	if q := c.attributedPower("5m", c.presence()); !strings.Contains(q, `sum(avg_over_time(DCGM_FI_DEV_POWER_USAGE{Hostname=~"nimbus3"}[5m]))`) {
		t.Errorf("live gated term not smoothed: %s", q)
	}
	free, gated := c.splitExtras("glm-5.3-flash", "nimbus")
	if len(free) != 0 || len(gated) != 1 || gated[0].owner.Name != "nimbus3" {
		t.Errorf("glm splitExtras = %v, %+v", free, gated)
	}
	free, gated = c.splitExtras("deepseek-v4-flash", "nimbus2")
	if len(free) != 1 || free[0] != "nimbus4" || len(gated) != 0 {
		t.Errorf("deepseek splitExtras = %v, %+v", free, gated)
	}
}

// TestValidateAllowsNodeHostClaim: nimbus3 claimed by two models on the SAME
// head node (glm now, mimo later) is fine: only one serves on nimbus at once.
// A model on another node claiming it too is not.
func TestValidateAllowsNodeHostClaim(t *testing.T) {
	c := pairConfig(true)
	c.Models["mimo-v2.6-flash@nimbus"] = ModelInfo{PowerHosts: []string{"nimbus", "nimbus3"}, GPUCount: 2}
	if err := c.Validate(); err != nil {
		t.Errorf("same-node models claiming nimbus3: %v", err)
	}
	c.Models["other@nimbus2"] = ModelInfo{PowerHosts: []string{"nimbus2", "nimbus3"}}
	if err := c.Validate(); err == nil {
		t.Error("expected an error when a model on nimbus2 also claims nimbus3")
	}
}

// TestCardKey: the same served name on two nodes is one card; a model@node
// entry with its own display name is a card of its own; ids are URL-safe.
func TestCardKey(t *testing.T) {
	c := Config{Models: map[string]ModelInfo{
		"qwen":            {DisplayName: "Qwen3 8B"},
		"qwen@nimbus":     {DisplayName: "Qwen3.8-Flash-Next NVFP4"},
		"q36-mtp@nimbus3": {Description: "no display name"},
	}}
	if a, b := c.cardKey("q36-mtp", "nimbus"), c.cardKey("q36-mtp", "nimbus3"); a != b || a != "q36-mtp" {
		t.Errorf("q36-mtp on two nodes: %q vs %q", a, b)
	}
	if a, b := c.cardKey("qwen", "nimbus"), c.cardKey("qwen", "cirrus"); a == b {
		t.Errorf("a different checkpoint behind a reused name must stay separate: %q", a)
	}
	if got := c.cardKey("qwen", "nimbus2"); got != "Qwen3 8B" {
		t.Errorf("bare display name not used: %q", got)
	}
	if got := cardID("q36-mtp-stock"); got != "q36-mtp-stock" {
		t.Errorf("URL-safe key should be its own id: %q", got)
	}
	id := cardID("Qwen3.8-Flash-Next NVFP4")
	if !urlSafe.MatchString(id) || !strings.HasPrefix(id, "Qwen3.8-Flash-Next-NVFP4-") {
		t.Errorf("display-name id not URL-safe: %q", id)
	}
	if cardID("Qwen3.8-Flash-Next NVFP4") != id {
		t.Error("card id not stable")
	}
	if cardID("a b") == cardID("a/b") {
		t.Error("keys that slug alike must still get distinct ids")
	}
}
