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
