package stats

import (
	"html"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRenderDashboard(t *testing.T) {
	pageHTML, err := renderDashboard(Config{
		Title:       `Stats <script>alert("x")</script>`,
		Description: `Description <img src=x>`,
		Footer:      `Footer & details`,
		FaviconURL:  "/assets/favicon.svg",
		Refresh:     3 * time.Second,
	})
	mustNoError(t, err)
	mustContain(t, pageHTML, `Stats &lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;`)
	mustNotContain(t, pageHTML, `<script>alert("x")</script>`)
	mustContain(t, pageHTML, `Description &lt;img src=x&gt;`)
	mustContain(t, pageHTML, `Footer &amp; details`)
	mustContain(t, pageHTML, `href="/assets/favicon.svg"`)
	mustRegexp(t, `const REFRESH_MS =\s+3000\s*;`, pageHTML)
	descriptorLabel := "FDs"
	if runtime.GOOS == "windows" {
		descriptorLabel = "Handles"
	}
	mustContain(t, pageHTML, "<dt>"+descriptorLabel+"</dt>")
}

func TestDashboardContract(t *testing.T) {
	pageHTML, err := renderDashboard(ConfigDefault)
	mustNoError(t, err)
	mustContain(t, html.UnescapeString(pageHTML), "data:image/svg+xml;base64,")
	mustEqual(t, 4, strings.Count(pageHTML, `class="metric-card"`))
	mustEqual(t, 8, strings.Count(pageHTML, "<canvas "))
	mustContain(t, pageHTML, `const MAX_SAMPLES = 90;`)
	mustContain(t, pageHTML, `let currentSampleWindow = 60;`)
	mustContain(t, pageHTML, `Accept: "application/json"`)
	mustContain(t, pageHTML, `@media (prefers-color-scheme: dark)`)
	mustContain(t, pageHTML, `@media (max-width: 980px)`)
	mustContain(t, pageHTML, `@media (max-width: 560px)`)
	mustContain(t, pageHTML, `grid-template-columns: repeat(4, minmax(0, 1fr))`)
	mustContain(t, pageHTML, `grid-template-columns: repeat(2, minmax(0, 1fr))`)
	mustContain(t, pageHTML, `class="metric-pager"`)
	mustContain(t, pageHTML, `width: min(1360px, calc(100% - 32px))`)
	mustContain(t, pageHTML, `border: 3px solid var(--border)`)
	mustContain(t, pageHTML, `class="page-scroll-dock"`)
	mustNotContain(t, pageHTML, "lang-toggle")
	mustNotContain(t, pageHTML, `data-active="en"`)
	mustNotContain(t, pageHTML, "container-card")
	mustNotContain(t, strings.ToLower(pageHTML), "chart.js")
	mustNotContain(t, strings.ToLower(pageHTML), "cdn")
}

func TestDashboardInteractions(t *testing.T) {
	pageHTML, err := renderDashboard(ConfigDefault)
	mustNoError(t, err)

	for _, sample := range []string{`data-samples="30"`, `data-samples="60"`, `data-samples="90"`} {
		mustContain(t, pageHTML, sample)
	}
	mustEqual(t, 3, strings.Count(pageHTML, `class="sample-option"`))
	mustContain(t, pageHTML, `storageSet("stats.samples"`)
	mustContain(t, pageHTML, `visibleSamples(values)`)

	mustContain(t, pageHTML, `id="theme-toggle"`)
	mustContain(t, pageHTML, `prefers-color-scheme: dark`)
	mustContain(t, pageHTML, `storageSet("stats.theme"`)
	mustContain(t, pageHTML, `currentTheme === "dark" ? "light" : "dark"`)
	mustContain(t, pageHTML, `.control-dot:hover`)
	mustContain(t, pageHTML, `transition: none`)
	mustContain(t, pageHTML, `data-series="bad"></span>P99`)
	mustContain(t, pageHTML, `fillForward(history.p95)`)
	mustContain(t, pageHTML, `dash: [6, 4]`)
	mustContain(t, pageHTML, `fillZero(error4xx)`)
	mustContain(t, pageHTML, `rawData: error4xx`)
	mustContain(t, pageHTML, `No requests in this window`)
	mustContain(t, pageHTML, `Unsupported on this platform`)
	mustContain(t, pageHTML, `collectionErrors.includes("system.load")`)
	mustContain(t, pageHTML, `runtime.gc_pause_metrics_enabled === true`)
	mustContain(t, pageHTML, `: "Disabled"`)
	mustContain(t, pageHTML, `"GC " + optional(currentRuntime.gc_count`)

	for _, id := range []string{
		`id="heap-details-button"`,
		`id="gc-details-button"`,
		`id="disk-details-button"`,
		`id="http-status-button"`,
		`id="heap-modal"`,
		`id="gc-modal"`,
		`id="disk-modal"`,
		`id="http-status-modal"`,
	} {
		mustContain(t, pageHTML, id)
	}
	for _, label := range []string{
		"Heap In-use",
		"Heap Released",
		"GOMAXPROCS",
		"Window GC Pause",
		"GC CPU Fraction",
		"Application filesystem",
		"Usage ",
		"4xx Rate",
		"5xx Rate",
	} {
		mustContain(t, pageHTML, label)
	}
	mustContain(t, pageHTML, `event.key === "Escape"`)
	mustContain(t, pageHTML, `event.target === modal`)
	mustContain(t, pageHTML, `byId(binding.trigger).focus()`)
	mustContain(t, pageHTML, `id="http-error-chart"`)
	mustContain(t, pageHTML, `id="gc-pause-chart"`)
}

func TestRenderDashboardSanitizesUnvalidatedFavicon(t *testing.T) {
	pageHTML, err := renderDashboard(Config{FaviconURL: "javascript:alert(1)", Refresh: time.Second})
	mustNoError(t, err)
	mustContain(t, pageHTML, `<link rel="icon" href="#ZgotmplZ">`)
}
