---
id: uptime
---

# Uptime

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*uptime*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20Uptime/badge.svg)

Uptime middleware for [Fiber](https://github.com/gofiber/fiber) that records
in-process heartbeat history and serves a lightweight status page.

**Compatible with Fiber v3.**

## Preview

![Uptime dashboard preview](https://raw.githubusercontent.com/gofurry/images/refs/heads/main/github/uptime/preview.png)

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/uptime
```

## Signature

```go
uptime.New(config ...uptime.Config) fiber.Handler
uptime.NewRuntime(config ...uptime.Config) (*uptime.Uptime, error)
up.Handler() fiber.Handler
up.Close() error
up.LastError() (time.Time, error)
up.Snapshot(ctx context.Context) (uptime.Snapshot, error)
```

## Basic usage

```go
package main

import (
	"github.com/gofiber/contrib/v3/uptime"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
)

func main() {
	app := fiber.New()

	app.Use(uptime.New(uptime.Config{
		App:         app,
		ServiceID:   "api",
		ServiceName: "API",
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	log.Fatal(app.Listen(":3000"))
}
```

Open:

- `http://localhost:3000/uptime`
- `http://localhost:3000/uptime/api/status`

## Redis

Redis is the built-in store and is created through
`github.com/gofiber/storage/redis/v3`. By default, that storage package connects
to `127.0.0.1:6379` and database `0`.

```go
app.Use(uptime.New(uptime.Config{
	App:       app,
	ServiceID: "api",
	Redis: uptime.RedisConfig{
		Addrs:    []string{"127.0.0.1:6379"},
		Password: "",
		Database: 0,
	},
	StorageKeyPrefix: "fiber:uptime",
}))
```

The middleware uses `Conn()` from the Fiber Redis storage to run the uptime
queries it needs. Use a dedicated Redis database or a distinct
`StorageKeyPrefix` when multiple environments share the same Redis server.

## Snapshots and custom UI

The dashboard and JSON API build a fresh `Snapshot` from the backing store on
each request. Use Fiber's cache middleware around the uptime route if you want
HTTP-level caching.

```go
up, err := uptime.NewRuntime(uptime.Config{
	App:       app,
	ServiceID: "api",
})
if err != nil {
	log.Fatal(err)
}
app.Use(up.Handler())

app.Get("/custom-uptime", func(c fiber.Ctx) error {
	snapshot, err := up.Snapshot(c.Context())
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "uptime unavailable")
	}
	return c.JSON(snapshot)
})
```

Use `LastError()` when you need to inspect the latest runtime store error that
was reported by heartbeat, maintenance, or snapshot reads.

## Config

| Property | Type | Description | Default |
|:--|:--|:--|:--|
| App | `*fiber.App` | Optional Fiber app used to register a shutdown hook that closes the uptime runtime. | `nil` |
| Next | `func(fiber.Ctx) bool` | Skip the uptime handler when true. | `nil` |
| ServiceID | `string` | Stable service identifier. | Required |
| ServiceName | `string` | Display name. | `ServiceID` |
| ServiceDescription | `string` | Display description. | `""` |
| SampleInterval | `time.Duration` | Heartbeat interval. | `3 * time.Second` |
| RetentionDays | `int` | Number of days to retain daily history. | `90` |
| DaysToShow | `int` | Number of days shown in snapshots and dashboard. | `30` |
| Timezone | `*time.Location` | Timezone for day and slot boundaries. | `time.Local` |
| NodeID | `int64` | Optional node value used for generated instance IDs. | `0` |
| InstanceID | `int64` | Explicit process instance ID. | Generated |
| IDGenerator | `uptime.IDGenerator` | Custom instance ID generator. | `nil` |
| Redis | `uptime.RedisConfig` | Redis store settings from `github.com/gofiber/storage/redis/v3`. | Fiber Redis storage defaults |
| StorageKeyPrefix | `string` | Prefix for all uptime Redis keys. | `"fiber:uptime"` |
| UI | `uptime.UIConfig` | Dashboard copy and thresholds. | Light English UI, green at `99.9%`, yellow at `99%` |

## Handler behavior

The Fiber handler serves:

- `/uptime`
- `/uptime/`
- `/uptime/api/status`

`GET` and `HEAD` are supported. Other methods return `405 Method Not Allowed`.
Requests outside `UI.Path` are passed to the next handler. Unknown uptime
subpaths return `404 Not Found`.

The handler matches request paths against `UI.Path` (default `/uptime`).

The middleware does not read request bodies, capture response bodies, or wrap
business handlers. Heartbeats are written by a background ticker owned by the
uptime runtime.

## Performance notes

Store writes happen on the heartbeat ticker, not on every business request.
Status requests read the backing store to build the response. Use Fiber's cache
middleware if the dashboard or JSON API should be cached.

## Concurrency safety

Uptime runtimes are safe for concurrent use after construction. The snapshot
payload is built from fresh store reads. Redis commands are issued through the
Fiber Redis storage connection and are safe for concurrent use by the background
recorder and Fiber handlers.

When `Config.App` is set, `New` and `NewRuntime` register a Fiber shutdown hook
that calls `Close`. If you create a runtime without `Config.App`, call `Close`
during application shutdown to stop the heartbeat goroutine and close the store.

## Security notes

Mount the dashboard on an internal or protected route when uptime history should
not be public. The middleware does not log request bodies, response bodies,
authorization headers, cookies, or query strings.
