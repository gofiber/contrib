package uptime

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"html/template"
	"time"
)

type dashboardPage struct {
	Title       string
	Description string
	Footer      string
	APIPathJSON template.JS
	RefreshMS   int64
	StatusJSON  template.JS
}

var dashboardTemplate = template.Must(template.New("uptime").Parse(dashboardHTML))

func renderDashboardHTML(config Config, status StatusResponse) (string, error) {
	statusJSON, err := json.Marshal(status)
	if err != nil {
		return "", err
	}
	apiPathJSON, err := json.Marshal(config.UI.Path + "/api/status")
	if err != nil {
		return "", err
	}
	page := dashboardPage{
		Title:       config.UI.Title,
		Description: config.UI.Description,
		Footer:      config.UI.Footer,
		APIPathJSON: template.JS(apiPathJSON),
		RefreshMS:   max(int64(config.SampleInterval/time.Millisecond), 10000),
		StatusJSON:  template.JS(statusJSON),
	}
	var buf bytes.Buffer
	if err := dashboardTemplate.Execute(&buf, page); err != nil {
		return "", err
	}
	return buf.String(), nil
}

//go:embed dashboard.gohtml
var dashboardHTML string
