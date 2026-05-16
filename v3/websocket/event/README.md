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

## Signatures

```go
func New(callback func(kws *event.Websocket), config ...websocket.Config) fiber.Handler
func NewWithConfig(callback func(kws *event.Websocket), eventCfg event.Config, wsConfig ...websocket.Config) fiber.Handler
```

```go
func On(name string, callback func(payload *event.EventPayload))
```

```go
func EmitTo(uuid string, message []byte, mType ...int) error
func EmitToList(uuids []string, message []byte, mType ...int)
func Broadcast(message []byte, mType ...int)
func Fire(name string, data []byte)
```

```go
func Drain()
func IsDraining() bool
func CloseAll(ctx context.Context, code int, reason string) error
```

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
		ep.Kws.Emit([]byte("echo: " + string(ep.Data)), event.TextMessage)
	})

	app.Get("/ws/:id", event.New(func(kws *event.Websocket) {
		kws.SetAttribute("user_id", kws.Params("id"))
	}))

	log.Fatal(app.Listen(":3000"))
}
```

## Supported Events

| Const             | Event        | Description                                             |
|:------------------|:-------------|:--------------------------------------------------------|
| `EventMessage`    | `message`    | Fired when a text or binary message is received.        |
| `EventPing`       | `ping`       | Fired when a WebSocket ping control frame is received.  |
| `EventPong`       | `pong`       | Fired when a WebSocket pong control frame is received.  |
| `EventDisconnect` | `disconnect` | Fired when the connection is closed.                    |
| `EventConnect`    | `connect`    | Fired when the connection is initialized.               |
| `EventClose`      | `close`      | Fired when the server actively closes the connection.   |
| `EventError`      | `error`      | Fired when an error occurs.                             |

## Event Payload

| Field              | Type                   | Description                                      |
|:-------------------|:-----------------------|:-------------------------------------------------|
| `Kws`              | `*event.Websocket`     | The connection object.                           |
| `Name`             | `string`               | The event name.                                  |
| `SocketUUID`       | `string`               | Unique connection UUID.                          |
| `SocketAttributes` | `map[string]any`       | Snapshot of optional connection attributes.      |
| `Error`            | `error`                | Optional error for disconnect and error events.  |
| `Data`             | `[]byte`               | Data used on message, custom, and error events.  |
