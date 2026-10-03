package uptime

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDashboardInsightsTemplate(t *testing.T) {
	t.Parallel()

	const unsafeText = `</script><img src=x onerror="alert(1)">`
	status := StatusResponse{Services: []ServiceStatus{{
		ID: unsafeText, Name: unsafeText, Description: unsafeText,
		Daily:   []DayStatus{{Day: "2026-09-09", HasData: true, UpSlots: 1, ExpectedSlots: 1, UptimeRate: 1}},
		Summary: ServiceSummary{HasData: true, AvailabilityRate: 1, StableStreakDays: 1, StableStreakCapped: true},
	}}}
	body, err := renderDashboardHTML(ConfigDefault, status, "/uptime/api/status")
	requireNoError(t, err)
	requireNotContains(t, body, unsafeText)
	requireEqual(t, 1, strings.Count(body, "<script>"))
	requireEqual(t, 1, strings.Count(body, "</script>"))

	// Verify that safe embedding preserves the full API data, including summary.
	_, initial, ok := strings.Cut(body, "const initialStatus = ")
	if !ok {
		t.Fatal("missing initial status JSON")
	}
	var decoded StatusResponse
	requireNoError(t, json.NewDecoder(strings.NewReader(initial)).Decode(&decoded))
	requireLen(t, decoded.Services, 1)
	requireEqual(t, unsafeText, decoded.Services[0].Name)
	requireEqual(t, status.Services[0].Summary, decoded.Services[0].Summary)
	requireLen(t, decoded.Services[0].Daily, 1)
	requireEqual(t, status.Services[0].Daily[0], decoded.Services[0].Daily[0])

	for _, contract := range []string{
		`button.textContent = "Insights"`,
		`"aria-expanded"`, `"aria-controls"`,
		`.service-insights[hidden] { display: none; }`,
		`id="trend-hovercard"`, `id="uptime-hovercard"`,
		`"aria-hidden": "true", focusable: "false"`,
		`document.createElementNS("http://www.w3.org/2000/svg"`,
	} {
		requireContains(t, body, contract)
	}
	for _, unsafe := range []string{
		"innerHTML", "insertAdjacentHTML", "eval(", "new Function", "foreignObject",
		"<script src=", "@import", "localStorage", "sessionStorage",
	} {
		requireNotContains(t, body, unsafe)
	}
}

func TestDashboardTrendHoverContract(t *testing.T) {
	t.Parallel()

	body, err := renderDashboardHTML(ConfigDefault, StatusResponse{}, "/uptime/api/status")
	requireNoError(t, err)

	for _, contract := range []string{
		`const CHART_POINT_PROXIMITY = 16;`,
		`const CHART_TOOLTIP_OFFSET = 12;`,
		`const CHART_TOOLTIP_MARGIN = 12;`,
		`function nearestTrendIndex(`,
		`function trendCursorX(`,
		`function trendPointFocused(`,
		`Math.abs(pointClientX - clientX) <= CHART_POINT_PROXIMITY`,
		`Math.abs(pointClientY - clientY) <= CHART_POINT_PROXIMITY`,
		`class: "trend-focus-guide"`,
		`guide.setAttribute("x1", cursorX)`,
		`guide.setAttribute("x2", cursorX)`,
		`validTrendDay(day) && Number.isFinite(day.uptime_rate)`,
		`focusGuide.setAttribute("visibility", focused ? "visible" : "hidden")`,
		`point.setAttribute("visibility", focused ? "visible" : "hidden")`,
		`trendTooltipOwner !== owner || trendTooltipIndex !== index`,
		`positionTrendHoverCard(owner.pointerClientX, owner.pointerClientY, owner.pointerTouch)`,
		`const margin = CHART_TOOLTIP_MARGIN`,
		`const offset = CHART_TOOLTIP_OFFSET`,
		`window.innerWidth - width - margin`,
		`window.innerHeight - height - margin`,
		`hit.addEventListener("pointermove"`,
		`hit.addEventListener("pointerleave"`,
		`hit.addEventListener("pointercancel"`,
		`window.requestAnimationFrame(paintHover)`,
		`window.cancelAnimationFrame(activeTrend.hoverFrame)`,
		`activeTrend.focusGuide.setAttribute("visibility", "hidden")`,
	} {
		requireContains(t, body, contract)
	}
	for _, obsolete := range []string{
		`activeTrend.index === index`,
		`guide.setAttribute("x1", x(index))`,
		`guide.setAttribute("x2", x(index))`,
		`anchor.x`, `anchor.y`,
	} {
		requireNotContains(t, body, obsolete)
	}
}

