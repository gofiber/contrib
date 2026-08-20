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
| Go runtime | Goroutines, heap allocation, heap objects, GC count, last GC pause |
| System | CPU, memory, current-filesystem usage, load average, aggregate network rates |
| HTTP | Requests, in-flight requests, status classes, RPS, 4xx/5xx rates, P50/P95/P99 latency |

CPU, network, request-rate, status-rate, and latency values need two collection
windows. Their first snapshot is `null`. Windows does not expose Load 1 because
the underlying gopsutil implementation starts a background sampler; this is an
unsupported metric and does not make the snapshot partial.

Snapshots are collected only when JSON is requested and are shared within the
configured refresh TTL. The dashboard keeps at most 60 trend samples in browser
memory; reloading the page resets that history.

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
- The embedded dashboard has no external JavaScript, CSS, font, or chart dependency and automatically follows the browser color scheme.
- Linux, macOS, and Windows are supported with graceful degradation when a metric is unavailable.
