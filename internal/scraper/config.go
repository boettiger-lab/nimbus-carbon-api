package scraper

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// NodeConfig describes one GPU node that serves (or has served) models. State
// is keyed by MODEL, not node — see Model — but every model is attributed to
// the node whose vLLM API server exports its metrics, and that node's entry
// says which hardware it runs on and whose power it draws.
//
// Both selectors matter. Namespace alone is not enough: every vLLM
// deployment on this cluster lives in the "vllm" namespace, so a
// namespace-only query sums one node's tokens into another's row.
type NodeConfig struct {
	// Name is the Kubernetes node name. It is matched against the `node`
	// label on vLLM metrics.
	Name string `json:"name"`
	// Namespace is the namespace the vLLM workload runs in.
	Namespace string `json:"namespace"`
	// GPUHardware and GPUCount are display strings for the dashboard card.
	// For a multi-node (tensor-parallel) model GPUCount is the total across
	// every host in PowerHosts.
	GPUHardware string `json:"gpu_hardware"`
	GPUCount    int    `json:"gpu_count"`
	// Container is the serving container name (display only).
	Container string `json:"container"`
	// PowerHosts are the DCGM `Hostname`s whose GPU power is attributed to the
	// model served from this node. Defaults to [Name]. A tensor-parallel model
	// spanning two machines lists both: its metrics are exported by the head's
	// node only, but it burns power on every rank.
	PowerHosts []string `json:"power_hosts"`
	// NodePower is accepted for compatibility with older ConfigMaps and is
	// ignored: power is ALWAYS the node total, attributed to a model only
	// while that model is serving there. Per-tenant DCGM attribution cannot be
	// trusted on this cluster (it names whichever pod the pod-resources mapping
	// picked — on nimbus, an MCP pod in "default").
	NodePower bool `json:"node_power,omitempty"`
}

// ModelInfo is optional metadata for a served model name.
type ModelInfo struct {
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
	// PowerHosts and GPUCount override the node's for this one model, and are
	// only accepted on a "model@node" key. They exist because a node's
	// hardware outlives any one model on it: nimbus2 served a tensor-parallel
	// pair (head nimbus2, worker nimbus4) and then a single-GPU model. Put the
	// pair's hosts on the node and the single model is charged for an idle
	// worker; take them off and the pair's history (recomputed from
	// Prometheus on every scrape) loses half its energy. On the model, each
	// is right for as long as Prometheus remembers it.
	//
	// PowerHosts must include every one of the node's own PowerHosts (it adds
	// hosts, it cannot drop them), and the hosts it adds must not belong to
	// any node: a host claimed by a node and by a model would count twice
	// whenever both serve.
	PowerHosts []string `json:"power_hosts,omitempty"`
	GPUCount   int      `json:"gpu_count,omitempty"`
}

// Config is the set of nodes this instance reports on, plus optional display
// names for the models they serve.
type Config struct {
	Nodes []NodeConfig
	// Models maps a vLLM served model name, or "model@node" to disambiguate a
	// name reused on two nodes, to its display metadata.
	Models map[string]ModelInfo
}

