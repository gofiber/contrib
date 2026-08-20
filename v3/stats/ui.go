package stats

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"html/template"
	"os"
	"runtime"
	"time"
)

const defaultStatsFaviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">
<rect width="64" height="64" rx="14" fill="#0f172a"/>
<path d="M8 34h13l6-17 11 32 7-20 4 5h7" fill="none" stroke="#67e8f9" stroke-width="6" stroke-linecap="round" stroke-linejoin="round"/>
</svg>`

var defaultStatsFaviconURL = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(defaultStatsFaviconSVG))

//go:embed dashboard.gohtml
var dashboardHTML string

// dashboardTemplate is parsed once per package and executed once per middleware
// instance. Dashboard requests serve the pre-rendered string and never perform
// template work on the request path.
var dashboardTemplate = template.Must(template.New("stats").Parse(dashboardHTML))

type dashboardPage struct {
	Title           string
	Description     string
	Footer          string
	FaviconURL      any
	RefreshMS       int64
	DescriptorLabel string
	PID             int
}

func renderDashboard(config Config) (string, error) {
	var faviconURL any = config.FaviconURL
	if config.FaviconURL == "" {
		faviconURL = template.URL(defaultStatsFaviconURL) // #nosec G203 -- package-owned embedded SVG.
	}
	descriptorLabel := "FDs"
	if runtime.GOOS == "windows" {
		descriptorLabel = "Handles"
	}
	page := dashboardPage{
		Title:           config.Title,
		Description:     config.Description,
		Footer:          config.Footer,
		FaviconURL:      faviconURL,
		RefreshMS:       max(config.Refresh.Milliseconds(), int64(time.Second/time.Millisecond)),
		DescriptorLabel: descriptorLabel,
		PID:             os.Getpid(),
	}
	var output bytes.Buffer
	if err := dashboardTemplate.Execute(&output, page); err != nil {
		return "", err
	}
	return output.String(), nil
}
