---
id: casbin
---

# Casbin

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=casbin*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Tests/badge.svg)
![Security](https://github.com/gofiber/contrib/workflows/Security/badge.svg)
![Linter](https://github.com/gofiber/contrib/workflows/Linter/badge.svg)

Casbin middleware for Fiber.

**Note: Requires Go 1.18 and above**

## Install
```
go get -u github.com/gofiber/fiber/v2
go get -u github.com/gofiber/contrib/casbin
```
choose an adapter from [here](https://casbin.org/docs/adapters)
```
go get -u github.com/casbin/xorm-adapter
```

## Signature
```go
casbin.New(config ...casbin.Config) *casbin.Middleware
```

## Config

| Property      | Type                      | Description                              | Default                                                             |
|:--------------|:--------------------------|:-----------------------------------------|:--------------------------------------------------------------------|
| ModelFilePath | `string`                  | Model file path                          | `"./model.conf"`                                                    |
| PolicyAdapter | `persist.Adapter`         | Database adapter for policies            | `./policy.csv`                                                      |
| Enforcer      | `*casbin.Enforcer`        | Custom casbin enforcer                   | `Middleware generated enforcer using ModelFilePath & PolicyAdapter` |
| Lookup        | `func(*fiber.Ctx) string` | Look up for current subject              | `""`                                                                |
| Unauthorized  | `func(*fiber.Ctx) error`  | Response body for unauthorized responses | `Unauthorized`                                                      |
| Forbidden     | `func(*fiber.Ctx) error`  | Response body for forbidden responses    | `Forbidden`                                                         |

### Examples
- [Gorm Adapter](https://github.com/svcg/-fiber_casbin_demo)
- [File Adapter](https://github.com/gofiber/contrib/casbin/tree/master/example)

## CustomPermission

```go
package main

import (
  "github.com/gofiber/fiber/v2"
  "github.com/gofiber/contrib/casbin"
  _ "github.com/go-sql-driver/mysql"
  "github.com/casbin/xorm-adapter/v2"
)

func main() {
  app := fiber.New()

  authz := casbin.New(casbin.Config{
      ModelFilePath: "path/to/rbac_model.conf",
      PolicyAdapter: xormadapter.NewAdapter("mysql", "root:@tcp(127.0.0.1:3306)/"),
      Lookup: func(c *fiber.Ctx) string {
          // fetch authenticated user subject
      },
  })

  app.Post("/blog",
      authz.RequiresPermissions([]string{"blog:create"}, casbin.WithValidationRule(casbin.MatchAllRule)),
      func(c *fiber.Ctx) error {
        // your handler
      },
  )
  
  app.Delete("/blog/:id",
    authz.RequiresPermissions([]string{"blog:create", "blog:delete"}, casbin.WithValidationRule(casbin.AtLeastOneRule)),
    func(c *fiber.Ctx) error {
      // your handler
    },
  )

  app.Listen(":8080")
}
```

## RoutePermission

```go
package main

import (
  "github.com/gofiber/fiber/v2"
  "github.com/gofiber/contrib/casbin"
  _ "github.com/go-sql-driver/mysql"
  "github.com/casbin/xorm-adapter/v2"
)

func main() {
  app := fiber.New()

  authz := casbin.New(casbin.Config{
      ModelFilePath: "path/to/rbac_model.conf",
      PolicyAdapter: xormadapter.NewAdapter("mysql", "root:@tcp(127.0.0.1:3306)/"),
      Lookup: func(c *fiber.Ctx) string {
          // fetch authenticated user subject
      },
  })

  // check permission with Method and Path
  app.Post("/blog",
    authz.RoutePermission(),
    func(c *fiber.Ctx) error {
      // your handler
    },
  )

  app.Listen(":8080")
}
```

## RoleAuthorization

```go
package main

import (
  "github.com/gofiber/fiber/v2"
  "github.com/gofiber/contrib/casbin"
  _ "github.com/go-sql-driver/mysql"
  "github.com/casbin/xorm-adapter/v2"
)

func main() {
  app := fiber.New()

  authz := casbin.New(casbin.Config{
      ModelFilePath: "path/to/rbac_model.conf",
      PolicyAdapter: xormadapter.NewAdapter("mysql", "root:@tcp(127.0.0.1:3306)/"),
      Lookup: func(c *fiber.Ctx) string {
          // fetch authenticated user subject
      },
  })
  
  app.Put("/blog/:id",
    authz.RequiresRoles([]string{"admin"}),
    func(c *fiber.Ctx) error {
      // your handler
    },
  )

  app.Listen(":8080")
}
```
