---
id: websocket-event
---

# WebSocket Event

Plain WebSocket event helper for [Fiber](https://github.com/gofiber/fiber), built on top of `github.com/gofiber/contrib/v3/websocket`.

This package is for applications that want the legacy event-bus behavior over ordinary WebSocket clients. It does not implement the Engine.IO or Socket.IO protocol. Use `github.com/gofiber/contrib/v3/socketio` when you need compatibility with the official `socket.io-client` package.

If your application used the older `socketio` package as a plain WebSocket event bus, migrate the import to `github.com/gofiber/contrib/v3/websocket/event`. The API intentionally stays close to that legacy event helper, while the `socketio` package is reserved for the Socket.IO protocol.

**Compatible with Fiber v3.**

## Install

```sh
go get -u github.com/gofiber/contrib/v3/websocket
```

The event helper is the `event` subpackage of that module:

```go
import "github.com/gofiber/contrib/v3/websocket/event"
```

## Signatures

Create a handler:

```go
func New(callback func(kws *event.Websocket), config ...websocket.Config) fiber.Handler
func NewWithConfig(callback func(kws *event.Websocket), eventCfg event.Config, wsConfig ...websocket.Config) fiber.Handler
```

Register listeners. Package-level `On` is process-global; the `(*Websocket)` method form is scoped to a single connection (see [Listeners](#listeners)):

```go
type EventCallback func(payload *event.EventPayload)

func On(name string, callback EventCallback)   // global: fires for every connection
func Off(name string)                          // removes global listeners for name

func (kws *Websocket) On(name string, callback EventCallback) // this connection only
func (kws *Websocket) Off(name string)                        // remove this connection's listeners
```

Send messages. The `(*Websocket)` method forms operate on / from a specific connection and fire `EventError` on failure; the package-level forms address connections in the global pool and do not fire `EventError` (see [Sending messages](#sending-messages)):

```go
// Method forms (fire EventError on failure).
func (kws *Websocket) Emit(message []byte, mType ...int)
func (kws *Websocket) EmitTo(uuid string, message []byte, mType ...int) error
func (kws *Websocket) EmitToList(uuids []string, message []byte, mType ...int)
func (kws *Websocket) Broadcast(message []byte, except bool, mType ...int)
func (kws *Websocket) Fire(name string, data []byte)

// Package forms (do not fire EventError).
func EmitTo(uuid string, message []byte, mType ...int) error
func EmitToList(uuids []string, message []byte, mType ...int)
func Broadcast(message []byte, mType ...int)
func Fire(name string, data []byte)
```

Connection identity and attributes:

```go
func (kws *Websocket) GetUUID() string
func (kws *Websocket) SetUUID(uuid string) error // returns ErrorUUIDDuplication on conflict
func (kws *Websocket) SetAttribute(key string, value interface{})
func (kws *Websocket) GetAttribute(key string) interface{}
```

Graceful shutdown:

```go
func Drain()
func IsDraining() bool
func CloseAll(ctx context.Context, code int, reason string) error
```

## Listeners

`On` registers a **process-global** listener: the callback fires for the given
event on **every** connection created by `New` / `NewWithConfig`, regardless of
route or `Config`. Listeners are additive and stay registered until removed with
`Off`. This matches the legacy `socketio` event bus.

```go
event.On(event.EventMessage, func(ep *event.EventPayload) { /* ... */ })
event.Off(event.EventMessage) // remove again, e.g. on reconfiguration or in tests
```

For listeners that should fire for a single connection only, use the
`(*Websocket).On` method (typically from the `New` callback). Per-connection
listeners fire in addition to the global ones and are discarded automatically
when the connection disconnects:

```go
app.Get("/ws/:id", event.New(func(kws *event.Websocket) {
    kws.On(event.EventMessage, func(ep *event.EventPayload) {
        // only fires for this connection
    })
}))
```

## Sending messages

There are two flavors of `EmitTo` / `EmitToList` / `Broadcast`, and the
difference is easy to miss:

| Form | Targets | On failure |
|:-----|:--------|:-----------|
| `(*Websocket).EmitTo` | a UUID in the pool | fires `EventError` on `kws` and returns the error |
| `(*Websocket).EmitToList` | a list of UUIDs | fires `EventError` on `kws` per failed UUID |
| `(*Websocket).Broadcast` | all connections (`except` skips `kws` itself) | fires `EventError` on `kws` per failed UUID |
| `EmitTo` (package) | a UUID in the pool | returns the error, does **not** fire `EventError` |
| `EmitToList` (package) | a list of UUIDs | silently ignores per-UUID errors |
| `Broadcast` (package) | all connections | fire-and-forget, no error feedback |

Use the method forms when you want delivery failures surfaced as `EventError`
events; use the package forms for fire-and-forget fan-out. `Emit` enqueues the
message on the connection's outbound queue, which a dedicated goroutine drains
in order; if the queue is full it blocks until a slot frees up or the connection
closes.

## Configuration

Per-instance tuning via `event.Config` passed to `NewWithConfig`. Zero values
fall back to the matching package-level var, which itself falls back to the
hard default.

| Config field        | Default | Description |
|:--------------------|:--------|:------------|
| `PingInterval`      | `1s`    | Interval between server-originated Ping frames. Must be less than any upstream proxy or load balancer idle timeout. |
| `ReadIdleTimeout`   | `3 * PingInterval` | Maximum silence before the read deadline fires and the connection is disconnected. |
| `WriteTimeout`      | `10s`   | Bounds a single `WriteMessage` / `WriteControl` call. |
| `MaxMessageSize`    | `1 MiB` | Inbound frame size limit. Set to `math.MaxInt64` to opt out. |
| `SendQueueSize`     | `100`   | Per-connection outbound message queue capacity. |
| `MaxSendRetry`      | `5`     | Max retries for transient socket write readiness issues. |
| `RetrySendTimeout`  | `20ms`  | Backoff between retries while the connection is not ready. |
| `RecoverHandler`    | `nil`   | Called on a panic inside a user `On` callback. If `nil`, panics are recovered silently. |

The legacy package-level vars (`PongTimeout`, `RetrySendTimeout`,
`MaxSendRetry`, `SendQueueSize`, `ReadTimeout`) are still read once per
connection at upgrade time for backwards compatibility, but mutating them
after a connection is established has no effect on running goroutines.
Prefer `NewWithConfig` for new code.

## Thread safety

`On`, `Off`, `Fire`, the package-level `EmitTo` / `EmitToList` / `Broadcast`,
and the connection pool are safe for concurrent use. A single `*Websocket` is
safe to use from multiple goroutines: reads and writes are serialized through a
per-connection mutex and an outbound send queue. Listener callbacks run on the
helper's goroutines, so a callback that blocks holds up event delivery for that
connection; offload long work to your own goroutine.

## Graceful Shutdown

The helper keeps an in-process pool of active connections. Use `event.Drain`
and `event.CloseAll` together with a Fiber shutdown hook so clients receive a
clean `1001 Going Away` close frame instead of an abrupt TCP reset:

```go
app.Hooks().OnShutdown(func() error {
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    event.Drain()
    return event.CloseAll(ctx, websocket.CloseGoingAway, "server shutting down")
})
```

If `ctx` expires before every goroutine exits, `CloseAll` force-closes the
remaining underlying connections and returns `ctx.Err()`.

`Drain` only flips the draining flag; it does **not** refuse new connections by
itself. Gate the upgrade route on `IsDraining` to stop accepting clients during
shutdown:

```go
app.Use("/ws", func(c fiber.Ctx) error {
    if event.IsDraining() {
        return fiber.NewError(fiber.StatusServiceUnavailable, "shutting down")
    }
    if websocket.IsWebSocketUpgrade(c) {
        return c.Next()
    }
    return fiber.ErrUpgradeRequired
})
```

## Example

```go
package main

import (
	"log"

	"github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/contrib/v3/websocket/event"
	"github.com/gofiber/fiber/v3"
)

func main() {
	app := fiber.New()

	app.Use("/ws", func(c fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})

	event.On(event.EventMessage, func(ep *event.EventPayload) {
		ep.Kws.Emit([]byte("echo: "+string(ep.Data)), event.TextMessage)
	})

	app.Get("/ws/:id", event.New(func(kws *event.Websocket) {
		// kws.Params / kws.Locals / kws.Query / kws.Cookies wrap the Fiber
		// request context captured at upgrade time.
		kws.SetAttribute("user_id", kws.Params("id"))
	}))

	log.Fatal(app.Listen(":3000"))
}
```

### Custom events

Event names are arbitrary strings. Register a listener with `On` and trigger it
with `Fire` (on one connection) or the package-level `Fire` (on all):

```go
event.On("notify", func(ep *event.EventPayload) {
    log.Printf("notify %s: %s", ep.SocketUUID, ep.Data)
})

// from a connection:
kws.Fire("notify", []byte("hello"))

// to every active connection:
event.Fire("notify", []byte("broadcast"))
```

## Supported Events

| Const             | Event        | Description                                             |
|:------------------|:-------------|:--------------------------------------------------------|
| `EventMessage`    | `message`    | Fired when a text or binary message is received.        |
| `EventPing`       | `ping`       | Fired when a WebSocket ping control frame is received.  |
| `EventPong`       | `pong`       | Fired when a WebSocket pong control frame is received.  |
| `EventDisconnect` | `disconnect` | Fired when the connection is closed. On an error close, `EventError` fires too. |
| `EventConnect`    | `connect`    | Fired after the `New` callback runs, before the read loop starts. |
| `EventClose`      | `close`      | Fired when the server actively closes the connection.   |
| `EventError`      | `error`      | Fired on a failed `EmitTo`, a dropped outbound message, or an error-driven disconnect. |

## Event Payload

| Field              | Type                   | Description                                      |
|:-------------------|:-----------------------|:-------------------------------------------------|
| `Kws`              | `*event.Websocket`     | The connection object.                           |
| `Name`             | `string`               | The event name.                                  |
| `SocketUUID`       | `string`               | Unique connection UUID.                          |
| `SocketAttributes` | `map[string]any`       | Snapshot of optional connection attributes.      |
| `Error`            | `error`                | Optional error for disconnect and error events.  |
| `Data`             | `[]byte`               | Data used on message, custom, and error events.  |
