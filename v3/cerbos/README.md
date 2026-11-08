---
id: cerbos
---

# Cerbos

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*cerbos*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20Cerbos/badge.svg)

[Cerbos](https://github.com/cerbos/cerbos) authorization middleware for Fiber. For each request it asks a Cerbos policy decision point (PDP) whether the principal may perform the action on the resource, and only calls the next handler if the answer is yes.

This middleware authorizes requests. It doesn't authenticate them, so it runs after whatever establishes the user, such as [JWT](https://github.com/gofiber/contrib/tree/main/v3/jwt) or [PASETO](https://github.com/gofiber/contrib/tree/main/v3/paseto).

**Compatible with Fiber v3.**

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/cerbos
go get -u github.com/cerbos/cerbos-sdk-go
```

The middleware needs a running PDP. See [Running Cerbos](#running-cerbos) for a local one.

## Signature

```go
cerbos.New(config cerbos.Config) fiber.Handler
```

`New` panics if `Client`, `Principal` or `Resource` is missing.

## Config

| Property     | Type                                  | Description                                                                 | Default                 |
|:-------------|:--------------------------------------|:----------------------------------------------------------------------------|:------------------------|
| Next         | `func(fiber.Ctx) bool`                | Skips the middleware when it returns true                                   | `nil`                   |
| Client       | `cerbos.Client`                       | Checks access; `*cerbos.GRPCClient` from `cerbos-sdk-go` implements it      | Required                |
| Principal    | `func(fiber.Ctx) (*Principal, error)` | Builds the principal; a nil principal with a nil error goes to Unauthorized | Required                |
| Resource     | `func(fiber.Ctx) (*Resource, error)`  | Builds the resource; a nil resource with a nil error goes to ErrorHandler   | Required                |
| Action       | `func(fiber.Ctx) string`              | Names the action to check                                                   | The HTTP method         |
| Unauthorized | `fiber.Handler`                       | Handles a request without a principal                                       | `401 Unauthorized`      |
| Forbidden    | `fiber.Handler`                       | Handles a request Cerbos denies                                             | `403 Forbidden`         |
| ErrorHandler | `fiber.ErrorHandler`                  | Handles errors from Principal, Resource and the Client                      | `500`, without the text |

`Principal` and `Resource` are types from `github.com/cerbos/cerbos-sdk-go/cerbos`.

## How a request is handled

1. If `Next` returns true, the request skips the check.
2. `Principal` runs. A nil principal goes to `Unauthorized`, without asking Cerbos.
3. `Resource` runs, and `Action` names the action. The default is the HTTP method, with `HEAD` checked as `GET` because Fiber serves `HEAD` on every `GET` route.
4. Cerbos is asked. If it allows the action the next handler runs, and otherwise the request goes to `Forbidden`.

The middleware fails closed: an error from `Principal`, `Resource` or the client goes to `ErrorHandler` and never reaches the next handler. If Cerbos can't be reached, the request gets the `ErrorHandler` response, not an allow.

## Example

Keep the middleware in a variable and pass it to the routes it protects. Here each route's HTTP method is the action:

```go
package main

import (
	"log"

	sdk "github.com/cerbos/cerbos-sdk-go/cerbos"
	"github.com/gofiber/contrib/v3/cerbos"
	"github.com/gofiber/fiber/v3"
)

// authenticate stands in for your authentication middleware.
func authenticate(c fiber.Ctx) error {
	fiber.Locals(c, "user", c.Get("X-User"))
	fiber.Locals(c, "role", c.Get("X-Role"))
	return c.Next()
}

func currentUser(c fiber.Ctx) (*sdk.Principal, error) {
	user := fiber.Locals[string](c, "user")
	if user == "" {
		return nil, nil
	}
	return sdk.NewPrincipal(user, fiber.Locals[string](c, "role")), nil
}

func post(c fiber.Ctx) (*sdk.Resource, error) {
	return sdk.NewResource("post", c.Params("id")), nil
}

func main() {
	client, err := sdk.New("localhost:3593", sdk.WithPlaintext())
	if err != nil {
		log.Fatal(err)
	}

	authz := cerbos.New(cerbos.Config{
		Client:    client,
		Principal: currentUser,
		Resource:  post,
	})

	app := fiber.New()
	app.Use(authenticate)

	app.Get("/posts/:id", authz, func(c fiber.Ctx) error {
		return c.SendString("post " + c.Params("id"))
	})
	app.Delete("/posts/:id", authz, func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})

	log.Fatal(app.Listen(":3000"))
}
```

## Running Cerbos

A policy names the actions each role may perform. With the HTTP method as the action, the example above needs a policy like this one, saved as `policies/post.yaml`:

```yaml
apiVersion: api.cerbos.dev/v1
resourcePolicy:
  version: default
  resource: post
  rules:
    - actions: ["GET"]
      effect: EFFECT_ALLOW
      roles: ["user", "admin"]
    - actions: ["DELETE"]
      effect: EFFECT_ALLOW
      roles: ["admin"]
