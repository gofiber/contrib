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

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/uptime
```

## Signature

```go
uptime.New(config ...uptime.Config) (*uptime.Uptime, error)
up.Handler() fiber.Handler
up.Close() error
up.Snapshot(ctx context.Context) (uptime.Snapshot, error)
up.CachedSnapshot(ctx context.Context) (uptime.Snapshot, error)
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

	up, err := uptime.New(uptime.Config{
		ServiceID:   "api",
		ServiceName: "API",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer up.Close()

	app.All("/uptime", up.Handler())
	app.All("/uptime/*", up.Handler())

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	log.Fatal(app.Listen(":3000"))
}
```

Open:

- `http://localhost:3000/uptime`
- `http://localhost:3000/uptime/api/status`

## SQLite

SQLite is the default store. When `Config.Store` is nil, the middleware creates a
SQLite store at `./data/uptime.db`.

```go
up, err := uptime.New(uptime.Config{
	ServiceID: "api",
	SQLite: uptime.SQLiteConfig{
		Path: "./data/uptime.db",
	},
})
```

SQLite uses the pure-Go `modernc.org/sqlite` driver and configures WAL mode,
normal synchronous mode, a busy timeout, and one open connection.

## PostgreSQL

Use PostgreSQL when multiple processes or machines should share one uptime
store.

```go
up, err := uptime.New(uptime.Config{
	ServiceID: "api",
	Postgres: uptime.PostgresConfig{
		DSN:         "postgres://postgres:password@localhost:5432/postgres?sslmode=disable",
		Schema:      "public",
		TablePrefix: "uptime_",
	},
})
```

The PostgreSQL store creates its schema, tables, and indexes automatically.

## Alert hook

Alerts are optional and disabled by default. Configure `Alert.Hook` to receive
deduplicated service status transitions.

```go
up, err := uptime.New(uptime.Config{
	ServiceID: "api",
	Alert: uptime.AlertConfig{
		NotifyOnFirstDown: true,
		Hook: func(ctx context.Context, event uptime.AlertEvent) error {
			log.Infof("%s changed from %s to %s", event.ServiceID, event.PreviousStatus, event.CurrentStatus)
			return nil
		},
	},
})
```

SQLite and PostgreSQL persist alert state, so shared deployments avoid duplicate
notifications for the same transition.

## Snapshots and custom UI

The dashboard and JSON API use `CachedSnapshot` to avoid querying the store on
every request.

```go
app.Get("/custom-uptime", func(c fiber.Ctx) error {
	snapshot, err := up.CachedSnapshot(c.Context())
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "uptime unavailable")
	}
	return c.JSON(snapshot)
})
```

Use `Snapshot(ctx)` when you need a fresh store read.

## Config

| Property | Type | Description | Default |
|:--|:--|:--|:--|
| Next | `func(fiber.Ctx) bool` | Skip the uptime handler when true. | `nil` |
| ServiceID | `string` | Stable service identifier. | Required |
| ServiceName | `string` | Display name. | `ServiceID` |
| ServiceDescription | `string` | Display description. | `""` |
| SampleInterval | `time.Duration` | Heartbeat interval. | `3 * time.Second` |
| RetentionDays | `int` | Number of days to retain daily history. | `90` |
| DaysToShow | `int` | Number of days shown in snapshots and dashboard. | `90` |
| Timezone | `*time.Location` | Timezone for day and slot boundaries. | `time.Local` |
| NodeID | `int64` | Optional node value used for generated instance IDs. | `0` |
| InstanceID | `int64` | Explicit process instance ID. | Generated |
| IDGenerator | `uptime.IDGenerator` | Custom instance ID generator. | `nil` |
| Store | `uptime.Store` | Custom store implementation. | SQLite |
| SQLite | `uptime.SQLiteConfig` | SQLite store settings. | `Path: "./data/uptime.db"` |
| Postgres | `uptime.PostgresConfig` | PostgreSQL store settings. | Empty |
| Alert | `uptime.AlertConfig` | Optional alert hook settings. | Disabled |
| Snapshot | `uptime.SnapshotConfig` | Snapshot cache settings. | Cache enabled |
| UI | `uptime.UIConfig` | Dashboard copy and thresholds. | Light English UI |

## Handler behavior

The Fiber handler serves:

- `/uptime`
- `/uptime/`
- `/uptime/api/status`

`GET` and `HEAD` are supported. Other methods return `405 Method Not Allowed`.
Unknown uptime subpaths return `404 Not Found`.

The middleware does not read request bodies, capture response bodies, or wrap
business handlers. Heartbeats are written by a background ticker owned by the
`Uptime` instance.

## Performance notes

Request handling reads from the snapshot cache by default. Store writes happen
on the heartbeat ticker, not on every business request. Set
`Snapshot.DisableCache` only when every dashboard or API request must perform a
fresh store read.

## Concurrency safety

`Uptime` instances are safe for concurrent use after construction. The snapshot
cache is protected by a mutex and returns cloned payloads. Store implementations
are designed for concurrent use by the background recorder and Fiber handlers.

Always call `Close` during application shutdown to stop the heartbeat goroutine
and close the store.

## Security notes

Mount the dashboard on an internal or protected route when uptime history should
not be public. The middleware does not log request bodies, response bodies,
authorization headers, cookies, or query strings. Runtime middleware logs use
Fiber's official `github.com/gofiber/fiber/v3/log` package.
