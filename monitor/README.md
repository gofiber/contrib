---
id: monitor
---

# Monitor

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=monitor*)
![Test](https://github.com/gofiber/contrib/workflows/Tests/badge.svg)
![Security](https://github.com/gofiber/contrib/workflows/Security/badge.svg)
![Linter](https://github.com/gofiber/contrib/workflows/Linter/badge.svg)

## Install

This middleware supports Fiber v3.

```
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/monitor
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
| Next       | `func(c *fiber.Ctx) bool` | Define a function to add custom fields.                                              | `nil`                                                                       |
| CustomHead | `string`                  | Custom HTML code to Head Section(Before End).                                        | `empty`                                                                     |
| FontURL    | `string`                  | FontURL for specilt font resource path or URL. also you can use relative path.       | `https://fonts.googleapis.com/css2?family=Roboto:wght@400;900&display=swap` |
| ChartJsURL | `string`                  | ChartJsURL for specilt chartjs library, path or URL, also you can use relative path. | `https://cdn.jsdelivr.net/npm/chart.js@2.9/dist/Chart.bundle.min.js`        |

> Because jsdelivr lost their ICP license, so chinese users maybe use other CDNs to load ChartJs library.

### Example

```go
package main

import (
    "log"

    "github.com/gofiber/fiber/v3"
    "github.com/gofiber/contrib/monitor"
)

func main() {
    app := fiber.New()

    app.Use("/monitor", monitor.New())


    log.Fatal(app.Listen(":3000"))
}
```
