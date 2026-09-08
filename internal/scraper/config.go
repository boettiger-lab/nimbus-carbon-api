package scraper

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// NodeConfig describes one GPU node serving models. The scraper keys all of
// its state by node name, so every node in a cluster gets its own row in the
// API and its own card on the dashboard.
//
// Both selectors matter. Namespace alone is not enough: on this cluster the
// cirrus and nimbus vLLM deployments both live in the "vllm" namespace, so a
// namespace-only query sums one node's tokens into the other's row while the
// power stays node-scoped — which silently understates CO2 per token.
type NodeConfig struct {
	// Name is the Kubernetes node name. It is matched against the `node`
	// label on vLLM metrics and the `Hostname` label DCGM exports.
	Name string `json:"name"`
	// Namespace is the namespace the vLLM workload runs in.
	Namespace string `json:"namespace"`
	// GPUHardware and GPUCount are display strings for the dashboard card.
	GPUHardware string `json:"gpu_hardware"`
	GPUCount    int    `json:"gpu_count"`
	// Container is the serving container name (display only).
	Container string `json:"container"`
	// NodePower attributes TOTAL node GPU power to this node's model rather
	// than reading DCGM's per-namespace attribution.
	//
	// Set it whenever the node's GPUs are shared with anything else — it is a
	// deliberate upper bound, flagged in the API as power_is_node_total and
	// labelled on the dashboard. It is also the only thing that works when
	// DCGM's pod-resources mapping attributes a GPU to some other tenant:
	// nimbus's GB10, for instance, reports its power under whichever pod the
	// mapping picked (currently an MCP pod in "default"), not under vLLM, so
	// a namespace-scoped power query there returns nothing at all.
	NodePower bool `json:"node_power"`
}

// Config is the set of nodes this instance reports on. One deployment covers
// the whole cluster; there is no longer one Deployment and one hostname per
// machine.
type Config struct {
	Nodes []NodeConfig
}

// DefaultConfig returns the single-node nimbus configuration the service
// started life with. Kept so the zero-configuration path still runs.
func DefaultConfig() Config {
	return Config{Nodes: []NodeConfig{{
		Name:        "nimbus",
		Namespace:   "default",
		GPUHardware: "NVIDIA GB10",
		GPUCount:    1,
		Container:   "vllm",
		NodePower:   false,
	}}}
}

// ParseNodes reads a JSON array of NodeConfig objects, applying defaults for
// omitted fields.
func ParseNodes(raw string) ([]NodeConfig, error) {
	var nodes []NodeConfig
	if err := json.Unmarshal([]byte(raw), &nodes); err != nil {
		return nil, fmt.Errorf("parsing node config: %w", err)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("node config is empty")
	}
	for i := range nodes {
		if nodes[i].Name == "" {
			return nil, fmt.Errorf("node %d has no name", i)
		}
		if nodes[i].Namespace == "" {
			nodes[i].Namespace = "vllm"
		}
		if nodes[i].Container == "" {
			nodes[i].Container = "vllm"
		}
		if nodes[i].GPUCount == 0 {
			nodes[i].GPUCount = 1
		}
	}
	return nodes, nil
}

// node returns the configured node with this name, or nil.
func (c Config) node(name string) *NodeConfig {
	for i := range c.Nodes {
		if c.Nodes[i].Name == name {
			return &c.Nodes[i]
		}
	}
	return nil
}

// keyFor maps a (node, namespace) label pair from a query result to the state
// key it belongs to, or "" if no configured node claims it. Guarding on both
// labels is what keeps one node's tokens out of another's row.
func (c Config) keyFor(node, namespace string) string {
	n := c.node(node)
	if n == nil || n.Namespace != namespace {
		return ""
	}
	return n.Name
}

// nodeMatcher builds a PromQL regex alternation over the given values,
// anchored implicitly by Prometheus (`=~` is fully anchored).
func alternation(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, regexp.QuoteMeta(v))
	}
	return strings.Join(quoted, "|")
}

// nodeNames returns the names of every configured node.
func (c Config) nodeNames() []string {
	out := make([]string, 0, len(c.Nodes))
	for _, n := range c.Nodes {
		out = append(out, n.Name)
	}
	return out
}

// namespaces returns the distinct namespaces across configured nodes.
func (c Config) namespaces() []string {
	seen := make(map[string]bool, len(c.Nodes))
	out := make([]string, 0, len(c.Nodes))
	for _, n := range c.Nodes {
		if !seen[n.Namespace] {
			seen[n.Namespace] = true
			out = append(out, n.Namespace)
		}
	}
	return out
}

// nodesByPowerMode splits configured node names by how their GPU power must
// be read: total-node (DCGM Hostname) versus per-namespace attribution.
func (c Config) nodesByPowerMode() (nodePower, namespacePower []string) {
	for _, n := range c.Nodes {
		if n.NodePower {
			nodePower = append(nodePower, n.Name)
		} else {
			namespacePower = append(namespacePower, n.Name)
		}
	}
	return nodePower, namespacePower
}

// vllmSelector is the label matcher shared by every vLLM query: the
// configured namespaces AND the configured nodes.
func (c Config) vllmSelector() string {
	return fmt.Sprintf(`namespace=~%q,node=~%q`, alternation(c.namespaces()), alternation(c.nodeNames()))
}
