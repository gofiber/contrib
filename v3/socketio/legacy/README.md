---
id: socketio-legacy
---

# SocketIO Legacy Event Shim

Compatibility shim for applications that used older `socketio` releases as a plain WebSocket event bus.

Deprecated: import `github.com/gofiber/contrib/v3/websocket/event` directly for new code. The root `github.com/gofiber/contrib/v3/socketio` package is reserved for the Engine.IO / Socket.IO protocol and clients such as `socket.io-client`.

## Migration

Preferred import:

```go
import "github.com/gofiber/contrib/v3/websocket/event"
```

Temporary compatibility import:

```go
import "github.com/gofiber/contrib/v3/socketio/legacy"
```

The shim re-exports the old event-bus surface:

```go
legacy.On(legacy.EventMessage, func(ep *legacy.EventPayload) {
	ep.Kws.Emit([]byte("pong"), legacy.TextMessage)
})

app.Get("/ws", legacy.New(func(kws *legacy.Websocket) {}))
```

Set tuning globals such as `PongTimeout`, `SendQueueSize`, or `MaxSendRetry` on `github.com/gofiber/contrib/v3/websocket/event` directly before accepting connections.
