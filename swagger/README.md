---
id: swagger
title: Swagger
---

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=swagger*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Tests/badge.svg)
![Security](https://github.com/gofiber/contrib/workflows/Security/badge.svg)
![Linter](https://github.com/gofiber/contrib/workflows/Linter/badge.svg)

Swagger middleware for [Fiber](https://github.com/gofiber/fiber). The middleware handles Swagger UI. 

### Table of Contents
- [Signatures](#signatures)
- [Examples](#examples)


### Signatures
```go
func New(config ...Config) fiber.Handler
```

### Examples
Import the middleware package that is part of the Fiber web framework
```go
import (
  "github.com/gofiber/fiber/v2"
  "github.com/gofiber/contrib/swagger"
)
```

Then create a Fiber app with app := fiber.New().

After you initiate your Fiber app, you can use the following possibilities:

### Default Config

```go
app.Use(swagger.New(cfg))
```

### Custom Config

```go
cfg := swagger.Config{
    BasePath: "/", //swagger ui base path
    FilePath: "./docs/swagger.json",
}

app.Use(swagger.New(cfg))
```
