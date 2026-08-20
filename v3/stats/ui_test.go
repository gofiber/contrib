package stats

import (
	"html"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRenderDashboard(t *testing.T) {
	pageHTML, err := renderDashboard(Config{
		Title:       `Stats <script>alert("x")</script>`,
		Description: `Description <img src=x>`,
		Footer:      `Footer & details`,
		FaviconURL:  "/assets/favicon.svg",
		Refresh:     3 * time.Second,
	})
	require.NoError(t, err)
	require.Contains(t, pageHTML, `Stats &lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;`)
	require.NotContains(t, pageHTML, `<script>alert("x")</script>`)
	require.Contains(t, pageHTML, `Description &lt;img src=x&gt;`)
	require.Contains(t, pageHTML, `Footer &amp; details`)
	require.Contains(t, pageHTML, `href="/assets/favicon.svg"`)
	require.Regexp(t, `const REFRESH_MS =\s+3000\s*;`, pageHTML)
	descriptorLabel := "FDs"
	if runtime.GOOS == "windows" {
		descriptorLabel = "Handles"
	}
	require.Contains(t, pageHTML, "<dt>"+descriptorLabel+"</dt>")
}

func TestDashboardContract(t *testing.T) {
	pageHTML, err := renderDashboard(ConfigDefault)
	require.NoError(t, err)
	require.Contains(t, html.UnescapeString(pageHTML), "data:image/svg+xml;base64,")
	require.Equal(t, 4, strings.Count(pageHTML, `class="metric-card"`))
	require.Equal(t, 6, strings.Count(pageHTML, "<canvas "))
	require.Contains(t, pageHTML, `const MAX_SAMPLES = 60;`)
	require.Contains(t, pageHTML, `Accept: "application/json"`)
	require.Contains(t, pageHTML, `@media (prefers-color-scheme: dark)`)
	require.Contains(t, pageHTML, `@media (max-width: 1100px)`)
	require.Contains(t, pageHTML, `@media (max-width: 640px)`)
	require.Contains(t, pageHTML, `grid-template-columns: repeat(4, minmax(0, 1fr))`)
	require.Contains(t, pageHTML, `grid-template-columns: repeat(2, minmax(0, 1fr))`)
	require.Contains(t, pageHTML, `class="metric-pager"`)
	require.NotContains(t, pageHTML, "lang-toggle")
	require.NotContains(t, pageHTML, "theme-toggle")
	require.NotContains(t, pageHTML, "sample-option")
	require.NotContains(t, strings.ToLower(pageHTML), "chart.js")
	require.NotContains(t, strings.ToLower(pageHTML), "cdn")
}

func TestRenderDashboardSanitizesUnvalidatedFavicon(t *testing.T) {
	pageHTML, err := renderDashboard(Config{FaviconURL: "javascript:alert(1)", Refresh: time.Second})
	require.NoError(t, err)
	require.Contains(t, pageHTML, `<link rel="icon" href="#ZgotmplZ">`)
}
