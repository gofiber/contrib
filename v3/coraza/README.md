---
id: coraza
---

# Coraza

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*coraza*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20Coraza/badge.svg)

[Coraza](https://coraza.io/) WAF middleware for Fiber.

**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get github.com/gofiber/fiber/v3
go get github.com/gofiber/contrib/v3/coraza
```

## Signature

```go
coraza.New(config ...coraza.Config) fiber.Handler
coraza.NewEngine(config coraza.Config) (*coraza.Engine, error)
```

## Config

| Property | Type | Description | Default |
|:--|:--|:--|:--|
| Next | `func(fiber.Ctx) bool` | Defines a function to skip this middleware when it returns true | `nil` |
| BlockHandler | `func(fiber.Ctx, coraza.InterruptionDetails) error` | Custom handler for blocked requests | `nil` |
| ErrorHandler | `func(fiber.Ctx, coraza.MiddlewareError) error` | Custom handler for middleware failures | `nil` |
| DirectivesFile | `[]string` | Coraza directives files loaded in order | `nil` |
| RootFS | `fs.FS` | Optional filesystem used to resolve `DirectivesFile` | `nil` |
| BlockMessage | `string` | Message returned by the built-in block handler | `"Request blocked by Web Application Firewall"` |
| LogLevel | `fiberlog.Level` | Middleware lifecycle log level | `fiberlog.LevelInfo` in `coraza.ConfigDefault` |
| RequestBodyAccess | `bool` | Enables request body inspection | `true` in `coraza.ConfigDefault` |
| MetricsCollector | `coraza.MetricsCollector` | Optional custom in-memory metrics collector | `nil` (falls back to the built-in collector) |

If you want the defaults, start from `coraza.ConfigDefault` and override the fields you need.
For zero-value-backed settings such as `RequestBodyAccess: false`, `LogLevel: fiberlog.LevelTrace`, or resetting `MetricsCollector` to the built-in default, use `ConfigDefault` or the helper methods `WithRequestBodyAccess`, `WithLogLevel`, and `WithMetricsCollector` so the choice remains explicit.
By default, the middleware starts without external rule files. Set `DirectivesFile` to load your Coraza or CRS ruleset.
Request body size follows the Fiber app `BodyLimit`.
Wildcard entries in `DirectivesFile` are expanded before Coraza initializes. If a wildcard matches no files, initialization fails with an error and the middleware does not start.

## Usage

```go
package main

import (
	"log"

	"github.com/gofiber/contrib/v3/coraza"
	"github.com/gofiber/fiber/v3"
)

func main() {
	app := fiber.New()

	cfg := coraza.ConfigDefault
	cfg.DirectivesFile = []string{"./conf/coraza.conf"}

	app.Use(coraza.New(cfg))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	log.Fatal(app.Listen(":3000"))
}
```

## Advanced usage with Engine

Use `NewEngine` when you need explicit lifecycle control, reload support, or observability data.

```go
engineCfg := coraza.ConfigDefault
engineCfg.DirectivesFile = []string{"./conf/coraza.conf"}

engine, err := coraza.NewEngine(engineCfg)
if err != nil {
	log.Fatal(err)
}

app.Use(engine.Middleware(coraza.MiddlewareConfig{
	Next: func(c fiber.Ctx) bool {
		return c.Path() == "/healthz"
	},
	BlockHandler: func(c fiber.Ctx, details coraza.InterruptionDetails) error {
		return c.Status(details.StatusCode).JSON(fiber.Map{
			"blocked": true,
			"rule_id": details.RuleID,
		})
	},
}))
```

## Engine observability

The middleware does not open operational routes for you, but `Engine` exposes data-oriented methods that can be used to build your own endpoints:

- `engine.Reload()`
- `engine.MetricsSnapshot()`
- `engine.Snapshot()`
- `engine.Report()`

## Notes

- Request headers and request bodies are inspected.
- Request body size follows the Fiber app `BodyLimit`.
- Response body inspection is not supported.
- `coraza.New()` starts successfully without external rule files, but it does not load any rules until `DirectivesFile` is configured.
- Invalid configuration causes `coraza.New(...)` to panic during startup, which allows applications to fail fast.

## References

- [Coraza Docs](https://coraza.io/)
- [OWASP Core Rule Set](https://coraza.io/docs/tutorials/coreruleset)
