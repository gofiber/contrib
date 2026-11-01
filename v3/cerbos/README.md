---
id: cerbos
---

# Cerbos

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*cerbos*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20Cerbos/badge.svg)

[Cerbos](https://www.cerbos.dev) authorization middleware for Fiber. For each request it asks a Cerbos policy decision point whether the principal may perform the action on the resource, and only lets the request through if the answer is yes.

**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/cerbos
```

## Signature

```go
cerbos.New(config cerbos.Config) fiber.Handler
```

`New` panics if `Client`, `Principal` or `Resource` is missing.

## Config

| Property     | Type                                         | Description                                                                 | Default                 |
|:-------------|:---------------------------------------------|:----------------------------------------------------------------------------|:------------------------|
| Next         | `func(fiber.Ctx) bool`                       | Skips the middleware when it returns true                                   | `nil`                   |
| Client       | `cerbos.Client`                              | Checks access; `*cerbos.GRPCClient` from `cerbos-sdk-go` implements it      | Required                |
| Principal    | `func(fiber.Ctx) (*Principal, error)`        | Builds the principal; a nil principal with a nil error goes to Unauthorized | Required                |
| Resource     | `func(fiber.Ctx) (*Resource, error)`         | Builds the resource                                                         | Required                |
| Action       | `func(fiber.Ctx) string`                     | Names the action to check                                                   | The HTTP method         |
| Unauthorized | `fiber.Handler`                              | Handles a request without a principal                                       | `401 Unauthorized`      |
| Forbidden    | `fiber.Handler`                              | Handles a request Cerbos denies                                             | `403 Forbidden`         |
| ErrorHandler | `fiber.ErrorHandler`                         | Handles errors from Principal, Resource and the Client                      | `500`, without the text |

`Principal` and `Resource` are types from `github.com/cerbos/cerbos-sdk-go/cerbos`.

A request never reaches the next handler when a check fails or errors. If Cerbos can't be reached, the request gets the `ErrorHandler` response, not an allow.

## Example

```go
package main

import (
	"log"

	sdk "github.com/cerbos/cerbos-sdk-go/cerbos"
	"github.com/gofiber/contrib/v3/cerbos"
	"github.com/gofiber/fiber/v3"
)

func main() {
	client, err := sdk.New("localhost:3593", sdk.WithPlaintext())
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	app := fiber.New()

	app.Delete("/posts/:id", cerbos.New(cerbos.Config{
		Client: client,
		Principal: func(c fiber.Ctx) (*sdk.Principal, error) {
			// Replace this with what your authentication middleware stored.
			user, _ := c.Locals("user").(string)
			if user == "" {
				return nil, nil
			}
			role, _ := c.Locals("role").(string)
			return sdk.NewPrincipal(user, role), nil
		},
		Resource: func(c fiber.Ctx) (*sdk.Resource, error) {
			return sdk.NewResource("post", c.Params("id")), nil
		},
		Action: func(fiber.Ctx) string { return "delete" },
	}), func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})

	log.Fatal(app.Listen(":3000"))
}
```

The action defaults to the HTTP method, so without `Action` your policy's rules would name `GET`, `DELETE` and so on.
