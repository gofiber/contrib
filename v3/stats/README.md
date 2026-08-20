---
id: stats
---

# Stats

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*stats*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20Stats/badge.svg)

Stats is a Fiber middleware that exposes real-time process, Go runtime, system,
and HTTP service metrics through a built-in dashboard and JSON snapshot.

It is a lightweight operational dashboard: it does not persist metrics, run a
background collection loop, or replace Prometheus, OpenTelemetry, or an APM.

**Compatible with Fiber v3.**

## Preview

![Stats dashboard preview](https://raw.githubusercontent.com/gofurry/images/refs/heads/main/github/stats/dark.png)

## Install

```sh
go get github.com/gofiber/contrib/v3/stats
```

## Signature

```go
stats.New(config ...stats.Config) fiber.Handler
```

## Usage

```go
package main

import (
	"github.com/gofiber/contrib/v3/stats"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
)

func main() {
	app := fiber.New()

	app.Use(stats.New())

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("Hello, World!")
	})

	log.Fatal(app.Listen(":3000"))
}
```

Open `http://localhost:3000/stats`.

When the application uses Fiber's recover middleware, place it after stats so
recovered panics are observed as `5xx` responses:

```go
app.Use(
	stats.New(),
	recover.New(),
)
```

Stats observes returned errors but does not call or replace the application
error handler and does not recover panics itself.

## Config

```go
app.Use(stats.New(stats.Config{
	Path:        "/stats",
	Title:       "My Service",
	Description: "Live runtime and HTTP statistics.",
	Footer:      "Internal operations dashboard.",
	FaviconURL:  "/assets/favicon.svg",
	Refresh:     2 * time.Second,
}))
```

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `Next` | `func(fiber.Ctx) bool` | `nil` | Skips both dashboard serving and request observation when true. |
| `Path` | `string` | `/stats` | Dashboard and JSON snapshot endpoint. A trailing slash is also accepted. |
| `Title` | `string` | `Fiber Stats` | Document title and dashboard heading. |
| `Description` | `string` | See `ConfigDefault` | Text displayed below the heading. |
| `Footer` | `string` | See `ConfigDefault` | Text displayed at the bottom of the page. |
| `FaviconURL` | `string` | Built-in SVG | Root-relative path or absolute HTTP(S) URL. |
| `Refresh` | `time.Duration` | `2s` | Browser polling interval and server snapshot cache TTL; values below `1s` are clamped. |
| `EnableGCPauseMetrics` | `bool` | `false` | Enables exact last/window/total GC pause metrics. This calls `runtime.ReadMemStats`, which briefly stops the Go runtime. |

Default runtime metrics use `runtime/metrics` and avoid the explicit
stop-the-world snapshot performed by `runtime.ReadMemStats`. Applications that
need exact GC pause totals and the GC Pause trend can opt in:

```go
app.Use(stats.New(stats.Config{EnableGCPauseMetrics: true}))
```

The middleware can also be mounted below a prefix:

```go
app.Use("/internal", stats.New(stats.Config{Path: "/stats"}))
```

The dashboard is then available at `/internal/stats`.

## JSON

Request the same endpoint with an explicit JSON accept header:

```sh
curl -H "Accept: application/json" http://localhost:3000/stats
```

The snapshot contains these groups:

```json
{
  "collected_at": "2026-08-20T00:00:00Z",
  "collection": { "partial": false, "errors": [] },
  "process": {},
  "runtime": {},
  "system": {},
  "http": {}
}
```

Unsupported, failed, and not-yet-available window metrics are encoded as
`null`, not as a synthetic zero. Collection failures expose stable identifiers
such as `system.disk`; raw operating-system errors and paths are not returned.

## Metrics

| Group | Metrics |
| --- | --- |
| Process | CPU, RSS, threads, file descriptors/handles, runtime since stats initialization |
| Go runtime | Goroutines, heap allocation/system/in-use/idle/released memory, heap objects, Next GC, mallocs/frees, GOMAXPROCS, GC count, optional last/window/total GC pause, GC CPU fraction |
| System | CPU, used/available/total memory, application-filesystem usage/type/free space, 1/5/15-minute load averages, aggregate network rates |
| HTTP | Requests, in-flight requests, 1xx–5xx status classes, RPS, 4xx/5xx rates, P50/P95/P99 latency |

CPU, network, request-rate, status-rate, and latency values need two collection
windows. Their first snapshot is `null`. Exact GC pause metrics are `null` unless
`EnableGCPauseMetrics` is true; when enabled, the first window pause is also
`null` while its baseline is established. Windows does not
expose Load 1/5/15 because the underlying gopsutil implementation starts a
background sampler; these are unsupported metrics and do not make the snapshot
partial.

Snapshots are collected only when JSON is requested and are shared within the
configured refresh TTL. The dashboard keeps at most 90 trend samples in browser
memory and displays 60 by default. The 30/60/90 selector changes only the
visible browser history and remembers the choice in local storage; reloading
the page resets the sampled values.

The eight trend panels cover CPU, memory, network, goroutines, requests per
second, HTTP latency, HTTP error rates, and GC pauses. The GC pause panel starts
receiving samples only when `EnableGCPauseMetrics` is enabled.

Idle HTTP windows keep the charts visually continuous without changing the
JSON contract: error-rate lines render at zero, while latency lines retain the
last observed percentile and identify the idle window in the hover tooltip.

Heap, GC, application-filesystem, and HTTP status-code buttons open detailed
views without triggering extra collection. Disk details intentionally identify
the target only as the application filesystem and never expose its path,
mountpoint, or device name.

## Performance

Run all hot-path, parallel, cache, runtime, and cold-collection benchmarks with:

```sh
go test -run='^$' -bench=. -benchmem
```

To measure only the cost of the default runtime collector against the exact GC
pause opt-in on the current Go version and host, run:

```sh
go test -run='^$' -bench='^BenchmarkRuntimeCollection' -benchmem
```

The cold-collection benchmarks include platform-specific gopsutil calls and are
therefore expected to vary between operating systems and machines.

## Security

> This middleware exposes operational information and should be treated as a protected administrative endpoint.

Stats does not implement authentication. Protect it with Fiber middleware, a
reverse proxy, a private network, VPN, or firewall. For example:

```go
app.Use("/stats", authMiddleware)
app.Use(stats.New())
```

Stats records aggregate counters only. It does not collect request or response
bodies, headers, cookies, authorization values, query strings, route parameters,
client IPs, user identifiers, environment variables, command lines, filesystem
paths, or device names.

## Notes

- Each `New` call owns independent counters, histogram, cache, and sampling baselines.
- Middleware instances are safe for concurrent use after construction.
- The business-request hot path uses time reads and atomic operations; system and runtime collection is not performed there.
- Default heap and GC counters use `runtime/metrics` and do not call `runtime.ReadMemStats`. Exact GC pause metrics are an explicit opt-in because `runtime.ReadMemStats` briefly stops the Go runtime for a consistent snapshot.
- The embedded dashboard has no external JavaScript, CSS, font, or chart dependency. It initially follows the browser color scheme and provides a persisted Light/Dark toggle.
- Linux, macOS, and Windows are supported with graceful degradation when a metric is unavailable. The dashboard labels unsupported Windows load averages as `N/A`.
