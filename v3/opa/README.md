---
id: opa
---

# OPA

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=opa*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20opa/badge.svg)

[Open Policy Agent](https://github.com/open-policy-agent/opa) support for Fiber.

**Note: Requires Go 1.25 and above**

**Compatible with Fiber v3.**


## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/opa
```

## Signature

```go
opa.New(config opa.Config) fiber.Handler

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
| InputCreationMethod   | `InputCreationFunc` | Use your own function to provide input for OPA               | `func defaultInput(ctx fiber.Ctx) (map[string]interface{}, error)` |

## Types

```go
type InputCreationFunc func(c fiber.Ctx) (map[string]interface{}, error)
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
    "bytes"

    "github.com/gofiber/fiber/v3"
    "github.com/gofiber/contrib/v3/opa"
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

    cfg := opa.Config{
        RegoQuery:             "data.example.authz.allow",
        RegoPolicy:            bytes.NewBufferString(module),
        IncludeQueryString:    true,
        DeniedStatusCode:      fiber.StatusForbidden,
        DeniedResponseMessage: "status forbidden",
        IncludeHeaders:        []string{"Authorization"},
        InputCreationMethod:   func(ctx fiber.Ctx) (map[string]interface{}, error) {
            return map[string]interface{}{
                "method": ctx.Method(),
                "path": ctx.Path(),
            }, nil
        },
    }
    app.Use(opa.New(cfg))

    app.Get("/", func(ctx fiber.Ctx) error {
        return ctx.SendStatus(200)
    })

    app.Listen(":8080")
}
```
