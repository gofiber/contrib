---
id: casbin
---

# Casbin

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*casbin*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20casbin/badge.svg)

Casbin middleware for Fiber.

**Compatible with Fiber v3.**

> This middleware targets **Casbin v3**. Casbin v2 is no longer supported here;
> if you still need it, pin the previous major
> (`github.com/gofiber/contrib/v3/casbin` at its last `v1` tag). Migrating from
> Casbin v2 to v3 is mostly an import-path change; see the
> [Casbin upgrade guide](https://casbin.org/docs/upgrade).

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install
```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/casbin/v2
```
choose an adapter from [here](https://casbin.org/docs/adapters)
```sh
go get -u github.com/casbin/gorm-adapter/v3
```

## Signature
```go
casbin.New(config ...casbin.Config) *casbin.Middleware
```

## Config

| Property      | Type                      | Description                              | Default                              |
|:--------------|:--------------------------|:-----------------------------------------|:--------------------------------------------------------------|
| ModelFilePath | `string`                  | Model file path                        | `"./model.conf"`                                                    |
| PolicyAdapter | `persist.Adapter`         | Database adapter for policies            | `./policy.csv`                                                      |
| Enforcer      | `casbin.IEnforcer`        | Custom casbin enforcer (any `IEnforcer`, e.g. `*casbin.Enforcer`, `*casbin.SyncedEnforcer`, `*casbin.CachedEnforcer`) | `Middleware generated enforcer using ModelFilePath & PolicyAdapter` |
| Lookup        | `func(fiber.Ctx) string`  | Look up for current subject              | `""`                                                              |
| Unauthorized  | `func(fiber.Ctx) error`   | Response body for unauthorized responses | `Unauthorized`                                                      |
| Forbidden     | `func(fiber.Ctx) error`   | Response body for forbidden responses    | `Forbidden`                                                         |

Cached enforcers (`*casbin.CachedEnforcer`, `*casbin.SyncedCachedEnforcer`) keep answering from their cache after policies or roles change, so a revoked permission can still pass, and cached decisions never expire by default. Call `InvalidateCache()` after such changes. A request enforced before the change can still write its old result back afterward, so also call `SetExpireTime` before serving requests (it only applies to decisions cached later), or stop requests while changing policies. If policies change while requests are served, use `*casbin.SyncedCachedEnforcer` or synchronize enforcement and policy updates yourself. Their default cache has no size limit, and `SetExpireTime` does not remove a decision that is not asked for again. `RoutePermission` caches a decision for every path a client sends, so give it a size-limited cache with `SetCache` or an uncached enforcer. Every enforcer also caches role function results (`g`, `g2`) per argument set without a limit, so with a model that passes the path or another client value to them, such as `g2(r.obj, p.obj)`, build the enforcer yourself, call `EnableGFunctionCache(false)` on it before serving requests and again after `LoadModel` or `SetModel`, and pass it as `Enforcer`.

### Examples
- [Gorm Adapter](https://github.com/svcg/-fiber_casbin_demo)
- [File Adapter](https://github.com/gofiber/contrib/tree/master/v3/casbin/example)

## CustomPermission

```go
package main

import (
  "github.com/gofiber/fiber/v3"
  "github.com/gofiber/contrib/v3/casbin/v2"
  _ "github.com/go-sql-driver/mysql"
  gormadapter "github.com/casbin/gorm-adapter/v3"
)

func main() {
  app := fiber.New()

  adapter, _ := gormadapter.NewAdapter("mysql", "root:@tcp(127.0.0.1:3306)/")
  authz := casbin.New(casbin.Config{
      ModelFilePath: "path/to/rbac_model.conf",
      PolicyAdapter: adapter,
      Lookup: func(c fiber.Ctx) string {
          return "" // fetch authenticated user subject
      },
  })

  app.Post("/blog",
      authz.RequiresPermissions([]string{"blog:create"}, casbin.WithValidationRule(casbin.MatchAllRule)),
      func(c fiber.Ctx) error {
        // your handler
      },
  )

  app.Delete("/blog/:id",
    authz.RequiresPermissions([]string{"blog:create", "blog:delete"}, casbin.WithValidationRule(casbin.AtLeastOneRule)),
    func(c fiber.Ctx) error {
      // your handler
    },
  )

  app.Listen(":8080")
}
```

## RoutePermission

Unless `fiber.Config.CaseSensitive` and `StrictRouting` are set, the router matches routes on the lower-cased path without trailing slashes, while route params are cut from the path without trailing slashes in the client's letter case. `RoutePermission` enforces the request path and each of these forms that differs from it, so write path objects lower-case and without trailing slashes. Unless `DisableHeadAutoRegister` is set, the router answers `HEAD` with the handlers of `GET` routes, so a `HEAD` request needs `GET` allowed as well, even on a route with its own `HEAD` handler. Subjects that only send `HEAD`, such as health checks, need the `GET` rule too, or set `DisableHeadAutoRegister`.

```go
package main

import (
  "github.com/gofiber/fiber/v3"
  "github.com/gofiber/contrib/v3/casbin/v2"
  _ "github.com/go-sql-driver/mysql"
  gormadapter "github.com/casbin/gorm-adapter/v3"
)

func main() {
  app := fiber.New()

  adapter, _ := gormadapter.NewAdapter("mysql", "root:@tcp(127.0.0.1:3306)/")
  authz := casbin.New(casbin.Config{
      ModelFilePath: "path/to/rbac_model.conf",
      PolicyAdapter: adapter,
      Lookup: func(c fiber.Ctx) string {
          return "" // fetch authenticated user subject
      },
  })

  // check permission with Method and Path
  app.Post("/blog",
    authz.RoutePermission(),
    func(c fiber.Ctx) error {
      // your handler
    },
  )

  app.Listen(":8080")
}
```

If the model matches roles by pattern with a matching function on `g` or `g2`, such as `util.KeyMatch2`, the request path or subject reaches that function. Register one that cannot panic:

```go
enforcer.AddNamedMatchingFunc("g2", "KeyMatch2", func(a, b string) (ok bool) {
  defer func() {
    if recover() != nil {
      ok = false
    }
  }()
  return util.KeyMatch2(a, b)
})
```

## RoleAuthorization

```go
package main

import (
  "github.com/gofiber/fiber/v3"
  "github.com/gofiber/contrib/v3/casbin/v2"
  _ "github.com/go-sql-driver/mysql"
  gormadapter "github.com/casbin/gorm-adapter/v3"
)

func main() {
  app := fiber.New()

  adapter, _ := gormadapter.NewAdapter("mysql", "root:@tcp(127.0.0.1:3306)/")
  authz := casbin.New(casbin.Config{
      ModelFilePath: "path/to/rbac_model.conf",
      PolicyAdapter: adapter,
      Lookup: func(c fiber.Ctx) string {
          return "" // fetch authenticated user subject
      },
  })

  app.Put("/blog/:id",
    authz.RequiresRoles([]string{"admin"}),
    func(c fiber.Ctx) error {
      // your handler
    },
  )

  app.Listen(":8080")
}
```
