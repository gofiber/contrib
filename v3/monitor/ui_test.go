package monitor

import (
	"html"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderDashboard(t *testing.T) {
	pageHTML, err := renderDashboard(Config{
		Title:       `Monitor <script>alert("x")</script>`,
		Description: `Description <img src=x>`,
		Footer:      `Footer & details`,
		FaviconURL:  "/assets/favicon.svg",
		Refresh:     3 * time.Second,
	})
	require.NoError(t, err)
	assert.Contains(t, pageHTML, `Monitor &lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;`)
	assert.NotContains(t, pageHTML, `<script>alert("x")</script>`)
	assert.Contains(t, pageHTML, `Description &lt;img src=x&gt;`)
	assert.Contains(t, pageHTML, `Footer &amp; details`)
	assert.Contains(t, pageHTML, `href="/assets/favicon.svg"`)
	assert.Regexp(t, `const REFRESH_MS =\s+3000\s*;`, pageHTML)
	descriptorLabel := "FDs"
	if runtime.GOOS == "windows" {
		descriptorLabel = "Handles"
	}
	assert.Contains(t, pageHTML, "<dt>"+descriptorLabel+"</dt>")
}

func TestDashboardCapsBrowserRefreshWithoutChangingSnapshotTTL(t *testing.T) {
	largeRefresh := time.Duration(1<<63 - 1)
	m, err := newMiddleware(Config{Refresh: largeRefresh})
	require.NoError(t, err)
	assert.Equal(t, largeRefresh, m.refresh)
	assert.Regexp(t, `const REFRESH_MS =\s+2147483647\s*;`, m.index)
}

func TestDashboardContract(t *testing.T) {
	pageHTML, err := renderDashboard(ConfigDefault)
	require.NoError(t, err)
	assert.Contains(t, html.UnescapeString(pageHTML), "data:image/svg+xml;base64,")
	assert.Equal(t, 4, strings.Count(pageHTML, `class="metric-card"`))
	assert.Equal(t, 8, strings.Count(pageHTML, "<canvas "))
	assert.Contains(t, pageHTML, `const MAX_SAMPLES = 90;`)
	assert.Contains(t, pageHTML, `let currentSampleWindow = 60;`)
	assert.Contains(t, pageHTML, `Accept: "application/json"`)
	assert.Contains(t, pageHTML, `@media (prefers-color-scheme: dark)`)
	assert.Contains(t, pageHTML, `@media (max-width: 980px)`)
	assert.Contains(t, pageHTML, `@media (max-width: 560px)`)
	assert.Contains(t, pageHTML, `grid-template-columns: repeat(4, minmax(0, 1fr))`)
	assert.Contains(t, pageHTML, `grid-template-columns: repeat(2, minmax(0, 1fr))`)
	assert.Contains(t, pageHTML, `class="metric-pager"`)
	assert.Contains(t, pageHTML, `width: min(1360px, calc(100% - 32px))`)
	assert.Contains(t, pageHTML, `border: 3px solid var(--border)`)
	assert.Contains(t, pageHTML, `class="page-scroll-dock"`)
	assert.NotContains(t, pageHTML, "lang-toggle")
	assert.NotContains(t, pageHTML, `data-active="en"`)
	assert.NotContains(t, pageHTML, "container-card")
	assert.NotContains(t, strings.ToLower(pageHTML), "chart.js")
	assert.NotContains(t, strings.ToLower(pageHTML), "cdn")
}

func TestDashboardInteractions(t *testing.T) {
	pageHTML, err := renderDashboard(ConfigDefault)
	require.NoError(t, err)

	for _, sample := range []string{`data-samples="30"`, `data-samples="60"`, `data-samples="90"`} {
		assert.Contains(t, pageHTML, sample)
	}
	assert.Equal(t, 3, strings.Count(pageHTML, `class="sample-option"`))
	assert.Contains(t, pageHTML, `storageSet("monitor.samples"`)
	assert.Contains(t, pageHTML, `visibleSamples(values)`)

	assert.Contains(t, pageHTML, `id="theme-toggle"`)
	assert.Contains(t, pageHTML, `prefers-color-scheme: dark`)
	assert.Contains(t, pageHTML, `storageSet("monitor.theme"`)
	assert.Contains(t, pageHTML, `currentTheme === "dark" ? "light" : "dark"`)
	assert.NotContains(t, pageHTML, `.control-dot:hover`)
	assert.Contains(t, pageHTML, `transition: none`)
	assert.Contains(t, pageHTML, `data-series="bad"></span>P99`)
	assert.Contains(t, pageHTML, `fillForward(history.p95)`)
	assert.Contains(t, pageHTML, `dash: [6, 4]`)
	assert.Contains(t, pageHTML, `fillZero(error4xx)`)
	assert.Contains(t, pageHTML, `rawData: error4xx`)
	assert.Contains(t, pageHTML, `No requests in this window`)
	assert.Contains(t, pageHTML, `Unsupported on this platform`)
	assert.Contains(t, pageHTML, `collectionErrors.includes("system.load")`)
	assert.Contains(t, pageHTML, `runtime.gc_pause_metrics_enabled === true`)
	assert.Contains(t, pageHTML, `: "Disabled"`)
	assert.Contains(t, pageHTML, `"GC " + optional(currentRuntime.gc_count`)

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
		assert.Contains(t, pageHTML, id)
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
		assert.Contains(t, pageHTML, label)
	}
	assert.Contains(t, pageHTML, `event.key === "Escape"`)
	assert.Contains(t, pageHTML, `event.target === modal`)
	assert.Contains(t, pageHTML, `byId(binding.trigger).focus()`)
	assert.Contains(t, pageHTML, `id="http-error-chart"`)
	assert.Contains(t, pageHTML, `id="gc-pause-chart"`)
}

func TestDashboardPollingPreservesQueryString(t *testing.T) {
	pageHTML, err := renderDashboard(ConfigDefault)
	require.NoError(t, err)
	assert.Contains(t, pageHTML, `fetch(window.location.pathname + window.location.search, {`)
	assert.NotContains(t, pageHTML, `fetch(window.location.pathname, {`)
	assert.NotContains(t, pageHTML, `window.location.hash`)
}

func TestRenderDashboardSanitizesUnvalidatedFavicon(t *testing.T) {
	pageHTML, err := renderDashboard(Config{FaviconURL: "javascript:alert(1)", Refresh: time.Second})
	require.NoError(t, err)
	assert.Contains(t, pageHTML, `<link rel="icon" href="#ZgotmplZ">`)
}
