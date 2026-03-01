---
id: sentry
---

# Sentry

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*sentry*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20sentry/badge.svg)

[Sentry](https://sentry.io/) support for Fiber.


**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/sentry
go get -u github.com/getsentry/sentry-go
```

## Signature

```go
sentry.New(config ...sentry.Config) fiber.Handler
```

## Config

| Property        | Type            | Description                                                                                                                                                                                                                                                          | Default           |
| :-------------- | :-------------- | :------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | :---------------- |
| Repanic         | `bool`          | Repanic configures whether Sentry should repanic after recovery. Set to true, if [Recover](https://github.com/gofiber/fiber/tree/master/middleware/recover) middleware is used.                                                                                      | `false`           |
| WaitForDelivery | `bool`          | WaitForDelivery configures whether you want to block the request before moving forward with the response. If [Recover](https://github.com/gofiber/fiber/tree/master/middleware/recover) middleware is used, it's safe to either skip this option or set it to false. | `false`           |
| Timeout         | `time.Duration` | Timeout for the event delivery requests.                                                                                                                                                                                                                             | `time.Second * 2` |

## Usage

`sentry` attaches an instance of `*sentry.Hub` (https://godoc.org/github.com/getsentry/sentry-go#Hub) to the request's context, which makes it available throughout the rest of the request's lifetime.
You can access it by using the `sentry.GetHubFromContext()` or `sentry.MustGetHubFromContext()` method on the context itself in any of your proceeding middleware and routes.
Keep in mind that `*sentry.Hub` should be used instead of the global `sentry.CaptureMessage`, `sentry.CaptureException`, or any other calls, as it keeps the separation of data between the requests.

- **Keep in mind that `*sentry.Hub` won't be available in middleware attached before `sentry`. In this case, `GetHubFromContext()` returns nil, and `MustGetHubFromContext()` will panic.**

```go
package main

import (
    "fmt"
    "log"

    sdk "github.com/getsentry/sentry-go"
    fiberSentry "github.com/gofiber/contrib/v3/sentry"
    "github.com/gofiber/fiber/v3"
    "github.com/gofiber/fiber/v3/utils"
)

func main() {
    _ = sdk.Init(sdk.ClientOptions{
        Dsn: "",
        BeforeSend: func(event *sdk.Event, hint *sdk.EventHint) *sdk.Event {
            if hint.Context != nil {
                if c, ok := hint.Context.Value(sdk.RequestContextKey).(fiber.Ctx); ok {
                    // You have access to the original Context if it panicked
                    fmt.Println(utils.ImmutableString(c.Hostname()))
                }
            }
            fmt.Println(event)
            return event
        },
        Debug:            true,
        AttachStacktrace: true,
    })

    app := fiber.New()

    app.Use(fiberSentry.New(fiberSentry.Config{
        Repanic:         true,
        WaitForDelivery: true,
    }))

    enhanceSentryEvent := func(c fiber.Ctx) error {
        if hub := fiberSentry.GetHubFromContext(c); hub != nil {
            hub.Scope().SetTag("someRandomTag", "maybeYouNeedIt")
        }
        return c.Next()
    }

    app.All("/foo", enhanceSentryEvent, func(c fiber.Ctx) error {
        panic("y tho")
    })

    app.All("/", func(c fiber.Ctx) error {
        if hub := fiberSentry.GetHubFromContext(c); hub != nil {
            hub.WithScope(func(scope *sdk.Scope) {
                scope.SetExtra("unwantedQuery", "someQueryDataMaybe")
                hub.CaptureMessage("User provided unwanted query string, but we recovered just fine")
            })
        }
        return c.SendStatus(fiber.StatusOK)
    })

    log.Fatal(app.Listen(":3000"))
}
```

## Accessing Context in `BeforeSend` callback

```go
import (
    "fmt"

    "github.com/gofiber/fiber/v3"
    sdk "github.com/getsentry/sentry-go"
)

sdk.Init(sdk.ClientOptions{
    Dsn: "your-public-dsn",
    BeforeSend: func(event *sdk.Event, hint *sdk.EventHint) *sdk.Event {
        if hint.Context != nil {
            if c, ok := hint.Context.Value(sdk.RequestContextKey).(fiber.Ctx); ok {
                // You have access to the original Context if it panicked
                fmt.Println(c.Hostname())
            }
        }
        return event
    },
})
```
