# Fiberzap

![Release](https://img.shields.io/github/release/gofiber/contrib.svg)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Tests/badge.svg)
![Security](https://github.com/gofiber/contrib/workflows/Security/badge.svg)
![Linter](https://github.com/gofiber/contrib/workflows/Linter/badge.svg)

[Zap](https://github.com/uber-go/zap) logging support for Fiber.

### Install

This middleware supports Fiber v2.

```
go get -u github.com/gofiber/fiber/v2
go get -u github.com/gofiber/contrib/fiberzap
go get -u go.uber.org/zap
```

### Signature

```go
fiberzap.New(config ...Config) fiber.Handler
```

### Config

| Property      | Type                           | Description                                                                                                                                                                   | Default                                                                     |
|:--------------|:-------------------------------|:------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|:----------------------------------------------------------------------------|
| Next          | `func(*Ctx) bool`              | Define a function to skip this middleware when returned true                                                                                                                  | `nil`                                                                       |
| Logger        | `*zap.Logger`                  | Add custom zap logger.                                                                                                                                                        | `zap.NewDevelopment()`                                                      |
| Fields        | `[]string`                     | Add fields what you want see.                                                                                                                                                 | `[]string{"latency", "status", "method", "url"}`                            |
| Messages      | `[]string`                     | Custom response messages.                                                                                                                                                     | `[]string{"Server error", "Client error", "Success"}`                       |                
| Levels        | `[]zapcore.Level`              | Custom response levels.                                                                                                                                                       | `[]zapcore.Level{zapcore.ErrorLevel, zapcore.WarnLevel, zapcore.InfoLevel}` |   
| SkipURIs      | `[]string`                     | Skip logging these URI.                                                                                                                                                       | `[]string{}`                                                                |                
| GetResBody    | func(c *fiber.Ctx) []byte      | Define a function to get response body when return non-nil.<br>eg: When use compress middleware, resBody is unreadable. you can set GetResBody func to get readable resBody.  | `nil`                                                                       |
### Example
```go
package main

import (
    "log"

    "github.com/gofiber/fiber/v2"
    "github.com/gofiber/contrib/fiberzap"
    "go.uber.org/zap"
)

func main() {
    app := fiber.New()
    logger, _ := zap.NewProduction()

    app.Use(fiberzap.New(fiberzap.Config{
        Logger: logger,
    }))

    app.Get("/", func (c *fiber.Ctx) error {
        return c.SendString("Hello, World!")
    })

    log.Fatal(app.Listen(":3000"))
}
```