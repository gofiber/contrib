---
id: newrelic
---

# New Relic

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*newrelic*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20newrelic/badge.svg)

[New Relic](https://github.com/newrelic/go-agent) support for Fiber.

Incoming request headers are forwarded to New Relic transactions by default. This enables distributed tracing header processing, but can also forward sensitive headers. Use `RequestHeaderFilter` to allowlist or redact headers as needed.


**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/newrelic
```

## Signature

```go
newrelic.New(config newrelic.Config) fiber.Handler
```

## Config

| Property               | Type             | Description                                                 | Default                         |
|:-----------------------|:-----------------|:------------------------------------------------------------|:--------------------------------|
| License                | `string`         | Required - New Relic License Key                            | `""`                            |
| AppName                | `string`         | New Relic Application Name                                  | `fiber-api`                     |
| Enabled                | `bool`           | Enable/Disable New Relic                                    | `false`                         |
| ~~TransportType~~      | ~~`string`~~     | ~~Can be HTTP or HTTPS~~ (Deprecated)                       | ~~`"HTTP"`~~                    |
| Application            | `Application`    | Existing New Relic App                                      | `nil`                           |
| ErrorStatusCodeHandler | `func(c fiber.Ctx, err error) int`    | If you want to change newrelic status code, you can use it. | `DefaultErrorStatusCodeHandler` |
| Next                   | `func(c fiber.Ctx) bool`    | Next defines a function to skip this middleware when returned true.                                                           | `nil`                           |
| RequestHeaderFilter    | `func(key, value string) bool`    | Return `true` to forward a request header to New Relic, `false` to skip it. | `nil` (forward all headers) |

## Usage

```go
package main

import (
    "github.com/gofiber/fiber/v3"
    middleware "github.com/gofiber/contrib/v3/newrelic"
)

func main() {
    app := fiber.New()

    app.Get("/", func(ctx fiber.Ctx) error {
        return ctx.SendStatus(200)
    })

    cfg := middleware.Config{
        License:       "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF",
        AppName:       "MyCustomApi",
        Enabled:       true,
    }

    app.Use(middleware.New(cfg))

    app.Listen(":8080")
}
```

## Usage with existing New Relic application

```go
package main

import (
    "github.com/gofiber/fiber/v3"
    middleware "github.com/gofiber/contrib/v3/newrelic"
    nr "github.com/newrelic/go-agent/v3/newrelic"
)

func main() {
    nrApp, err := nr.NewApplication(
        nr.ConfigAppName("MyCustomApi"),
        nr.ConfigLicense("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF"),
        nr.ConfigEnabled(true),
    )

    app := fiber.New()

    app.Get("/", func(ctx fiber.Ctx) error {
        return ctx.SendStatus(200)
    })
    
    app.Get("/foo", func(ctx fiber.Ctx) error {
        txn := middleware.FromContext(ctx)
        segment := txn.StartSegment("foo segment")
        defer segment.End()
        
        // do foo 

        return nil
    })

    cfg := middleware.Config{
        Application:       nrApp,
    }

    app.Use(middleware.New(cfg))

    app.Listen(":8080")
}
```
