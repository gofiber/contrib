---
id: swagger
title: Swagger
---

# Swagger

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=swagger*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Tests/badge.svg)
![Security](https://github.com/gofiber/contrib/workflows/Security/badge.svg)
![Linter](https://github.com/gofiber/contrib/workflows/Linter/badge.svg)

Swagger middleware for [Fiber](https://github.com/gofiber/fiber). The middleware handles Swagger UI. 

**Note: Requires Go 1.18 and above**

### Table of Contents
- [Signatures](#signatures)
- [Installation](#installation)
- [Examples](#examples)
- [Config](#config)
- [Default Config](#default-config)

### Signatures
```go
func New(config ...swagger.Config) fiber.Handler
```

### Installation
Swagger is tested on the latests [Go versions](https://golang.org/dl/) with support for modules. So make sure to initialize one first if you didn't do that yet:
```bash
go mod init github.com/<user>/<repo>
```
And then install the swagger middleware:
```bash
go get github.com/gofiber/contrib/swagger
```

### Examples
Import the middleware package
```go
import (
  "github.com/gofiber/fiber/v2"
  "github.com/gofiber/contrib/swagger"
)
```

Using the default config:
```go
app.Use(swagger.New(cfg))
```

Using a custom config:
```go
cfg := swagger.Config{
    BasePath: "/",
    FilePath: "./docs/swagger.json",
    Path:     "swagger",
    Title:    "Swagger API Docs",
}

app.Use(swagger.New(cfg))
```

Using multiple instances of Swagger:
```go
// Create Swagger middleware for v1
//
// Swagger will be available at: /api/v1/docs
app.Use(swagger.New(swagger.Config{
    BasePath: "/api/v1/",
    FilePath: "./docs/v1/swagger.json",
    Path:     "docs",
}))

// Create Swagger middleware for v2
//
// Swagger will be available at: /api/v2/docs
app.Use(swagger.New(swagger.Config{
    BasePath: "/api/v2/",
    FilePath: "./docs/v2/swagger.json",
    Path:     "docs",
}))
```

### Config
```go
type Config struct {
	// Next defines a function to skip this middleware when returned true.
	//
	// Optional. Default: nil
	Next func(c *fiber.Ctx) bool

	// BasePath for the UI path
	//
	// Optional. Default: /
	BasePath string

	// FilePath for the swagger.json or swagger.yaml file
	//
	// Optional. Default: ./swagger.json
	FilePath string

	// Path combines with BasePath for the full UI path
	//
	// Optional. Default: docs
	Path string

	// Title for the documentation site
	//
	// Optional. Default: Fiber API documentation
	Title string

	// CacheAge defines the max-age for the Cache-Control header in seconds.
	//
	// Optional. Default: 3600 (1 hour)
	CacheAge int
}
```

### Default Config
```go
var ConfigDefault = Config{
	Next:     nil,
	BasePath: "/",
	FilePath: "./swagger.json",
	Path:     "docs",
	Title:    "Fiber API documentation",
	CacheAge: 3600, // Default to 1 hour
}
```