---
id: fgprof
---

# Fgprof

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=fgprof*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20Fgprof/badge.svg)

[fgprof](https://github.com/felixge/fgprof) support for Fiber.

**Note: Requires Go 1.25 and above**

## Install

**Compatible with Fiber v3.**

Using fgprof to profiling your Fiber app.

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/fgprof
```

## Config

| Property | Type                      | Description                                                                                                                                      | Default |
|----------|---------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------|---------|
| Next     | `func(c fiber.Ctx) bool` | A function to skip this middleware when returned `true`.                                                                                         | `nil`   |
| Prefix   | `string`.                 | Prefix defines a URL prefix added before "/debug/fgprof". Note that it should start with (but not end with) a slash. Example: "/federated-fiber" | `""`    |

## Example

```go
package main

import (
    "log"

    "github.com/gofiber/contrib/v3/fgprof"
    "github.com/gofiber/fiber/v3"
)

func main() {
    app := fiber.New()
    app.Use(fgprof.New())
    app.Get("/", func(c fiber.Ctx) error {
        return c.SendString("OK")
    })
    log.Fatal(app.Listen(":3000"))
}
```

```bash
go tool pprof -http=:8080 http://localhost:3000/debug/fgprof
```
