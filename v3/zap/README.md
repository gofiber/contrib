---
id: zap
---

# Zap

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*zap*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20zap/badge.svg)

[Zap](https://github.com/uber-go/zap) logging support for Fiber.


**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/zap
go get -u go.uber.org/zap
```

### Signature

```go
zap.New(config ...zap.Config) fiber.Handler
```

### Config

| Property   | Type                       | Description                                                                                                                                                                    | Default                                                                     |
| :--------- | :------------------------- | :----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | :-------------------------------------------------------------------------- |
| Next       | `func(fiber.Ctx) bool`          | Define a function to skip this middleware when returned true                                                                                                                   | `nil`                                                                       |
| Logger     | `*zap.Logger`              | Add custom zap logger.                                                                                                                                                         | `zap.NewProduction()`                                                      |
| Fields     | `[]string`                 | Add fields that you want to see.                                                                                                                                                  | `[]string{"latency", "status", "method", "url"}`                            |
| FieldsFunc | `func(fiber.Ctx) []zap.Field` | Define a function to add custom fields.                                                                                                                                        | `nil`                                                                       |
| Messages   | `[]string`                 | Custom response messages.                                                                                                                                                      | `[]string{"Server error", "Client error", "Success"}`                       |
| Levels     | `[]zapcore.Level`          | Custom response levels.                                                                                                                                                        | `[]zapcore.Level{zapcore.ErrorLevel, zapcore.WarnLevel, zapcore.InfoLevel}` |
| SkipURIs   | `[]string`                 | Skip logging these URI.                                                                                                                                                        | `[]string{}`                                                                |
| GetResBody | `func(c fiber.Ctx) []byte` | Define a function to get response body when return non-nil.<br />eg: When use compress middleware, resBody is unreadable. you can set GetResBody func to get readable resBody. | `nil`                                                                       |

### Example

```go
package main

import (
    "log"

    middleware "github.com/gofiber/contrib/v3/zap"
    "github.com/gofiber/fiber/v3"
    "go.uber.org/zap"
)

func main() {
    app := fiber.New()
    logger, _ := zap.NewProduction()
    defer logger.Sync()

    app.Use(middleware.New(middleware.Config{
        Logger: logger,
    }))

    app.Get("/", func(c fiber.Ctx) error {
        return c.SendString("Hello, World!")
    })

    log.Fatal(app.Listen(":3000"))
}
```

## NewLogger

### Signature

```go
zap.NewLogger(config ...zap.LoggerConfig) *zap.LoggerConfig
```

### LoggerConfig

| Property    | Type           | Description                                                                                              | Default                        |
| :---------- | :------------- | :------------------------------------------------------------------------------------------------------- | :----------------------------- |
| CoreConfigs | `[]CoreConfig` | Define Config for zapcore                                                                                | `zap.LoggerConfigDefault` |
| SetLogger   | `*zap.Logger`  | Add custom zap logger. if not nil, `ZapOptions`, `CoreConfigs`, `SetLevel`, `SetOutput` will be ignored. | `nil`                          |
| ExtraKeys   | `[]string`     | Allow users log extra values from context.                                                               | `[]string{}`                   |
| ZapOptions  | `[]zap.Option` | Allow users to configure the zap.Option supplied by zap.                                                 | `[]zap.Option{}`               |

### Example

```go
package main

import (
    "context"

    middleware "github.com/gofiber/contrib/v3/zap"
    "github.com/gofiber/fiber/v3"
    "github.com/gofiber/fiber/v3/log"
)

func main() {
    app := fiber.New()
    logger := middleware.NewLogger(middleware.LoggerConfig{
        ExtraKeys: []string{"request_id"},
    })
    log.SetLogger(logger)
    defer logger.Sync()

    app.Use(func(c fiber.Ctx) error {
        ctx := context.WithValue(c.UserContext(), "request_id", "123")
        c.SetUserContext(ctx)
        return c.Next()
    })
    app.Get("/", func(c fiber.Ctx) error {
        log.WithContext(c.UserContext()).Info("Hello, World!")
        return c.SendString("Hello, World!")
    })
    log.Fatal(app.Listen(":3000"))
}
```
