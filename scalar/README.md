---
id: scalar
title: Scalar
---

# Scalar

Scalar middleware for [Fiber](https://github.com/gofiber/fiber). The middleware handles Scalar UI.

**Note: Requires Go 1.23.0 and above**

### Table of Contents
- [Signatures](#signatures)
- [Installation](#installation)
- [Examples](#examples)
- [Config](#config)
- [Default Config](#default-config)

### Signatures
```go
func New(config ...scalar.Config) fiber.Handler
```

### Installation
Scalar is tested on the latest [Go versions](https://golang.org/dl/) with support for modules. So make sure to initialize one first if you didn't do that yet:
```bash
go mod init github.com/<user>/<repo>
```
And then install the Scalar middleware:
```bash
go get github.com/gofiber/contrib/scalar
```

### Examples
Import the middleware package
```go
import (
  "github.com/gofiber/fiber/v2"
  "github.com/gofiber/contrib/scalar"
)
```

Using swaggo to generate documents default output path is `(root)/docs`:
```bash
swag init -v3.1
```

Using the default config:
```go
app.Use(scalar.New())
```

Using a custom config:
```go
cfg := scalar.Config{
    BasePath: "/",
    FilePath: "./docs/swagger.json",
    Path:     "swagger",
    Title:    "Swagger API Docs",
}

app.Use(scalar.New(cfg))
```

Use program data for Swagger content:
```go
cfg := scalar.Config{
    BasePath:    "/",
    FilePath:    "./docs/swagger.json",
    FileContent: mySwaggerByteSlice,
    Path:        "swagger",
    Title:       "Swagger API Docs",
}

app.Use(scalar.New(cfg))
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
	// Optional. Default: ./docs/swagger.json
	FilePath string

	// FileContent for the content of the swagger.json or swagger.yaml file.
	// If provided, FilePath will not be read.
	//
	// Optional. Default: nil
	FileContent []byte

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
	// Optional. Default: 0 (no cache)
	CacheAge int
}
```

### Default Config
```go
var ConfigDefault = Config{
	Next:     nil,
	BasePath: "/",
	FilePath: "./docs/swagger.json",
	Path:     "docs",
	Title:    "Fiber API documentation",
	CacheAge: 0,
}
```
