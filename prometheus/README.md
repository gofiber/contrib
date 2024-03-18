id: prometheus
---

# Prometheus

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=prometheus*)
![Test](https://github.com/gofiber/contrib/workflows/Tests/badge.svg)
![Security](https://github.com/gofiber/contrib/workflows/Security/badge.svg)
![Linter](https://github.com/gofiber/contrib/workflows/Linter/badge.svg)

Prometheus middleware for [Fiber](https://github.com/gofiber/fiber).

**Note: Requires Go 1.21 and above**

## Install

```
go get -u github.com/gofiber/fiber/v2
go get -u github.com/gofiber/contrib/prometheus
```

## Following metrics are available by default:

```
http_requests_total
http_request_duration_seconds
http_requests_in_progress_total
http_cache_results
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

  // This here will appear as a label, one can also use
  // fiberprometheus.NewWith(servicename, namespace, subsystem )
  // or
  // labels := map[string]string{"custom_label1":"custom_value1", "custom_label2":"custom_value2"}
  // fiberprometheus.NewWithLabels(labels, namespace, subsystem )
  prom := prometheus.New("my-service-name")
  prom.RegisterAt(app, "/metrics")
  app.Use(prom.Middleware)

  app.Get("/", func(c *fiber.Ctx) error {
    return c.SendString("Hello World")
  })

  app.Post("/hello", func(c *fiber.Ctx) error {
    return c.SendString("Hello World!")
  })

  app.Listen(":3000")
}
```

## Result

- Hit the default url at http://localhost:3000
- Navigate to http://localhost:3000/metrics

## Grafana Dashboards

- https://grafana.com/grafana/dashboards/14331
- https://grafana.com/grafana/dashboards/14061-go-runtime-metrics/

## Credits

- Original middleware was contributed and created by https://github.com/ansrivas under MIT license