// DefaultConfig returns the single-node nimbus configuration the service
// started life with. Kept so the zero-configuration path still runs.
func DefaultConfig() Config {
	return Config{Nodes: []NodeConfig{{
		Name:        "nimbus",
		Namespace:   "vllm",
		GPUHardware: "NVIDIA GB10",
		GPUCount:    1,
		Container:   "vllm",
		PowerHosts:  []string{"nimbus"},
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
	claimed := make(map[string]string)
	for i := range nodes {
		n := &nodes[i]
		if n.Name == "" {
			return nil, fmt.Errorf("node %d has no name", i)
		}
		if n.Namespace == "" {
			n.Namespace = "vllm"
		}
		if n.Container == "" {
			n.Container = "vllm"
		}
		if n.GPUCount == 0 {
			n.GPUCount = 1
		}
		if len(n.PowerHosts) == 0 {
			n.PowerHosts = []string{n.Name}
		}
		// A host's watts can only be attributed to one node's models, or the
		// same joules are counted twice.
		for _, h := range n.PowerHosts {
			if prev, ok := claimed[h]; ok {
				return nil, fmt.Errorf("power host %q is claimed by both %q and %q", h, prev, n.Name)
			}
			claimed[h] = n.Name
		}
	}
	return nodes, nil
}

// ParseModels reads a JSON object mapping served model names to ModelInfo.
func ParseModels(raw string) (map[string]ModelInfo, error) {
	var models map[string]ModelInfo
	if err := json.Unmarshal([]byte(raw), &models); err != nil {
		return nil, fmt.Errorf("parsing model config: %w", err)
	}
	return models, nil
}

// Validate checks the model overrides against the node set. Call it once both
// halves of the config are loaded.
func (c Config) Validate() error {
	claimed := make(map[string]bool)
	for _, h := range c.powerHosts() {
		claimed[h] = true
	}
	// Two models on the SAME node may share an extra host: only one model
	// serves on a node at a time. Across nodes they could overlap.
	extraBy := make(map[string]string)
	for _, key := range sortedKeys(c.Models) {
		mi := c.Models[key]
		if len(mi.PowerHosts) == 0 && mi.GPUCount == 0 {
			continue
		}
		model, node := splitID(key)
		if model == "" || node == "" {
			return fmt.Errorf("model %q: power_hosts/gpu_count need a \"model@node\" key", key)
		}
		n := c.node(node)
		if n == nil {
			return fmt.Errorf("model %q: node %q is not configured", key, node)
		}
		have := make(map[string]bool, len(mi.PowerHosts))
		for _, h := range mi.PowerHosts {
			have[h] = true
		}
		if len(mi.PowerHosts) > 0 {
			for _, h := range n.PowerHosts {
				if !have[h] {
					return fmt.Errorf("model %q: power_hosts must include node %q's own host %q", key, node, h)
				}
			}
		}
		for _, h := range c.extraHosts(model, node) {
			if prev, ok := extraBy[h]; ok && prev == node {
				continue
			}
			if claimed[h] {
				return fmt.Errorf("model %q: power host %q is already claimed (by a node or another model's node), so it would be counted twice", key, h)
			}
			extraBy[h] = node
			claimed[h] = true
		}
	}
	return nil
}

// powerHostsFor is the full list of hosts whose power belongs to model on
// node: the model's override if it has one, else the node's.
func (c Config) powerHostsFor(model, node string) []string {
	if mi, ok := c.Models[model+"@"+node]; ok && len(mi.PowerHosts) > 0 {
		return mi.PowerHosts
	}
	if n := c.node(node); n != nil {
		return n.PowerHosts
	}
	return nil
}

// gpuCountFor is the model's GPU count override, else the node's.
func (c Config) gpuCountFor(model, node string) int {
	if mi, ok := c.Models[model+"@"+node]; ok && mi.GPUCount > 0 {
		return mi.GPUCount
	}
	if n := c.node(node); n != nil {
		return n.GPUCount
	}
	return 0
}

// extraHosts are the hosts a model override adds beyond its node's own.
func (c Config) extraHosts(model, node string) []string {
	n := c.node(node)
	if n == nil {
		return nil
	}
	own := make(map[string]bool, len(n.PowerHosts))
	for _, h := range n.PowerHosts {
		own[h] = true
	}
	var out []string
	for _, h := range c.powerHostsFor(model, node) {
		if !own[h] {
			out = append(out, h)
		}
	}
	return out
}

// info returns display metadata for a model served on a node, preferring an
// exact "model@node" entry over a bare model-name one.
func (c Config) info(model, node string) ModelInfo {
	if mi, ok := c.Models[model+"@"+node]; ok {
		return mi
	}
	return c.Models[model]
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

// keyFor maps a (node, namespace) label pair from a query result to the node
// it belongs to, or "" if no configured node claims it. Guarding on both
// labels is what keeps one node's tokens out of another's row.
func (c Config) keyFor(node, namespace string) string {
	n := c.node(node)
	if n == nil || n.Namespace != namespace {
		return ""
	}
	return n.Name
}

// alternation builds a PromQL regex alternation over the given values
// (`=~` is fully anchored by Prometheus).
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

// powerHosts returns every DCGM host whose power is attributed to some node.
func (c Config) powerHosts() []string {
	var out []string
	for _, n := range c.Nodes {
		out = append(out, n.PowerHosts...)
	}
	return out
}

// vllmSelector is the label matcher shared by every vLLM query: the
// configured namespaces AND the configured nodes.
func (c Config) vllmSelector() string {
	return fmt.Sprintf(`namespace=~%q,node=~%q`, alternation(c.namespaces()), alternation(c.nodeNames()))
}

// sortedKeys returns m's keys in order, so validation errors are deterministic.
func sortedKeys(m map[string]ModelInfo) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// modelOverrides lists every (model, node) pair whose override adds hosts, in
// a stable order.
func (c Config) modelOverrides() [][2]string {
	var out [][2]string
	for _, key := range sortedKeys(c.Models) {
		model, node := splitID(key)
		if node != "" && len(c.extraHosts(model, node)) > 0 {
			out = append(out, [2]string{model, node})
		}
	}
	return out
}

// OverrideKeys names every "model@node" entry that adds power hosts, with the
// full host list, for the startup log.
func (c Config) OverrideKeys() []string {
	var out []string
	for _, o := range c.modelOverrides() {
		out = append(out, fmt.Sprintf("%s@%s power_hosts=%v gpu_count=%d",
			o[0], o[1], c.powerHostsFor(o[0], o[1]), c.gpuCountFor(o[0], o[1])))
	}
	return out
}