```

Start a PDP that serves the policy folder:

```sh
docker run --rm -p 3593:3593 -v "$(pwd)/policies:/policies" ghcr.io/cerbos/cerbos:0.57.0
```

Then try it:

```sh
curl -i -H "X-User: bob" -H "X-Role: user" localhost:3000/posts/1                # 200
curl -i -H "X-User: bob" -H "X-Role: user" -X DELETE localhost:3000/posts/1      # 403
curl -i -H "X-User: bob" -H "X-Role: admin" -X DELETE localhost:3000/posts/1     # 204
curl -i localhost:3000/posts/1                                                   # 401
```

## Naming actions

Policies usually name actions after what they do. Set `Action` to check something other than the HTTP method. Using one middleware per action keeps each route's action next to the route:

```go
func can(client cerbos.Client, action string) fiber.Handler {
	return cerbos.New(cerbos.Config{
		Client:    client,
		Principal: currentUser,
		Resource:  post,
		Action:    func(fiber.Ctx) string { return action },
	})
}

app.Get("/posts/:id", can(client, "read"), showPost)
app.Post("/posts/:id/publish", can(client, "publish"), publishPost)
```

## Attributes and conditions

Policy conditions can read attributes of the principal and the resource. To let authors edit their own posts, load the post and pass its owner as an attribute:

```go
func storedPost(c fiber.Ctx) (*sdk.Resource, error) {
	p, err := posts.Find(c.Context(), c.Params("id"))
	if err != nil {
		return nil, err
	}
	return sdk.NewResource("post", p.ID).WithAttr("owner", p.AuthorID), nil
}
```

```yaml
- actions: ["PUT"]
  effect: EFFECT_ALLOW
  roles: ["user"]
  condition:
    match:
      expr: request.resource.attr.owner == request.principal.id
```

`posts.Find` returning an error sends the request to `ErrorHandler`. To answer `404` for a missing post, handle that error in `ErrorHandler`, as in the next section.

## Custom responses

```go
authz := cerbos.New(cerbos.Config{
	Client:    client,
	Principal: currentUser,
	Resource:  storedPost,
	Unauthorized: func(c fiber.Ctx) error {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "sign in first"})
	},
	Forbidden: func(c fiber.Ctx) error {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "not allowed"})
	},
	ErrorHandler: func(c fiber.Ctx, err error) error {
		if errors.Is(err, posts.ErrNotFound) {
			return c.SendStatus(fiber.StatusNotFound)
		}
		log.Printf("authorization failed: %v", err)
		return c.SendStatus(fiber.StatusServiceUnavailable)
	},
})
```

A `Resource` func that returns nil without an error reaches `ErrorHandler` as `cerbos.ErrMissingResource`.

## Skipping the check

`Next` lets a request through without asking Cerbos. Use it for routes that are public under an otherwise protected prefix:

```go
authz := cerbos.New(cerbos.Config{
	Next:      func(c fiber.Ctx) bool { return c.Path() == "/posts/feed" },
	Client:    client,
	Principal: currentUser,
	Resource:  post,
})
```

## Testing your routes

`Client` is a one-method interface, so tests don't need a PDP:

```go
type allowAll struct{}

func (allowAll) IsAllowed(context.Context, *sdk.Principal, *sdk.Resource, string) (bool, error) {
	return true, nil
}
```
