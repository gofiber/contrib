package uptime

import (
	"bytes"
	"encoding/json"
	"html/template"
	"strconv"
	"time"
)

type dashboardPage struct {
	Title       string
	Description string
	Footer      string
	APIPathJSON template.JS
	RefreshMS   int64
	Status      StatusResponse
	StatusJSON  template.JS
}

var dashboardTemplate = template.Must(template.New("uptime").Funcs(template.FuncMap{
	"formatTime":     formatTime,
	"formatRate":     formatRate,
	"formatDowntime": formatDowntime,
	"statusClass":    statusClass,
	"dayTitle":       dayTitle,
}).Parse(dashboardHTML))

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
		RefreshMS:   maxInt64(int64(config.SampleInterval/time.Millisecond), 3000),
		Status:      status,
		StatusJSON:  template.JS(statusJSON),
	}
	var buf bytes.Buffer
	if err := dashboardTemplate.Execute(&buf, page); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

func formatRate(rate float64) string {
	return strconv.FormatFloat(rate*100, 'f', 2, 64) + "%"
}

func formatDowntime(seconds int64) string {
	if seconds <= 0 {
		return "0s"
	}
	d := time.Duration(seconds) * time.Second
	hours := int64(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	minutes := int64(d / time.Minute)
	d -= time.Duration(minutes) * time.Minute
	secs := int64(d / time.Second)
	if hours > 0 {
		return strconv.FormatInt(hours, 10) + "h " + strconv.FormatInt(minutes, 10) + "m"
	}
	if minutes > 0 {
		return strconv.FormatInt(minutes, 10) + "m " + strconv.FormatInt(secs, 10) + "s"
	}
	return strconv.FormatInt(secs, 10) + "s"
}

func statusClass(status string) string {
	switch status {
	case "green", "up":
		return "ok"
	case "yellow":
		return "warn"
	case "red", "down":
		return "down"
	default:
		return "none"
	}
}

func dayTitle(day DayStatus) string {
	if !day.HasData {
		return day.Day + ": no data"
	}
	return day.Day + ": " + formatRate(day.UptimeRate) + " uptime, " + formatDowntime(day.EstimatedDowntimeSeconds) + " estimated downtime"
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<title>{{ .Title }}</title>
	<style>
		:root {
			color: #172033;
			background: #f7f9fc;
			font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
		}
		* { box-sizing: border-box; }
		body { margin: 0; min-width: 320px; }
		main {
			width: min(1120px, calc(100% - 32px));
			margin: 0 auto;
			padding: 32px 0;
		}
		header {
			display: flex;
			justify-content: space-between;
			gap: 24px;
			align-items: flex-start;
			margin-bottom: 24px;
		}
		h1 { margin: 0 0 8px; font-size: 32px; line-height: 1.1; font-weight: 750; }
		p { margin: 0; color: #5f6b7a; }
		.meta { text-align: right; font-size: 14px; color: #5f6b7a; }
		.meta strong { color: #172033; }
		.service-list { display: grid; gap: 12px; }
		.service {
			background: #ffffff;
			border: 1px solid #d9e0ea;
			border-radius: 8px;
			padding: 16px;
			box-shadow: 0 8px 24px rgba(31, 45, 61, 0.06);
		}
		.service-head {
			display: grid;
			grid-template-columns: minmax(0, 1fr) auto;
			gap: 16px;
			align-items: start;
			margin-bottom: 14px;
		}
		h2 { margin: 0; font-size: 18px; line-height: 1.25; }
		.service-id { margin-top: 2px; font: 12px ui-monospace, SFMono-Regular, Consolas, monospace; color: #748094; }
		.badge {
			display: inline-flex;
			align-items: center;
			min-width: 72px;
			height: 28px;
			justify-content: center;
			border-radius: 999px;
			font-size: 13px;
			font-weight: 700;
			text-transform: uppercase;
		}
		.badge.ok { color: #0f6b3f; background: #dff7ea; }
		.badge.warn { color: #8a5a00; background: #fff1c7; }
		.badge.down { color: #a42525; background: #ffe0e0; }
		.badge.none { color: #596579; background: #ecf0f5; }
		.stats {
			display: grid;
			grid-template-columns: repeat(3, minmax(0, 1fr));
			gap: 10px;
			margin-bottom: 14px;
		}
		.stat {
			border: 1px solid #e5eaf1;
			border-radius: 6px;
			padding: 10px;
			min-width: 0;
		}
		.stat span { display: block; color: #748094; font-size: 12px; margin-bottom: 4px; }
		.stat strong { font-size: 14px; overflow-wrap: anywhere; }
		.days {
			display: grid;
			grid-template-columns: repeat(auto-fit, minmax(8px, 1fr));
			gap: 3px;
			height: 34px;
			align-items: stretch;
		}
		.day {
			border-radius: 3px;
			background: #dce3ed;
		}
		.day.ok { background: #24a267; }
		.day.warn { background: #f3ba2f; }
		.day.down { background: #d94d4d; }
		.day.none { background: #dce3ed; }
		.empty {
			background: #ffffff;
			border: 1px dashed #cbd5e1;
			border-radius: 8px;
			padding: 24px;
			color: #5f6b7a;
		}
		footer { margin-top: 24px; color: #748094; font-size: 13px; }
		@media (max-width: 720px) {
			main { width: min(100% - 24px, 1120px); padding: 20px 0; }
			header { display: block; }
			.meta { text-align: left; margin-top: 12px; }
			.service-head { grid-template-columns: 1fr; }
			.stats { grid-template-columns: 1fr; }
		}
	</style>
</head>
<body>
	<main>
		<header>
			<div>
				<h1>{{ .Title }}</h1>
				<p>{{ .Description }}</p>
			</div>
			<div class="meta">
				<div>Generated <strong id="generated-at">{{ formatTime .Status.GeneratedAt }}</strong></div>
				<div>Storage <strong id="storage-status">{{ .Status.Storage.Driver }} / {{ .Status.Storage.Status }}</strong></div>
			</div>
		</header>
		<section class="service-list" id="services">
			{{ if .Status.Services }}
			{{ range .Status.Services }}
			<article class="service" data-service-id="{{ .ID }}">
				<div class="service-head">
					<div>
						<h2>{{ .Name }}</h2>
						<div class="service-id">{{ .ID }}</div>
						{{ if .Description }}<p>{{ .Description }}</p>{{ end }}
					</div>
					<span class="badge {{ statusClass .CurrentStatus }}" data-role="status">{{ .CurrentStatus }}</span>
				</div>
				<div class="stats">
					<div class="stat"><span>Last seen</span><strong data-role="last-seen">{{ formatTime .LastSeenAt }}</strong></div>
					<div class="stat"><span>Sample interval</span><strong>{{ .SampleIntervalSeconds }}s</strong></div>
					<div class="stat"><span>Window</span><strong>{{ len .Daily }} days</strong></div>
				</div>
				<div class="days" aria-label="Daily uptime history">
					{{ range .Daily }}
					<div class="day {{ statusClass .Status }}" title="{{ dayTitle . }}" aria-label="{{ dayTitle . }}"></div>
					{{ end }}
				</div>
			</article>
			{{ end }}
			{{ else }}
			<div class="empty">No services have reported uptime yet.</div>
			{{ end }}
		</section>
		<footer>{{ .Footer }}</footer>
	</main>
	<script>
		const initialStatus = {{ .StatusJSON }};
		const apiPath = {{ .APIPathJSON }};
		const refreshMS = {{ .RefreshMS }};
		function formatTime(value) {
			if (!value || value === "0001-01-01T00:00:00Z") return "never";
			const date = new Date(value);
			if (Number.isNaN(date.getTime())) return "never";
			return date.toISOString().replace("T", " ").replace(/\.\d{3}Z$/, " UTC").replace("Z", " UTC");
		}
		function statusClass(status) {
			if (status === "up" || status === "green") return "ok";
			if (status === "yellow") return "warn";
			if (status === "down" || status === "red") return "down";
			return "none";
		}
		function update(data) {
			document.getElementById("generated-at").textContent = formatTime(data.generated_at);
			document.getElementById("storage-status").textContent = data.storage.driver + " / " + data.storage.status;
			for (const service of data.services || []) {
				for (const row of document.querySelectorAll(".service")) {
					if (row.dataset.serviceId !== service.id) continue;
					const badge = row.querySelector('[data-role="status"]');
					badge.textContent = service.current_status;
					badge.className = "badge " + statusClass(service.current_status);
					row.querySelector('[data-role="last-seen"]').textContent = formatTime(service.last_seen_at);
				}
			}
		}
		async function refresh() {
			try {
				const response = await fetch(apiPath, { headers: { Accept: "application/json" }, credentials: "same-origin" });
				if (!response.ok) return;
				update(await response.json());
			} catch (_) {}
		}
		update(initialStatus);
		setInterval(refresh, refreshMS);
	</script>
</body>
</html>`