func TestDashboardPollingLifecycle(t *testing.T) {
	t.Parallel()

	body, err := renderDashboardHTML(ConfigDefault, StatusResponse{}, "/uptime/api/status")
	requireNoError(t, err)

	for _, contract := range []string{
		`const pollMS = Math.max(Number(refreshMS) || 10000, 10000);`,
		`const controller = new AbortController();`,
		`window.setTimeout(() => controller.abort(), pollMS)`,
		`signal: controller.signal`,
		`if (refreshInFlight) return;`,
		`refreshTimer = document.hidden ? 0 : window.setTimeout(refresh, Math.max(0, delay));`,
		`document.addEventListener("visibilitychange", handleVisibilityChange);`,
		`scheduleRefresh(lastRefreshStarted + pollMS - performance.now());`,
		`scheduleRefresh(pollMS);`,
		`scheduleRefresh(0);`,
		// Neither a hidden page nor one refreshing as it becomes visible is stale.
		`if (document.hidden || refreshInFlight || !lastSuccessAt || currentStatus === "error") return;`,
	} {
		requireContains(t, body, contract)
	}
	// Hidden pages must not keep polling, so no fixed-rate timer may drive refresh.
	requireNotContains(t, body, `setInterval(refresh`)
}

func TestDashboardAccessibilityContract(t *testing.T) {
	t.Parallel()

	body, err := renderDashboardHTML(ConfigDefault, StatusResponse{}, "/uptime/api/status")
	requireNoError(t, err)

	requireContains(t, body, `<html lang="en">`)

	// Only connection state changes are announced; timestamps change on every refresh.
	requireEqual(t, 1, strings.Count(body, `role="status"`))
	requireContains(t, body, `id="live-status" class="status-line" role="status"`)
	requireNotContains(t, body, `aria-live`)
	requireContains(t, body, `if (text.textContent !== label) text.textContent = label;`)

	// The light accent and hovercard labels keep text above WCAG AA contrast.
	requireContains(t, body, `--accent: #0e7490;`)
	requireContains(t, body, `--hovercard-label: #52657f;`)

	// aria-label is only valid on elements whose role allows naming.
	requireContains(t, body, `class="summary-metrics" role="group" aria-label="Service summary"`)
	requireContains(t, body, `dot.setAttribute("role", "img");`)
	requireContains(t, body, `bars.setAttribute("role", "group");`)

	// The trend exposes its latest and lowest values; its bare date labels are decorative.
	requireContains(t, body, `plot.setAttribute("role", "img");`)
	requireContains(t, body, `plot.setAttribute("aria-label", trendSummary(days));`)
	requireContains(t, body, `dates.setAttribute("aria-hidden", "true");`)
	requireContains(t, body, `function trendSummary(`)

	requireContains(t, body, `aria-label="Scroll up; scroll progress 0%"`)
	requireNotContains(t, body, `aria-value`)
	requireContains(t, body, `window.matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth"`)
}

func TestDashboardTrendTouchContract(t *testing.T) {
	t.Parallel()

	body, err := renderDashboardHTML(ConfigDefault, StatusResponse{}, "/uptime/api/status")
	requireNoError(t, err)

	// Touch pointers cannot hover, so a tap or horizontal drag shows the day's values.
	for _, contract := range []string{
		`touch-action: pan-y pinch-zoom`,
		`hit.addEventListener("pointerdown"`,
		`if (event.pointerType !== "mouse") trackPointer(event);`,
		`trend.pointerTouch = event.pointerType === "touch";`,
		`if (event.pointerType !== "touch") clearHover();`,
		`if (activeTrend && event.target !== activeTrend.hit) hideTrendHoverCard();`,
		`let top = above ? clientY - height - offset * 2 : clientY + offset;`,
	} {
		requireContains(t, body, contract)
	}
}

func TestDashboardTrendAxisContract(t *testing.T) {
	t.Parallel()

	body, err := renderDashboardHTML(ConfigDefault, StatusResponse{}, "/uptime/api/status")
	requireNoError(t, err)

	// Every domain splits into four steps that read as round percentages.
	requireContains(t, body, `const TREND_FLOORS = [0.99, 0.98, 0.96, 0.92, 0.9, 0.8, 0.6, 0.2, 0];`)
	requireContains(t, body, `return minimum - floor >= (1 - floor) * 0.04;`)
	// The old one-point-below-the-minimum domain produced ticks such as 87.25%.
	requireNotContains(t, body, `Math.floor(minimum * 100) - 1`)
}
