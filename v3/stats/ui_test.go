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
	mustEqual(t, 6, strings.Count(pageHTML, "<canvas "))
	mustContain(t, pageHTML, `const MAX_SAMPLES = 60;`)
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
	mustNotContain(t, pageHTML, "theme-toggle")
	mustNotContain(t, pageHTML, "sample-option")
	mustNotContain(t, pageHTML, "container-card")
	mustNotContain(t, strings.ToLower(pageHTML), "chart.js")
	mustNotContain(t, strings.ToLower(pageHTML), "cdn")
}

func TestRenderDashboardSanitizesUnvalidatedFavicon(t *testing.T) {
	pageHTML, err := renderDashboard(Config{FaviconURL: "javascript:alert(1)", Refresh: time.Second})
	mustNoError(t, err)
	mustContain(t, pageHTML, `<link rel="icon" href="#ZgotmplZ">`)
}
