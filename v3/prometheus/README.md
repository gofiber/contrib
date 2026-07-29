# Prometheus

Prometheus middleware for [Fiber v3](https://github.com/gofiber/fiber) based on [ansrivas/fiberprometheus](https://github.com/ansrivas/fiberprometheus).

![Release](https://img.shields.io/github/release/ansrivas/fiberprometheus.svg)
[![Discord](https://img.shields.io/badge/discord-join%20channel-7289DA)](https://gofiber.io/discord)

Following metrics are available by default:

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

> [!NOTE]
> The middleware requires Go 1.25 or newer and Fiber v3 (currently RC).

## 🚀 Installation

```bash
go get github.com/gofiber/contrib/v3/prometheus
```

## 📄 Example

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
only once — mounting the same handler twice double-counts every request.

Because Fiber runs the application error handler only after the whole handler
chain has unwound, the middleware invokes it itself when a downstream handler
returns an error. This is what Fiber's own logger middleware does, and it is
what allows the recorded status code and response size to match what the client
actually received. The error is therefore consumed by this middleware and does
not propagate to handlers mounted *before* it.

### Collector, OpenMetrics, and response options

The middleware exposes Prometheus collector toggles and `HandlerOpts` via
`Config`. By default it creates a private `Registerer`/`Gatherer` pair and uses
that for both registration and scraping. When customizing the registry, ensure
that the `Registerer` and `Gatherer` refer to the same metrics source (for
example, a `*prometheus.Registry`). Supplying only one that does not implement
the other interface or providing a mismatched pair will cause initialization to
panic so metrics are not silently dropped.

- `DisableGoCollector` disables the default Go runtime metrics collector when set to `true`.
- `DisableProcessCollector` disables the default process metrics collector when set to `true`.

- `EnableOpenMetrics` negotiates the experimental OpenMetrics encoding so exemplars are exported.
- `EnableOpenMetricsTextCreatedSamples` adds synthetic `_created` samples when OpenMetrics is enabled.
- `DisableCompression` disables gzip/zstd compression even when clients request it.

- `TrackUnmatchedRequests` records metrics for requests that miss all registered routes using `UnmatchedRouteLabel` as the path label. Defaults to `false`.
- `UnmatchedRouteLabel` customizes the path label applied to unmatched requests when tracking is enabled. Defaults to `/__unmatched__`.

- `RequestDurationBuckets`, `RequestSizeBuckets`, and `ResponseSizeBuckets` customize the histogram buckets used for latency and payload metrics. They default to:
  - Duration: `[0.005 0.01 0.025 0.05 0.075 0.1 0.25 0.5 0.75 1 2.5 5 10 15 30 60]`
  - Request size: `[256 512 1024 2048 4096 8192 16384 32768 65536 131072 262144 524288 1048576 2097152 5242880]`
  - Response size: `[256 512 1024 2048 4096 8192 16384 32768 65536 131072 262144 524288 1048576 2097152 5242880]`

All of the boolean options default to `false` and can be enabled or disabled individually as needed.

- `MetricsPath` sets the path served with the Prometheus exposition format. Defaults to `/metrics`. Requests to it are answered by the middleware itself and are never instrumented.
- `SkipURIs` is matched against the registered route pattern, so use `/user/:id` rather than `/user/42`.
- `Next` gates the middleware entirely: when it returns `true` the request is passed straight through, including requests to `MetricsPath`.

## 📊 Result

- Hit the default url at http://localhost:3000
- Navigate to http://localhost:3000/metrics
- Metrics are recorded only for routes registered with Fiber unless `TrackUnmatchedRequests` is enabled, in which case unmatched requests are labeled with `UnmatchedRouteLabel`.

## 📈 Grafana Dashboard

- https://grafana.com/grafana/dashboards/14331
