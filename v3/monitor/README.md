---
id: monitor
---

# Monitor

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*monitor*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20Monitor/badge.svg)

Monitor is a Fiber middleware that exposes real-time process, Go runtime,
system, and HTTP service metrics through a built-in dashboard and JSON
snapshot.

The v3 monitor dashboard has been redesigned with an embedded,
dependency-free UI and richer runtime and system metrics. Existing
`monitor.New(config...)` usage remains source compatible.

Monitor does not persist metrics, run a background collection loop, or replace
Prometheus, OpenTelemetry, or an APM.

**Compatible with Fiber v3.**

## Preview

![Monitor dashboard preview](https://raw.githubusercontent.com/gofurry/images/refs/heads/main/github/stats/dark.png)

## Install

```sh
go get github.com/gofiber/contrib/v3/monitor
```

## Signature

```go
monitor.New(config ...monitor.Config) fiber.Handler
```

## Usage

The existing route-mounted form continues to serve HTML and JSON at whichever
path you choose:

```go
app.Get("/metrics", monitor.New())
```

Request the same endpoint with `Accept: application/json` to receive the raw
snapshot. A route-only mount does not see application traffic, so its HTTP
request metrics remain empty.

To collect HTTP metrics for the whole application, mount one monitor instance
app-wide and use `Next` to pass non-monitor requests downstream:

```go
package main

import (
    "github.com/gofiber/contrib/v3/monitor"
    "github.com/gofiber/fiber/v3"
    "github.com/gofiber/fiber/v3/log"
)

func main() {
    app := fiber.New()

    app.Use(monitor.New(monitor.Config{
        Next: func(c fiber.Ctx) bool {
            return c.Path() != "/metrics"
        },
    }))

    app.Get("/", func(c fiber.Ctx) error {
        return c.SendString("Hello, World!")
    })

    log.Fatal(app.Listen(":3000"))
}
```

Open `http://localhost:3000/metrics`. Requests for the monitor endpoint itself
are not included in the HTTP metrics.

For production deployments, protect the endpoint with authentication, network
restrictions, or a reverse proxy. Place endpoint-specific authentication before
monitor:

```go
app.Use("/metrics", authMiddleware)
app.Use(monitor.New(monitor.Config{
    Next: func(c fiber.Ctx) bool { return c.Path() != "/metrics" },
}))
```

When the application uses Fiber's recover middleware, place it after monitor so
recovered panics are observed as `5xx` responses:

```go
app.Use(
    monitor.New(monitor.Config{
        Next: func(c fiber.Ctx) bool {
            return c.Path() != "/metrics"
        },
    }),
    recover.New(),
)
```

For instrumented requests that return an error, monitor invokes the
application's configured Fiber `ErrorHandler` before recording the response.
This makes status metrics match the response sent to the client, including
custom error statuses. The error is therefore consumed at the monitor layer and
does not propagate to middleware mounted before it.

## Config

```go
app.Use(monitor.New(monitor.Config{
    Next: func(c fiber.Ctx) bool { return c.Path() != "/metrics" },
    Title:       "My Service",
    Description: "Live runtime and HTTP statistics.",
    Footer:      "Internal operations dashboard.",
    FaviconURL:  "/assets/favicon.svg",
    Refresh:     3 * time.Second,
}))
```

| Field                  | Type                   | Default             | Description |
| ---------------------- | ---------------------- | ------------------- | ----------- |
| `Next`                 | `func(fiber.Ctx) bool` | `nil`               | Passes matching requests downstream and includes them in HTTP metrics. A false result serves monitor on the current path. |
| `Title`                | `string`               | `Fiber Monitor`     | Document title and dashboard heading. |
| `Description`          | `string`               | See `ConfigDefault` | Text displayed below the heading. |
| `Footer`               | `string`               | See `ConfigDefault` | Text displayed at the bottom of the page. |
| `FaviconURL`           | `string`               | Built-in SVG        | Root-relative path or absolute HTTP(S) URL. |
| `Refresh`              | `time.Duration`        | `3s`                | Browser polling interval and server snapshot cache TTL; values below `1s` are clamped. |
| `APIOnly`              | `bool`                 | `false`             | Returns JSON regardless of the request `Accept` header. |
| `EnableGCPauseMetrics` | `bool`                 | `false`             | Enables exact last/window/total GC pause metrics through `runtime.ReadMemStats`, which briefly stops the Go runtime. |
| `CustomHead`           | `string`               | Deprecated          | Retained for source compatibility and ignored by the embedded dashboard. |
| `FontURL`              | `string`               | Deprecated          | Retained for source compatibility; no external font is loaded. |
| `ChartJSURL`           | `string`               | Deprecated          | Retained for source compatibility; charts use the built-in Canvas implementation. |

Default runtime metrics use `runtime/metrics` and avoid the explicit
stop-the-world snapshot performed by `runtime.ReadMemStats`. Applications that
need exact GC pause totals and the GC Pause trend can opt in:

```go
app.Use(monitor.New(monitor.Config{
    Next:                 func(c fiber.Ctx) bool { return c.Path() != "/metrics" },
    EnableGCPauseMetrics: true,
}))
```

## JSON

Request the monitor endpoint with an explicit JSON accept header:

```sh
curl -H "Accept: application/json" http://localhost:3000/metrics
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

### Migration notes

Existing `monitor.New(config...)` usage remains source compatible.

The JSON snapshot has been redesigned from the legacy `pid` / `os` payload
to the new `process` / `runtime` / `system` / `http` structure.

Route-mounted monitor instances still serve the dashboard and JSON snapshot,
but application HTTP metrics require the app-wide `Next` setup shown above.
Monitor endpoint requests are no longer counted as application traffic.

## Metrics

| Group      | Metrics |
| ---------- | ------- |
| Process    | CPU, RSS, threads, file descriptors/handles, runtime since monitor initialization |
| Go runtime | Goroutines, heap allocation/system/in-use/idle/released memory, heap objects, Next GC, mallocs/frees, GOMAXPROCS, GC count, optional last/window/total GC pause, GC CPU fraction |
| System     | CPU, used/available/total memory, application-filesystem usage/type/free space, 1/5/15-minute load averages, aggregate network rates |
| HTTP       | Requests, in-flight requests, 1xx–5xx status classes, RPS, 4xx/5xx rates, P50/P95/P99 latency |

CPU, network, request-rate, status-rate, and latency values need two collection
windows. Their first snapshot is `null`.

Exact GC pause metrics are `null` unless `EnableGCPauseMetrics` is true; when
enabled, the first window pause is also `null` while its baseline is
established.

Snapshots are collected only when JSON is requested and are shared within the
configured refresh TTL. The dashboard keeps at most 90 trend samples in browser
memory and displays 60 by default. The 30/60/90 selector changes only the
visible browser history and remembers the choice in local storage.

The eight trend panels cover CPU, memory, network, goroutines, requests per
second, HTTP latency, HTTP error rates, and GC pauses. Heap, GC,
application-filesystem, and HTTP status-code buttons open detailed views without
triggering extra collection. Disk details never expose the filesystem path,
mountpoint, or device name.

## Performance

Run all benchmarks with:

```sh
go test -run='^$' -bench=. -benchmem
```

The cold-collection benchmarks include platform-specific gopsutil calls and are
expected to vary between operating systems and machines. Results are intended
for regression tracking, not as production throughput guarantees.

## Security

> This middleware exposes operational information and should be treated as a
> protected administrative endpoint.

Monitor does not implement authentication. Protect it with Fiber middleware, a
reverse proxy, a private network, VPN, or firewall.

Monitor records aggregate counters only. It does not collect request or
response bodies, headers, cookies, authorization values, query strings, route
parameters, client IPs, user identifiers, environment variables, command lines,
filesystem paths, or device names.

## Notes

- Each `New` call owns independent counters, histogram, cache, and sampling
  baselines.
- Middleware instances are safe for concurrent use after construction.
- The business-request hot path uses time reads and atomic operations; system
  and runtime collection is not performed there.
- The embedded dashboard has no external JavaScript, CSS, font, or chart
  dependency.
- It initially follows the browser color scheme and provides a persisted
  Light/Dark toggle.
- Linux, macOS, and Windows are supported with graceful degradation when a
  metric is unavailable. Load averages are intentionally unsupported on Windows
  because gopsutil's Windows implementation starts a background sampler.