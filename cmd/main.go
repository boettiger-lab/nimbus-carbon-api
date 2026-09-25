package main

import (
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/boettiger-lab/nimbus-carbon-api/internal/scraper"
)

//go:embed static/dashboard.html
var dashboardHTML []byte

//go:embed static/methodology.html
var methodologyHTML []byte

var s *scraper.Scraper

func main() {
	promURL := getenv("PROMETHEUS_URL", "http://prometheus-server.monitoring.svc.cluster.local")
	interval := getenvDuration("SCRAPE_INTERVAL", 30*time.Second)
	addr := getenv("LISTEN_ADDR", ":8080")

	cfg := loadNodes()

	s = scraper.NewWithConfig(promURL, interval, cfg)
	go s.Run()
	for _, n := range cfg.Nodes {
		log.Printf("carbon-api node: name=%s ns=%s gpu=%q count=%d power_hosts=%v",
			n.Name, n.Namespace, n.GPUHardware, n.GPUCount, n.PowerHosts)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleDashboard)
	mux.HandleFunc("/methodology", handleMethodology)
	mux.HandleFunc("/api/v1/models", handleModels)
	mux.HandleFunc("/api/v1/carbon", handleModels) // the pre-2026-09 name, kept for old links
	mux.HandleFunc("/api/v1/carbon/timeseries", handleTimeSeries)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	log.Printf("carbon-api listening on %s (prometheus=%s, interval=%s)", addr, promURL, interval)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// loadNodes builds the node set this instance reports on.
//
// Preferred: NODES_FILE (a JSON array from a ConfigMap) or NODES_JSON
// (the same JSON inline), e.g.
//
//	[{"name":"cirrus","gpu_hardware":"Quadro RTX 8000","gpu_count":2},
//	 {"name":"nimbus2","gpu_hardware":"NVIDIA GB10","gpu_count":2,
//	  "power_hosts":["nimbus2","nimbus4"]}]
//
// Legacy single-node env vars (NODE_NAME/NAMESPACE/GPU_HARDWARE/GPU_COUNT/
// CONTAINER) still work and describe exactly one node.
func loadNodes() scraper.Config {
	cfg := loadNodeList()
	// MODELS_FILE: optional display names, {"served-name": {"display_name": ...}}.
	if path := os.Getenv("MODELS_FILE"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			log.Fatalf("reading MODELS_FILE %s: %v", path, err)
		}
		models, err := scraper.ParseModels(string(raw))
		if err != nil {
			log.Fatalf("MODELS_FILE %s: %v", path, err)
		}
		cfg.Models = models
	}
	return cfg
}

func loadNodeList() scraper.Config {
	if path := os.Getenv("NODES_FILE"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			log.Fatalf("reading NODES_FILE %s: %v", path, err)
		}
		nodes, err := scraper.ParseNodes(string(raw))
		if err != nil {
			log.Fatalf("NODES_FILE %s: %v", path, err)
		}
		return scraper.Config{Nodes: nodes}
	}
	if raw := os.Getenv("NODES_JSON"); raw != "" {
		nodes, err := scraper.ParseNodes(raw)
		if err != nil {
			log.Fatalf("NODES_JSON: %v", err)
		}
		return scraper.Config{Nodes: nodes}
	}

	single := scraper.DefaultConfig().Nodes[0]
	single.Name = getenv("NODE_NAME", single.Name)
	single.Namespace = getenv("NAMESPACE", single.Namespace)
	single.GPUHardware = getenv("GPU_HARDWARE", single.GPUHardware)
	single.Container = getenv("CONTAINER", single.Container)
	single.GPUCount = getenvInt("GPU_COUNT", single.GPUCount)
	single.PowerHosts = []string{single.Name}
	return scraper.Config{Nodes: []scraper.NodeConfig{single}}
}

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(dashboardHTML)
}

func handleMethodology(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(methodologyHTML)
}

// GET /api/v1/carbon/timeseries?range=24h|7d|15d
// Cluster-wide LLM power and CO2 over time, from Prometheus history.
func handleTimeSeries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rangeDur := parseDuration(r.URL.Query().Get("range"), 24*time.Hour)
	// Prometheus keeps 15 days; asking for more returns the same data.
	if rangeDur > 15*24*time.Hour {
		rangeDur = 15 * 24 * time.Hour
	}
	step := 5 * time.Minute
	switch {
	case rangeDur > 8*24*time.Hour:
		step = 2 * time.Hour
	case rangeDur > 25*time.Hour:
		step = time.Hour
	}

	pts, err := s.ClusterTimeSeries(rangeDur, step)
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	writeJSON(w, map[string]interface{}{
		"range":  rangeDur.String(),
		"step":   step.String(),
		"points": pts,
		"error":  errMsg,
	})
}

// GET /api/v1/models
// One record per model: live state (null when offline), aggregates for every
// window, and an availability strip per window.
func handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type window struct {
		Name    string `json:"name"`
		Seconds int64  `json:"seconds"`
	}
	var windows []window
	for _, win := range s.Windows() {
		windows = append(windows, window{win.Name, int64(win.Dur.Seconds())})
	}
	body := map[string]interface{}{
		"models":     s.Models(),
		"windows":    windows,
		"updated_at": s.Updated(),
	}
	if rs := s.RetentionStart(); !rs.IsZero() {
		body["retention_start"] = rs
	}
	writeJSON(w, body)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(v)
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenvDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	// accept bare seconds integer or Go duration string
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	// time.ParseDuration only handles up to "h"; handle "d" manually.
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err == nil {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}
