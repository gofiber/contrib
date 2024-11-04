id: prometheus
---

# Prometheus

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=prometheus*)
![Test](https://github.com/gofiber/contrib/workflows/Tests/badge.svg)
![Security](https://github.com/gofiber/contrib/workflows/Security/badge.svg)
![Linter](https://github.com/gofiber/contrib/workflows/Linter/badge.svg)

Prometheus middleware for [Fiber](https://github.com/gofiber/fiber).

**Note: This middleware is only supported on the latest two versions of Go**

## Install

```
go get -u github.com/gofiber/fiber/v2
go get -u github.com/gofiber/contrib/prometheus
```

## Following metrics are available:

```
http_requests_total
http_request_duration_seconds
http_requests_in_progress_total
```

## Example

```go
package main

import (
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/contrib/prometheus"
)

func main() {
  app := fiber.New()
  prom := prometheus.New("my-fiber-app")
  prom.RegisterAt(app, "/metrics")
  app.Use(prom.Middleware)

  app.Get("/", func(c *fiber.Ctx) error {
    return c.SendString("Hello World")
  })

  app.Listen(":3000")
}
```

## Result

- Visit your server http://localhost:3000
- Navigate to http://localhost:3000/metrics to see the Prom metrics

## Grafana Dashboards

- https://grafana.com/grafana/dashboards/14331
- https://grafana.com/grafana/dashboards/14061-go-runtime-metrics/

## Credits

- Thanks to https://github.com/ansrivas for creating the original middleware and contributing it to the GoFiber Framework. This middleware was licensed under MIT License.
