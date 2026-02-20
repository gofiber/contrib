---
id: monitor
---

# Monitor

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*monitor*)
![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)
![Test](https://github.com/gofiber/contrib/workflows/Test%20Monitor/badge.svg)

Monitor middleware for [Fiber](https://github.com/gofiber/fiber) that reports server metrics, inspired by [express-status-monitor](https://github.com/RafalWilinski/express-status-monitor)

![](https://i.imgur.com/nHAtBpJ.gif)

**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/monitor
```

### Signature

```go
monitor.New(config ...monitor.Config) fiber.Handler
```

### Config

| Property   | Type                      | Description                                                                          | Default                                                                     |
| :--------- | :------------------------ | :----------------------------------------------------------------------------------- | :-------------------------------------------------------------------------- |
| Title      | `string`                  | Metrics page title.                                                                  | `Fiber Monitor`                                                             |
| Refresh    | `time.Duration`           | Refresh period.                                                                      | `3 seconds`                                                                 |
| APIOnly    | `bool`                    | Whether the service should expose only the montioring API.                           | `false`                                                                     |
| Next       | `func(c fiber.Ctx) bool` | Define a function to add custom fields.                                              | `nil`                                                                       |
| CustomHead | `string`                  | Custom HTML code to Head Section(Before End).                                        | `empty`                                                                     |
| FontURL    | `string`                  | FontURL for specilt font resource path or URL. also you can use relative path.       | `https://fonts.googleapis.com/css2?family=Roboto:wght@400;900&display=swap` |
| ChartJsURL | `string`                  | ChartJsURL for specilt chartjs library, path or URL, also you can use relative path. | `https://cdn.jsdelivr.net/npm/chart.js@2.9/dist/Chart.bundle.min.js`        |

### Example

```go
package main

import (
    "log"

    "github.com/gofiber/fiber/v3"
    "github.com/gofiber/contrib/v3/monitor"
)

func main() {
    app := fiber.New()

    // Initialize default config (Assign the middleware to /metrics)
    app.Get("/metrics", monitor.New())

    // Or extend your config for customization
    // Assign the middleware to /metrics
    // and change the Title to `MyService Metrics Page`
    app.Get("/metrics", monitor.New(monitor.Config{Title: "MyService Metrics Page"}))

    log.Fatal(app.Listen(":3000"))
}
```

## Default Config

```go
var ConfigDefault = Config{
    Title:      defaultTitle,
    Refresh:    defaultRefresh,
    FontURL:    defaultFontURL,
    ChartJsURL: defaultChartJSURL,
    CustomHead: defaultCustomHead,
    APIOnly:    false,
    Next:       nil,
}
```
