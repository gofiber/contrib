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
go get -u github.com/gofiber/storage/redis/v3
```

## Signature

```go
uptime.New(config ...uptime.Config) fiber.Handler
```

## Basic usage

```go
package main

import (
	"github.com/gofiber/contrib/v3/uptime"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
	fiberredis "github.com/gofiber/storage/redis/v3"
)

func main() {
	app := fiber.New()
	store := fiberredis.New()
	app.Hooks().OnPreShutdown(store.Close)

	app.Use(uptime.New(uptime.Config{
		App:         app,
		Store:       store,
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

## Endpoint probes

By default, uptime records a heartbeat for the current process identified by
`ServiceID`. You can also configure HTTP endpoints. Each endpoint is shown as a
separate service on the dashboard and in the JSON API.

```go
app.Use(uptime.New(uptime.Config{
	App:   app,
	Store: store,
	Endpoints: []uptime.EndpointConfig{
		{
			ID:                  "api-health",
			Name:                "API Health",
			URL:                 "https://api.example.com/health",
			Method:              "GET",
			Interval:            10 * time.Second,
			Timeout:             3 * time.Second,
			ExpectedStatusCodes: []int{200, 204},
		},
	},
}))
```

When `ExpectedStatusCodes` is empty, any `2xx` or `3xx` response is considered
up. Failed probes do not write heartbeat slots, so the endpoint naturally moves
to yellow, red, or down as slots are missed.

## Redis

Redis state is stored through `github.com/gofiber/storage/redis/v3`. Create the
Fiber Redis storage in your app and pass it to the uptime config. By default,
that storage package connects to `127.0.0.1:6379` and database `0`.

```go
store := fiberredis.New(fiberredis.Config{
	Addrs:    []string{"127.0.0.1:6379"},
	Password: "",
	Database: 0,
})
app.Hooks().OnPreShutdown(store.Close)

app.Use(uptime.New(uptime.Config{
	App:              app,
	Store:            store,
	ServiceID:        "api",
	StorageKeyPrefix: "fiber:uptime",
}))
```

The middleware uses `Conn()` from the Fiber Redis storage to run the uptime
queries it needs. Use a dedicated Redis database or a distinct
`StorageKeyPrefix` when multiple environments share the same Redis server. The
caller owns the Redis storage lifecycle.

## Snapshots and custom UI

The dashboard and JSON API build a fresh `Snapshot` from the backing store on
each request. Use Fiber's cache middleware around the uptime route if you want
HTTP-level caching. The same snapshot payload is available at
`UI.Path + "/api/status"` for custom dashboards.

## Config

| Property | Type | Description | Default |
|:--|:--|:--|:--|
| App | `*fiber.App` | Fiber app used to register the shutdown hook that closes the uptime runtime. | Required |
| Next | `func(fiber.Ctx) bool` | Skip the uptime handler when true. | `nil` |
| ServiceID | `string` | Stable service identifier for the current process. Required only when `Endpoints` is empty. | `""` |
| ServiceName | `string` | Display name. | `ServiceID` |
| ServiceDescription | `string` | Display description. | `""` |
| Endpoints | `[]uptime.EndpointConfig` | Optional HTTP endpoints to probe as tracked services. | `nil` |
| SampleInterval | `time.Duration` | Heartbeat interval. | `3 * time.Second` |
| RetentionDays | `int` | Number of days to retain daily history. | `90` |
| DaysToShow | `int` | Number of days shown in snapshots and dashboard. | `30` |
| Timezone | `*time.Location` | Timezone for day and slot boundaries. | `time.Local` |
| NodeID | `int64` | Optional node value used for generated instance IDs. | `0` |
| InstanceID | `int64` | Explicit process instance ID. | Generated |
| IDGenerator | `uptime.IDGenerator` | Custom instance ID generator. | `nil` |
| Store | `*fiberredis.Storage` | Fiber Redis storage instance from `github.com/gofiber/storage/redis/v3`. | Required |
| StorageKeyPrefix | `string` | Prefix for all uptime Redis keys. | `"fiber:uptime"` |
| UI | `uptime.UIConfig` | Dashboard copy and thresholds. | Light English UI, green at `99.9%`, yellow at `99%` |

### EndpointConfig

| Property | Type | Description | Default |
|:--|:--|:--|:--|
| ID | `string` | Stable endpoint identifier. | Required |
| Name | `string` | Display name. | `ID` |
| Description | `string` | Display description. | `""` |
| URL | `string` | Absolute `http` or `https` URL to probe. | Required |
| Method | `string` | HTTP method used for the probe. | `GET` |
| Headers | `map[string]string` | Optional request headers sent with each probe. | `nil` |
| ExpectedStatusCodes | `[]int` | Status codes that mark the endpoint up. Empty means any `2xx` or `3xx`. | `nil` |
| Interval | `time.Duration` | Endpoint heartbeat interval. | `Config.SampleInterval` |
| Timeout | `time.Duration` | Maximum duration for one probe. | `5 * time.Second` |

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
business handlers. Process heartbeats and endpoint probes are run by background
tickers owned by the uptime runtime.

## Performance notes

Store writes and endpoint probes happen on background tickers, not on every
business request. Status requests read the backing store to build the response.
Use Fiber's cache middleware if the dashboard or JSON API should be cached.

## Concurrency safety

Uptime middleware instances are safe for concurrent use after construction. The snapshot
payload is built from fresh store reads. Redis commands are issued through the
Fiber Redis storage connection and are safe for concurrent use by the background
recorder and Fiber handlers.

`Config.App` is required so `New` can register a Fiber shutdown hook that stops
the uptime runtime. The caller owns the Redis storage lifecycle and should close
it from an app hook or signal handler.

## Security notes

Mount the dashboard on an internal or protected route when uptime history should
not be public. The middleware does not log request bodies, response bodies,
authorization headers, cookies, or query strings. Endpoint probe response bodies
are closed without being read. Avoid putting secrets in endpoint URLs because
URLs may still appear in upstream infrastructure logs outside this middleware.
