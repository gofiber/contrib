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

## Configuration

Set these package-level variables before accepting connections.

| Variable           | Default | Description |
|:-------------------|:--------|:------------|
| `PongTimeout`      | `1s`    | Interval for server-sent WebSocket pong frames. |
| `RetrySendTimeout` | `20ms`  | Backoff between retries while the connection is not ready. |
| `MaxSendRetry`     | `5`     | Max retries for transient socket write readiness issues. |
| `SendQueueSize`    | `100`   | Per-connection outbound message queue capacity. |
| `ReadTimeout`      | `10ms`  | Deprecated; reads now block until a frame arrives or the connection closes. |

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
