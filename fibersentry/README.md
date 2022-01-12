# Fibersentry

![Release](https://img.shields.io/github/release/gofiber/contrib.svg)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Tests/badge.svg)
![Security](https://github.com/gofiber/contrib/workflows/Security/badge.svg)
![Linter](https://github.com/gofiber/contrib/workflows/Linter/badge.svg)

[Sentry](https://sentry.io/) support for Fiber.

### Install

This middleware supports Fiber v2.

```
go get -u github.com/gofiber/fiber/v2
go get -u github.com/gofiber/contrib/fibersentry
go get -u github.com/getsentry/sentry-go
```

### Signature

```
fibersentry.New(config ...Config) fiber.Handler
```

### Config

| Property       | Type                            | Description                                                                                                                                                                                             | Default                         |
| :------------- | :------------------------------ | :------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | :------------------------------ |
| Repanic| `bool` | Repanic configures whether Sentry should repanic after recovery. Set to true, if [Recover](https://github.com/gofiber/fiber/tree/master/middleware/recover) middleware is used. | `false` |
| WaitForDelivery| `bool` | WaitForDelivery configures whether you want to block the request before moving forward with the response. If [Recover](https://github.com/gofiber/fiber/tree/master/middleware/recover) middleware is used, it's safe to either skip this option or set it to false. | `false` |
| Timeout   | `time.Duration` | Timeout for the event delivery requests. | `time.Second * 2` |


### Usage

`fibersentry` attaches an instance of `*sentry.Hub` (https://godoc.org/github.com/getsentry/sentry-go#Hub) to the request's context, which makes it available throughout the rest of the request's lifetime.
You can access it by using the `fibersentry.GetHubFromContext()` method on the context itself in any of your proceeding middleware and routes.
And it should be used instead of the global `sentry.CaptureMessage`, `sentry.CaptureException`, or any other calls, as it keeps the separation of data between the requests.

**Keep in mind that `*sentry.Hub` won't be available in middleware attached before to `fibersentry`!**

```go
// Later in the code
sentryHandler := fibersentry.New(fibersentry.Options{
    Repanic:         true,
    WaitForDelivery: true,
})

enhanceSentryEvent := func(ctx *fiber.Ctx) {
    if hub := fibersentry.GetHubFromContext(ctx); hub != nil {
        hub.Scope().SetTag("someRandomTag", "maybeYouNeedIt")
    }
    ctx.Next()
}

app := fiber.New()
app.Use(sentryHandler)
app.All("/foo", enhanceSentryEvent, func(ctx *fiber.Ctx) {
    panic("y tho")
})

app.All("/", func(ctx *fiber.Ctx) {
    if hub := fibersentry.GetHubFromContext(ctx); hub != nil {
        hub.WithScope(func(scope *sentry.Scope) {
            scope.SetExtra("unwantedQuery", "someQueryDataMaybe")
            hub.CaptureMessage("User provided unwanted query string, but we recovered just fine")
        })
    }
    ctx.Status(fiber.StatusOK)
})

app.Listen(3000)
```

### Accessing Context in `BeforeSend` callback

```go
sentry.Init(sentry.ClientOptions{
	Dsn: "your-public-dsn",
	BeforeSend: func(event *sentry.Event, hint *sentry.EventHint) *sentry.Event {
		if hint.Context != nil {
			if ctx, ok := hint.Context.Value(sentry.RequestContextKey).(*fiber.Ctx); ok {
				// You have access to the original Context if it panicked
				fmt.Println(ctx.Hostname())
			}
		}
		return event
	},
})
```
