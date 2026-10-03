package prometheus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var (
	// dashboardMetric matches a family a dashboard query names through its prefix
	// variable, which stands in for Namespace and Subsystem.
	dashboardMetric = regexp.MustCompile(`\$\{prefix\}_([a-z_]+)`)
	// dashboardLink matches a link from one dashboard to another by uid.
	dashboardLink = regexp.MustCompile(`/d/([^/?]+)`)
)

// TestGrafanaDashboards keeps the dashboards under grafana/ in step with the
// middleware: a renamed family would leave every panel built on it empty, and a
// hardcoded data source uid would tie the import to one Grafana instance.
func TestGrafanaDashboards(t *testing.T) {
	known := make(map[string]bool)
	for _, metric := range allMetrics {
		known[string(metric)] = true
	}
	for _, histogram := range []Metric{MetricRequestDuration, MetricRequestSize, MetricResponseSize} {
		for _, series := range []string{"_bucket", "_sum", "_count"} {
			known[string(histogram)+series] = true
		}
	}

	files, err := filepath.Glob(filepath.Join("grafana", "*.json"))
	if err != nil {
		t.Fatalf("listing dashboards: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected dashboards in grafana/")
	}

	uids := make(map[string]string, len(files))
	links := make(map[string][]string)
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}

		var dashboard map[string]any
		if err := json.Unmarshal(raw, &dashboard); err != nil {
			t.Fatalf("%s is not valid JSON: %v", file, err)
		}

		uid, _ := dashboard["uid"].(string)
		if uid == "" {
			t.Fatalf("%s has no uid, so links to it would break on every import", file)
		}
		if other, ok := uids[uid]; ok {
			t.Fatalf("%s and %s share the uid %q", file, other, uid)
		}
		uids[uid] = file

		references := 0
		panelIDs := make(map[float64]bool)
		walkDashboard(dashboard, func(node map[string]any) {
			for key, value := range node {
				if text, ok := value.(string); ok {
					for _, match := range dashboardMetric.FindAllStringSubmatch(text, -1) {
						references++
						if !known[match[1]] {
							t.Errorf("%s queries %q, which the middleware does not expose", file, match[0])
						}
					}
					if key == "url" {
						for _, match := range dashboardLink.FindAllStringSubmatch(text, -1) {
							links[file] = append(links[file], match[1])
						}
					}
				}
			}

			if _, isPanel := node["gridPos"]; isPanel {
				id, _ := node["id"].(float64)
				if panelIDs[id] {
					t.Errorf("%s has more than one panel with id %v", file, id)
				}
				panelIDs[id] = true
			}

			if source, ok := node["datasource"].(map[string]any); ok && source["type"] == "prometheus" &&
				source["uid"] != "${datasource}" {
				t.Errorf("%s pins the data source %v instead of using the datasource variable", file, source["uid"])
			}
		})

		if references == 0 {
			t.Errorf("%s queries none of the middleware's metrics", file)
		}
	}

	for file, targets := range links {
		for _, uid := range targets {
			if _, ok := uids[uid]; !ok {
				t.Errorf("%s links to the dashboard %q, which is not shipped", file, uid)
			}
		}
	}
}

// walkDashboard calls visit for every JSON object in the decoded dashboard.
func walkDashboard(node any, visit func(map[string]any)) {
	switch value := node.(type) {
	case map[string]any:
		visit(value)
		for _, child := range value {
			walkDashboard(child, visit)
		}
	case []any:
		for _, child := range value {
			walkDashboard(child, visit)
		}
	}
}
