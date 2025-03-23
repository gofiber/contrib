---
id: opafiber
---

# Opafiber

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=opafiber*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20opafiber/badge.svg)

[Open Policy Agent](https://github.com/open-policy-agent/opa) support for Fiber.

**Note: Requires Go 1.19 and above**

## Install

```
go get -u github.com/gofiber/fiber/v2
go get -u github.com/gofiber/contrib/opafiber/v2
```

## Signature

```go
opafiber.New(config opafiber.Config) fiber.Handler

```

## Config

| Property              | Type                | Description                                                  | Default                                                             |
|:----------------------|:--------------------|:-------------------------------------------------------------|:--------------------------------------------------------------------|
| RegoQuery             | `string`            | Required - Rego query                                        | -                                                                   |
| RegoPolicy            | `io.Reader`         | Required - Rego policy                                       | -                                                                   |
| IncludeQueryString    | `bool`              | Include query string as input to rego policy                 | `false`                                                             |
| DeniedStatusCode      | `int`               | Http status code to return when policy denies request        | `400`                                                               |
| DeniedResponseMessage | `string`            | Http response body text to return when policy denies request | `""`                                                                |
| IncludeHeaders        | `[]string`          | Include headers as input to rego policy                      | -                                                                   |
| InputCreationMethod   | `InputCreationFunc` | Use your own function to provide input for OPA               | `func defaultInput(ctx *fiber.Ctx) (map[string]interface{}, error)` |

## Types

```go
type InputCreationFunc func(c *fiber.Ctx) (map[string]interface{}, error)
```

## Usage

OPA Fiber middleware sends the following example data to the policy engine as input:

```json
{
  "method": "GET",
  "path": "/somePath",
  "query": {
    "name": ["John Doe"]
  },
  "headers": {
    "Accept": "application/json",
    "Content-Type": "application/json"
  }
}
```

```go
package main

import (
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/contrib/opafiber/v2"
)

func main() {
	app := fiber.New()
	module := `
package example.authz

default allow := false

allow {
	input.method == "GET"
}
`

	cfg := opafiber.Config{
		RegoQuery:             "data.example.authz.allow",
		RegoPolicy:            bytes.NewBufferString(module),
		IncludeQueryString:    true,
		DeniedStatusCode:      fiber.StatusForbidden,
		DeniedResponseMessage: "status forbidden",
		IncludeHeaders:        []string{"Authorization"},
		InputCreationMethod:   func (ctx *fiber.Ctx) (map[string]interface{}, error) {
            return map[string]interface{}{
                "method": ctx.Method(),
                "path": ctx.Path(),
            }, nil
        },
	}
	app.Use(opafiber.New(cfg))

	app.Get("/", func(ctx *fiber.Ctx) error {
		return ctx.SendStatus(200)
	})

	app.Listen(":8080")
}
```
