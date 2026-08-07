package uptime

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"time"
)

const defaultUIFaviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">
<rect width="64" height="64" rx="14" fill="#0f172a"/>
<path d="M8 34h13l6-17 11 32 7-20 4 5h7" fill="none" stroke="#67e8f9" stroke-width="6" stroke-linecap="round" stroke-linejoin="round"/>
</svg>`

var defaultUIFaviconURL = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(defaultUIFaviconSVG))

type dashboardPage struct {
	Title       string
	Description string
	Footer      string
	FaviconURL  any
	APIPathJSON template.JS
	RefreshMS   int64
	StatusJSON  template.JS
}

var dashboardTemplate = template.Must(template.New("uptime").Parse(dashboardHTML))

func renderDashboardHTML(config Config, status StatusResponse, apiPath string) (string, error) {
	var faviconURL any = config.UI.FaviconURL
	if config.UI.FaviconURL == "" {
		faviconURL = template.URL(defaultUIFaviconURL) // #nosec G203 -- this is the package-owned embedded favicon.
	}
	statusJSON, err := json.Marshal(status)
	if err != nil {
		return "", err
	}
	apiPathJSON, err := json.Marshal(apiPath)
	if err != nil {
		return "", err
	}
	page := dashboardPage{
		Title:       config.UI.Title,
		Description: config.UI.Description,
		Footer:      config.UI.Footer,
		FaviconURL:  faviconURL,
		APIPathJSON: template.JS(string(apiPathJSON)),
		RefreshMS:   max(int64(config.SampleInterval/time.Millisecond), 10000),
		StatusJSON:  template.JS(string(statusJSON)),
	}
	var buf bytes.Buffer
	if err := dashboardTemplate.Execute(&buf, page); err != nil {
		return "", err
	}
	return buf.String(), nil
}

//go:embed dashboard.gohtml
var dashboardHTML string
