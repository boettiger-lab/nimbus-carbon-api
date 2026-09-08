package scraper

import (
	"strings"
	"testing"
)

func twoNodeConfig() Config {
	return Config{Nodes: []NodeConfig{
		{Name: "cirrus", Namespace: "vllm", GPUHardware: "Quadro RTX 8000", GPUCount: 2, Container: "vllm", NodePower: true},
		{Name: "nimbus", Namespace: "vllm", GPUHardware: "NVIDIA GB10", GPUCount: 1, Container: "vllm", NodePower: true},
	}}
}

// TestKeyForRequiresBothLabels is the regression test for the misattribution
// bug: two nodes served models out of the SAME namespace, so keying state by
// namespace alone summed nimbus's tokens into cirrus's row.
func TestKeyForRequiresBothLabels(t *testing.T) {
	c := twoNodeConfig()

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

// TestVLLMSelectorFiltersNodes guards the selector both nodes' queries share.
func TestVLLMSelectorFiltersNodes(t *testing.T) {
	sel := twoNodeConfig().vllmSelector()
	if !strings.Contains(sel, `node=~"cirrus|nimbus"`) {
		t.Errorf("selector missing node filter: %s", sel)
	}
	if !strings.Contains(sel, `namespace=~"vllm"`) {
		t.Errorf("selector missing namespace filter: %s", sel)
	}
}

// TestVLLMQueryGroupsByNode: without `node` in the by-clause the two nodes
// collapse back into one series, which is the bug in a different disguise.
func TestVLLMQueryGroupsByNode(t *testing.T) {
	s := NewWithConfig("http://prometheus", 0, twoNodeConfig())
	q := s.vllmQuery(`rate(vllm:generation_tokens_total{%s}[2m])`, "model_name")
	if !strings.HasPrefix(q, "sum by (node, namespace, model_name)") {
		t.Errorf("query does not group by node: %s", q)
	}
	if !strings.Contains(q, `node=~"cirrus|nimbus"`) {
		t.Errorf("query does not filter by node: %s", q)
	}
}

// TestPowerQueriesSplitByMode: node-power nodes are read by DCGM Hostname,
// namespace-attributed nodes by (Hostname, namespace).
func TestPowerQueriesSplitByMode(t *testing.T) {
	c := twoNodeConfig()
	c.Nodes = append(c.Nodes, NodeConfig{Name: "thelio", Namespace: "vllm", GPUCount: 1, NodePower: false})
	s := NewWithConfig("http://prometheus", 0, c)

	nodeScoped, nsScoped := s.powerQueries("[5m]")
	if !strings.Contains(nodeScoped, `sum by (Hostname)`) || !strings.Contains(nodeScoped, `Hostname=~"cirrus|nimbus"`) {
		t.Errorf("node-scoped power query wrong: %s", nodeScoped)
	}
	if !strings.Contains(nsScoped, `sum by (Hostname, namespace)`) || !strings.Contains(nsScoped, `Hostname=~"thelio"`) {
		t.Errorf("namespace-scoped power query wrong: %s", nsScoped)
	}
	if !strings.Contains(nodeScoped, "avg_over_time") {
		t.Errorf("instant power query should smooth over the window: %s", nodeScoped)
	}
	// An empty range selector means a raw range query (backfill / timeseries).
	raw, _ := s.powerQueries("")
	if strings.Contains(raw, "avg_over_time") {
		t.Errorf("range power query should not wrap avg_over_time: %s", raw)
	}
}

// TestParseNodesDefaults: omitted fields get sane values, bad input is loud.
func TestParseNodesDefaults(t *testing.T) {
	nodes, err := ParseNodes(`[{"name":"cirrus","gpu_hardware":"Quadro RTX 8000","gpu_count":2,"node_power":true}]`)
	if err != nil {
		t.Fatalf("ParseNodes: %v", err)
	}
	if nodes[0].Namespace != "vllm" || nodes[0].Container != "vllm" {
		t.Errorf("defaults not applied: %+v", nodes[0])
	}
	if _, err := ParseNodes(`[]`); err == nil {
		t.Error("expected an error for an empty node list")
	}
	if _, err := ParseNodes(`[{"namespace":"vllm"}]`); err == nil {
		t.Error("expected an error for a node with no name")
	}
}

// TestNodeForNamespaceAmbiguity: the legacy /api/v1/carbon/{namespace}/...
// route may only resolve when exactly one node serves that namespace.
func TestNodeForNamespace(t *testing.T) {
	single := NewWithConfig("http://prometheus", 0, Config{Nodes: []NodeConfig{
		{Name: "cirrus", Namespace: "vllm"},
	}})
	if got := single.nodeForNamespace("vllm"); got != "cirrus" {
		t.Errorf("nodeForNamespace(vllm) = %q, want cirrus", got)
	}
	shared := NewWithConfig("http://prometheus", 0, twoNodeConfig())
	if got := shared.nodeForNamespace("vllm"); got != "" {
		t.Errorf("nodeForNamespace(vllm) = %q, want empty (ambiguous)", got)
	}
}
