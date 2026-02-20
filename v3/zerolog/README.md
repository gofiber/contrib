---
id: zerolog
---

# Zerolog

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*zerolog*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20zerolog/badge.svg)

[Zerolog](https://zerolog.io/) logging support for Fiber.


**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/zerolog
go get -u github.com/rs/zerolog/log
```

## Signature

```go
zerolog.New(config ...zerolog.Config) fiber.Handler
```

## Config

| Property        | Type                                | Description                                                                                                                                                                                                                                        | Default                                                                  |
|:----------------|:------------------------------------|:---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|:-------------------------------------------------------------------------|
| Next            | `func(fiber.Ctx) bool`              | Define a function to skip this middleware when it returns true.                                                                                                                                                                                   | `nil`                                                                    |
| Logger          | `*zerolog.Logger`                   | Add a custom zerolog logger.                                                                                                                                                                                                                      | `zerolog.New(os.Stderr).With().Timestamp().Logger()`                     |
| GetLogger       | `func(fiber.Ctx) zerolog.Logger`    | Get a custom zerolog logger. If set, the returned logger replaces `Logger`.                                                                                                                                                                       | `nil`                                                                    |
| Fields          | `[]string`                          | Add the fields you want to log.                                                                                                                                                                                                                   | `[]string{"latency", "status", "method", "url", "error"}`               |
| SkipField       | `func(string, fiber.Ctx) bool`      | Skip logging a field when it returns true.                                                                                                                                                                                                         | `nil`                                                                    |
| SkipHeader      | `func(string, fiber.Ctx) bool`      | Skip logging a header when it returns true.                                                                                                                                                                                                        | `nil`                                                                    |
| WrapHeaders     | `bool`                              | Wrap headers into a dictionary.<br />If false: `{"method":"POST", "header-key":"header value"}`<br />If true: `{"method":"POST", "reqHeaders":{"header-key":"header value"}}`                                                                  | `false`                                                                  |
| FieldsSnakeCase | `bool`                              | Use snake case for `FieldResBody`, `FieldQueryParams`, `FieldBytesReceived`, `FieldBytesSent`, `FieldRequestID`, `FieldReqHeaders`, `FieldResHeaders`.<br />If false: `{"method":"POST", "resBody":"v", "queryParams":"v"}`<br />If true: `{"method":"POST", "res_body":"v", "query_params":"v"}` | `false`                                                                  |
| Messages        | `[]string`                          | Custom response messages.                                                                                                                                                                                                                          | `[]string{"Server error", "Client error", "Success"}`                    |
| Levels          | `[]zerolog.Level`                   | Custom response levels.                                                                                                                                                                                                                            | `[]zerolog.Level{zerolog.ErrorLevel, zerolog.WarnLevel, zerolog.InfoLevel}` |
| GetResBody      | `func(c fiber.Ctx) []byte`          | Define a function to get the response body when it returns non-nil.<br />For example, with compress middleware the body can be unreadable; `GetResBody` lets you provide a readable body.                                                      | `nil`                                                                    |

## Example

```go
package main

import (
    "os"

    middleware "github.com/gofiber/contrib/v3/zerolog"
    "github.com/gofiber/fiber/v3"
    "github.com/rs/zerolog"
)

func main() {
    app := fiber.New()
    logger := zerolog.New(os.Stderr).With().Timestamp().Logger()

    app.Use(middleware.New(middleware.Config{
        Logger: &logger,
    }))

    app.Get("/", func(c fiber.Ctx) error {
        return c.SendString("Hello, World!")
    })

    if err := app.Listen(":3000"); err != nil {
        logger.Fatal().Err(err).Msg("Fiber app error")
    }
}
```
