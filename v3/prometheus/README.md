---
id: prometheus
---

# Prometheus

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*prometheus*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20Prometheus/badge.svg)

Prometheus middleware for [Fiber](https://github.com/gofiber/fiber) that instruments incoming requests and serves the metrics endpoint, based on [ansrivas/fiberprometheus](https://github.com/ansrivas/fiberprometheus).

**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/prometheus
```

## Signature

```go
prometheus.New(config ...prometheus.Config) fiber.Handler
```

## Config

| Property | Type | Description | Default |
|:---------|:-----|:------------|:--------|
| Service | `string` | Added as the `service` const label on every metric. Omitted when empty. | `""` |
| Namespace | `string` | Prefixes every metric name. | `"http"` |
| Subsystem | `string` | Prefixes every metric name after `Namespace`. | `""` |
| MetricsPath | `string` | Path served with the Prometheus exposition format. Unless `Next` returns true, requests to it are answered by the middleware and are not instrumented. | `"/metrics"` |
| Labels | `prometheus.Labels` | Extra const labels attached to every metric. | `nil` |
| Registerer | `prometheus.Registerer` | Registry used to register the metrics. | private registry |
| Gatherer | `prometheus.Gatherer` | Source the metrics endpoint gathers from. | private registry |
| DisableGoCollector | `bool` | Skips registration of the Go runtime metrics collector. | `false` |
| DisableProcessCollector | `bool` | Skips registration of the process metrics collector. | `false` |
| RequestDurationBuckets | `[]float64` | Histogram buckets for request latency, in seconds. | see [Default Config](#default-config) |
| RequestSizeBuckets | `[]float64` | Histogram buckets for request payload size, in bytes. | see [Default Config](#default-config) |
| ResponseSizeBuckets | `[]float64` | Histogram buckets for response payload size, in bytes. | see [Default Config](#default-config) |
| TrackUnmatchedRequests | `bool` | Records metrics for requests that do not resolve to a registered route. | `false` |
| UnmatchedRouteLabel | `string` | Path label used for unmatched requests when `TrackUnmatchedRequests` is enabled. | `"/__unmatched__"` |
| EnableOpenMetrics | `bool` | Negotiates the experimental OpenMetrics encoding, which is what exports exemplars. | `false` |
| EnableOpenMetricsTextCreatedSamples | `bool` | Adds synthetic `_created` samples to OpenMetrics responses. | `false` |
| DisableCompression | `bool` | Serves metrics uncompressed even when the client requests gzip or zstd. | `false` |
| SkipURIs | `[]string` | Route patterns excluded from instrumentation, e.g. `/user/:id`. | `nil` |
| IgnoreStatusCodes | `[]int` | Response status codes excluded from metrics. | `nil` |
| Next | `func(fiber.Ctx) bool` | Skips the middleware when it returns true, including for `MetricsPath`. | `nil` |

## Default Config

```go
var ConfigDefault = Config{
    Namespace:              "http",
    MetricsPath:            "/metrics",
    UnmatchedRouteLabel:    "/__unmatched__",
    RequestDurationBuckets: []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 10, 15, 30, 60},
    RequestSizeBuckets:     []float64{256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 5242880},
    ResponseSizeBuckets:    []float64{256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 5242880},
}
```

## Example

```go
package main

import (
    fiberprometheus "github.com/gofiber/contrib/v3/prometheus"
    "github.com/gofiber/fiber/v3"
)

func main() {
    app := fiber.New()

    // Mount the middleware globally so it observes every request. It answers
    // scrapes on Config.MetricsPath ("/metrics" by default) itself and passes
    // everything else through to your handlers.
    app.Use(fiberprometheus.New(fiberprometheus.Config{
        Service:           "my-service-name",
        SkipURIs:          []string{"/ping"},
        IgnoreStatusCodes: []int{401, 403, 404},
    }))

    app.Get("/", func(c fiber.Ctx) error {
        return c.SendString("Hello World")
    })

    app.Get("/ping", func(c fiber.Ctx) error {
        return c.SendString("pong")
    })

    app.Post("/some", func(c fiber.Ctx) error {
        return c.SendString("Welcome!")
    })

    app.Listen(":3000")
}
```

Register the middleware before the routes you want instrumented, and mount it
only once — mounting the same handler twice double-counts every request that
reaches both instances. Scrapes are the exception: the first invocation answers
them without calling `ctx.Next()`, so they never reach the second.

## Metrics

The following metrics are exposed, prefixed with `Namespace` (`http` by
default) and `Subsystem`:

```text
http_requests_total
http_requests_status_class_total
http_request_duration_seconds
http_requests_in_progress
http_request_size_bytes
http_response_size_bytes
```

Every metric except `http_requests_in_progress` is labeled with the *registered
route pattern* (for example `/user/:id`), not the request path, so label
cardinality stays bounded no matter what clients request.

`http_requests_in_progress` is labeled by HTTP method only. It has to be
incremented before the router picks a handler, at which point the route pattern
is not known yet.

Requests that miss every registered route are not recorded unless
`TrackUnmatchedRequests` is enabled, in which case they are labeled with
`UnmatchedRouteLabel`.

## Error handling

Because Fiber runs the application error handler only after the whole handler
chain has unwound, the middleware invokes it itself when a downstream handler
returns an error. This is what Fiber's own logger middleware does, and it is
what allows the recorded status code and response size to match what the client
actually received. The error is therefore consumed by this middleware and does
not propagate to handlers mounted *before* it.

## Registry and collectors

By default the middleware creates a private `Registerer`/`Gatherer` pair and
uses it for both registration and scraping, and registers the Go runtime and
process collectors into it.

When customizing the registry, ensure that `Registerer` and `Gatherer` refer to
the same metrics source (for example, a `*prometheus.Registry`). Supplying only
one that does not implement the other interface, or providing a mismatched
pair, panics during initialization so metrics are not silently dropped.

```go
registry := prometheus.NewRegistry()

app.Use(fiberprometheus.New(fiberprometheus.Config{
    Service:    "my-service-name",
    Registerer: registry,
    Gatherer:   registry,
}))
```

## Exemplars

When the request context carries a valid OpenTelemetry trace, the duration and
size histograms record the trace ID as an exemplar under the `traceID` label.
Exemplars are only serialized in the OpenMetrics encoding, so set
`EnableOpenMetrics: true` and scrape with an OpenMetrics-capable client to see
them.

## 📊 Result

- Hit the default url at <http://localhost:3000>
- Navigate to <http://localhost:3000/metrics>

## 📈 Grafana Dashboard

- <https://grafana.com/grafana/dashboards/14331>
