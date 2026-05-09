---
id: socketio
---

# Socket.io

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*socketio*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20Socket.io/badge.svg)

WebSocket wrapper for [Fiber](https://github.com/gofiber/fiber) that implements the [Engine.IO v4](https://github.com/socketio/engine.io-protocol) / [Socket.IO v5](https://github.com/socketio/socket.io-protocol) wire protocol, making it fully compatible with the official [`socket.io-client`](https://socket.io/docs/v4/client-api/) library.

**Compatible with Fiber v3.**

## Features

This middleware implements the full Engine.IO v4 / Socket.IO v5 wire protocol. Highlights:

- **Synchronous handshake.** The Engine.IO OPEN / Socket.IO CONNECT exchange completes before the user `New()` callback returns, so emits issued inside the callback are ordered after the handshake reply.
- **Namespaces and handshake auth.** The negotiated namespace is honoured for inbound and outbound packets; the client's connect-time `auth` payload is exposed via `Websocket.HandshakeAuth()` and `EventPayload.HandshakeAuth`.
- **Inbound acks.** Client-initiated callbacks surface as `EventPayload.HasAck` / `AckID`; reply once with `payload.Ack(args...)`.
- **Outbound acks.** Server-initiated `EmitWithAck`, `EmitWithAckTimeout`, and `EmitWithAckArgs` round-trip a callback id and invoke the supplied callback when the client acks (or on timeout/disconnect).
- **Multi-arg events.** Inbound events expose every argument tuple as `EventPayload.Args [][]byte`; outbound `EmitArgs` / `EmitWithAckArgs` send pre-encoded JSON tuples.
- **Deterministic heartbeat.** Server PINGs every `PingInterval`; the connection is torn down if no PONG arrives within `PingTimeout`.
- **EIO 0x1E batched frames.** Multi-packet WebSocket frames separated by ASCII RS (`0x1E`) are parsed correctly, with a hard cap (`MaxBatchPackets`) to prevent slice-header amplification.
- **Reserved-event-name guard.** User code cannot register or emit names reserved by the protocol (e.g. `connect`, `disconnect`).
- **EIO version validation.** Handshakes that advertise an unsupported `EIO` version are rejected.
- **Auth payload validation.** The auth blob must be valid JSON and is bounded by `MaxAuthPayload`; oversize or malformed payloads are answered with CONNECT_ERROR.
- **DoS hardening.** `MaxPayload`, `MaxBatchPackets`, `MaxEventNameLength`, and `MaxAuthPayload` bound every attacker-controlled length.
- **Lock-free listener registry** plus `atomic.Bool isAlive`, removing the per-event mutex from the hot path.
- **Optional drop-frames-on-overflow.** When `DropFramesOnOverflow` is true, a saturated send queue drops the offending frame and fires `EventError` instead of tearing down the connection.
- **Graceful drain.** The package-level `Shutdown(ctx)` closes every active socket and waits for each worker to exit (or until `ctx` is cancelled).

## Known limitations

- **One namespace per Engine.IO connection.** Each WebSocket binds the namespace negotiated during the SIO CONNECT packet; multiplexing several namespaces over one EIO connection is not supported.
- **No BINARY_EVENT (5) / BINARY_ACK (6).** Binary Socket.IO frames are passed through as raw `EventMessage` data; attachment reassembly is not implemented.
- **No connection-state recovery.** Resume-on-reconnect (Socket.IO's `connectionStateRecovery` feature) is not implemented; reconnects always start a fresh session.

## Configuration

All tunables are package-level variables; override before the first connection is accepted.

| Variable               | Default            | Meaning                                                                       |
|:-----------------------|:-------------------|:------------------------------------------------------------------------------|
| `PingInterval`         | `25s`              | How often the server emits Engine.IO PING.                                    |
| `PingTimeout`          | `20s`              | Grace window for the client PONG before the connection is killed.             |
| `HandshakeTimeout`     | `10s`              | Hard deadline for completing EIO OPEN + SIO CONNECT.                          |
| `MaxPayload`           | `1_000_000` (1 MB) | Max bytes per inbound WebSocket frame; advertised to the client.              |
| `MaxAuthPayload`       | `8 KiB`            | Max bytes for the SIO CONNECT auth JSON.                                      |
| `MaxBatchPackets`      | `256`              | Max EIO packets in a single `0x1E`-batched frame.                             |
| `MaxEventNameLength`   | `256`              | Max length of an inbound SIO event name.                                      |
| `OutboundAckTimeout`   | `30s`              | Default ack deadline for `EmitWithAck`.                                       |
| `SendQueueSize`        | `100`              | Capacity of the per-connection outbound queue.                                |
| `DropFramesOnOverflow` | `false`            | If true, drop the offending frame on overflow (fires `EventError`).           |
| `RetrySendTimeout`     | `20ms`             | Back-off between send retries.                                                |
| `MaxSendRetry`         | `5`                | Max send retries before a frame is dropped.                                   |
| `ReadTimeout`          | `10ms`             | Idle pause inside the read loop.                                              |

Use `socketio.Shutdown(ctx)` from `fiber.App.ShutdownWithContext` for a deterministic drain.

## Go version support

We only support the latest two versions of Go. Visit [https://go.dev/doc/devel/release](https://go.dev/doc/devel/release) for more information.

## Install

```sh
go get -u github.com/gofiber/fiber/v3
go get -u github.com/gofiber/contrib/v3/socketio
```

## Protocol compatibility

The middleware automatically handles the Engine.IO / Socket.IO handshake so you do **not** need any special server-side code; just point your `socket.io-client` at the WebSocket endpoint.

### Required client configuration

The client **must** use `transports: ['websocket']` (polling transport is not supported):

```js
import { io } from "socket.io-client";

const socket = io("http://localhost:3000", {
  path: "/ws",                 // match the Fiber route
  transports: ["websocket"],   // WebSocket-only; polling is not supported
});
```

### Tunable globals

These package-level variables can be overridden before the first connection is accepted (typically in `init()` or early in `main`). They control timing and limits for the Engine.IO / Socket.IO transport.

| Variable            | Default            | Description                                                                                          |
|:--------------------|:-------------------|:-----------------------------------------------------------------------------------------------------|
| `PingInterval`      | `25 * time.Second` | Interval between Engine.IO PING frames sent by the server to keep the connection alive.              |
| `PingTimeout`       | `20 * time.Second` | How long the server waits for the client's PONG before considering the connection dead.              |
| `HandshakeTimeout`  | `10 * time.Second` | Maximum time allowed for the Engine.IO / Socket.IO handshake (including namespace CONNECT) to complete. |
| `MaxPayload`        | `1 << 20` (1 MiB)  | Maximum size in bytes for a single inbound WebSocket frame; oversize messages close the socket.      |
| `OutboundAckTimeout`| `30 * time.Second` | Default timeout used by `EmitWithAck` when no per-call timeout is supplied.                          |

```go
func init() {
    socketio.PingInterval = 15 * time.Second
    socketio.PingTimeout  = 10 * time.Second
    socketio.MaxPayload   = 4 << 20 // 4 MiB
}
```

### Message format

All messages are exchanged as Socket.IO events.

| Side            | API call                              | Wire format                        |
|:----------------|:--------------------------------------|:-----------------------------------|
| Server → Client | `kws.Emit([]byte(`"hello"`))` | `42["message","hello"]`            |
| Server → Client | `kws.EmitEvent("greet", data)` | `42["greet",<data>]`              |
| Client → Server | `socket.emit("message", obj)` | fires `EventMessage` with `obj` |
| Client → Server | `socket.emit("custom", obj)`  | fires the `"custom"` event     |

> **Note:** `Emit` and `EmitEvent` expect the `data` argument to be **valid JSON** (an object, array, string literal, number, etc.).

### Acks, namespaces, handshake auth

The middleware implements the full Socket.IO v5 ack flow and forwards the client's connect-time auth payload to your handlers.

#### Multi-argument emits

`EmitArgs` and `EmitWithAckArgs` accept a variadic list of already-encoded JSON values, so you can send richer event tuples without manually concatenating arrays:

```go
// 42["greet","hi",{"id":1}]
kws.EmitArgs("greet", []byte(`"hi"`), []byte(`{"id":1}`))
```

#### Server-initiated acks

`EmitWithAck` (and `EmitWithAckTimeout`) emit an event with an ack id and invoke the supplied callback once the client acks, or with an error when the timeout expires. `EmitWithAck` uses `OutboundAckTimeout`; `EmitWithAckTimeout` takes a per-call duration plus a structured `AckCallback` that distinguishes timeout from disconnect.

```go
kws.EmitWithAckTimeout("ping", []byte(`"hello"`), 3*time.Second, func(ack []byte, err error) {
    if err != nil {
        log.Printf("ack failed: %v", err)
        return
    }
    // ack is the raw JSON the client passed to its callback (single value
    // or a JSON-array literal for multi-arg acks).
})
```

#### Client-initiated acks

When the client emits with a callback, the inbound event payload carries an ack id. Use `HasAck` and `AckID` to detect it, then send a single ack reply via `EventPayload.Ack`:

```go
socketio.On("greet", func(ep *socketio.EventPayload) {
    if ep.HasAck {
        // ep.Args holds the raw JSON arguments the client sent.
        _ = ep.Ack([]byte(`"ok"`))
    }
})
```

#### Namespaces

The middleware honours the namespace negotiated during the Socket.IO CONNECT packet. Events emitted from the server are routed back on the same namespace the client joined; no extra configuration is required on the Go side.

#### Handshake auth

Whatever the client passes via `auth` (object, string, etc.) is parsed during the Socket.IO handshake and exposed to handlers as `EventPayload.HandshakeAuth` (raw JSON bytes). It is most commonly inspected on `EventConnect`:

```js
// client
const socket = io("http://localhost:3000", {
  path: "/ws",
  transports: ["websocket"],
  auth: { token: "secret" },
});
```

```go
socketio.On(socketio.EventConnect, func(ep *socketio.EventPayload) {
    // ep.HandshakeAuth == []byte(`{"token":"secret"}`)
    var auth struct{ Token string `json:"token"` }
    _ = json.Unmarshal(ep.HandshakeAuth, &auth)
})
```

## Signatures

```go
// Initialize new socketio in the callback this will
// execute a callback that expects kws *Websocket Object
// and optional config websocket.Config
func New(callback func(kws *Websocket), config ...websocket.Config) func(fiber.Ctx) error
```

```go
// Add listener callback for an event into the listeners list
func On(event string, callback func(payload *EventPayload))
```

```go
// Emit the message to a specific socket uuids list
// Ignores all errors
func EmitToList(uuids []string, message []byte, mType ...int)
```

```go
// Emit to a specific socket connection
func EmitTo(uuid string, message []byte, mType ...int) error
```

```go
// Broadcast to all the active connections
func Broadcast(message []byte, mType ...int)
```

```go
// Fire custom event on all connections
func Fire(event string, data []byte)
```

```go
// Emit a named event with multiple already-encoded JSON arguments
// (e.g. EmitArgs("greet", []byte(`"hi"`), []byte(`{"id":1}`)))
func (kws *Websocket) EmitArgs(event string, args ...[]byte)
```

```go
// Emit a named event and invoke cb when the client acks (or on timeout /
// disconnect). The default deadline is OutboundAckTimeout. The callback
// receives the raw JSON ack value (or nil on timeout/disconnect).
func (kws *Websocket) EmitWithAck(event string, data []byte, cb func(ack []byte))
```

```go
// Like EmitWithAck but with a per-call timeout and a structured AckCallback
// that distinguishes ErrAckTimeout from ErrAckDisconnected. Pass timeout = 0
// to disable the timeout.
func (kws *Websocket) EmitWithAckTimeout(event string, data []byte, timeout time.Duration, cb AckCallback)
```

```go
// Multi-argument variant of EmitWithAck. The callback receives the slice of
// raw-JSON ack arguments the client supplied (or an error on timeout /
// disconnect). Uses OutboundAckTimeout.
func (kws *Websocket) EmitWithAckArgs(event string, args [][]byte, cb func([][]byte, error))
```

```go
// HandshakeAuth returns the raw JSON auth payload sent by the client at
// connect time (nil if the client did not provide one).
func (kws *Websocket) HandshakeAuth() json.RawMessage
```

```go
// Ack sends a Socket.IO ACK frame back to the client for the inbound event
// represented by this payload. Idempotent: only the first invocation
// produces a wire frame; later calls return ErrAckAlreadySent.
func (ep *EventPayload) Ack(args ...[]byte) error
```

## Example

### Go server

```go
package main

import (
    "encoding/json"
    "fmt"
    "log"

    "github.com/gofiber/contrib/v3/socketio"
    "github.com/gofiber/contrib/v3/websocket"
    "github.com/gofiber/fiber/v3"
)

// MessageObject Basic chat message object
type MessageObject struct {
    Data  string `json:"data"`
    From  string `json:"from"`
    Event string `json:"event"`
    To    string `json:"to"`
}

func main() {

    // The key for the map is message.to
    clients := make(map[string]string)

    // Start a new Fiber application
    app := fiber.New()

    // Setup the middleware to retrieve the data sent in first GET request
    app.Use(func(c fiber.Ctx) error {
        // IsWebSocketUpgrade returns true if the client
        // requested upgrade to the WebSocket protocol.
        if websocket.IsWebSocketUpgrade(c) {
            c.Locals("allowed", true)
            return c.Next()
        }
        return fiber.ErrUpgradeRequired
    })

    // Multiple event handling supported
    socketio.On(socketio.EventConnect, func(ep *socketio.EventPayload) {
        fmt.Printf("Connection event 1 - User: %s", ep.Kws.GetStringAttribute("user_id"))
    })

    // Custom event handling supported
    socketio.On("CUSTOM_EVENT", func(ep *socketio.EventPayload) {
        fmt.Printf("Custom event - User: %s", ep.Kws.GetStringAttribute("user_id"))
        // --->

        // DO YOUR BUSINESS HERE

        // --->
    })

    // On message event
    socketio.On(socketio.EventMessage, func(ep *socketio.EventPayload) {

        fmt.Printf("Message event - User: %s - Message: %s", ep.Kws.GetStringAttribute("user_id"), string(ep.Data))

        message := MessageObject{}

        // Unmarshal the json message
        // {
        //  "from": "<user-id>",
        //  "to": "<recipient-user-id>",
        //  "event": "CUSTOM_EVENT",
        //  "data": "hello"
        //}
        err := json.Unmarshal(ep.Data, &message)
        if err != nil {
            fmt.Println(err)
            return
        }

        // Fire custom event based on some
        // business logic
        if message.Event != "" {
            ep.Kws.Fire(message.Event, []byte(message.Data))
        }

        // Emit the message directly to specified user
        err = ep.Kws.EmitTo(clients[message.To], ep.Data, socketio.TextMessage)
        if err != nil {
            fmt.Println(err)
        }
    })

    // On disconnect event
    socketio.On(socketio.EventDisconnect, func(ep *socketio.EventPayload) {
        // Remove the user from the local clients
        delete(clients, ep.Kws.GetStringAttribute("user_id"))
        fmt.Printf("Disconnection event - User: %s", ep.Kws.GetStringAttribute("user_id"))
    })

    // On close event
    // This event is called when the server disconnects the user actively with .Close() method
    socketio.On(socketio.EventClose, func(ep *socketio.EventPayload) {
        // Remove the user from the local clients
        delete(clients, ep.Kws.GetStringAttribute("user_id"))
        fmt.Printf("Close event - User: %s", ep.Kws.GetStringAttribute("user_id"))
    })

    // On error event
    socketio.On(socketio.EventError, func(ep *socketio.EventPayload) {
        fmt.Printf("Error event - User: %s", ep.Kws.GetStringAttribute("user_id"))
    })

    app.Get("/ws/:id", socketio.New(func(kws *socketio.Websocket) {

        // Retrieve the user id from endpoint
        userId := kws.Params("id")

        // Add the connection to the list of the connected clients
        // The UUID is generated randomly and is the key that allow
        // socketio to manage Emit/EmitTo/Broadcast
        clients[userId] = kws.UUID

        // Every websocket connection has an optional session key => value storage
        kws.SetAttribute("user_id", userId)

        // Broadcast to all the connected users the newcomer
        newUserMsg, _ := json.Marshal(fmt.Sprintf("New user connected: %s and UUID: %s", userId, kws.UUID))
        kws.Broadcast(newUserMsg, true, socketio.TextMessage)

        // Write welcome message (must be valid JSON)
        welcomeMsg, _ := json.Marshal(fmt.Sprintf("Hello user: %s with UUID: %s", userId, kws.UUID))
        kws.Emit(welcomeMsg, socketio.TextMessage)
    }))

    log.Fatal(app.Listen(":3000"))
}
```

### TypeScript / JavaScript client

```ts
import { io } from "socket.io-client";

const socket = io("http://localhost:3000", {
  path: "/ws",
  transports: ["websocket"],
});

socket.on("connect", () => {
  console.log("connected, sid =", socket.id);

  // Send a message to the server
  socket.emit("message", {
    from: "user1",
    to:   "user2",
    event: "",
    data: "hello",
  });
});

socket.on("message", (data: unknown) => {
  console.log("received message:", data);
});

socket.on("disconnect", (reason) => {
  console.log("disconnected:", reason);
});
```

---

## Supported events

| Const           | Event        | Description                                                                                                                                                |
|:----------------|:-------------|:-----------------------------------------------------------------------------------------------------------------------------------------------------------|
| EventMessage    | `message`    | Fired when a `socket.emit("message", …)` event is received from the client                                                                                |
| EventPing       | `ping`       | Fired when an Engine.IO PING is received from the client                                                                                                   |
| EventPong       | `pong`       | Fired when the client replies with an Engine.IO PONG to the server's heartbeat                                                                             |
| EventDisconnect | `disconnect` | Fired on disconnection. The error provided in disconnection event as defined in RFC 6455, section 11.7.                                                    |
| EventConnect    | `connect`    | Fired after the Engine.IO / Socket.IO handshake completes; `ep.HandshakeAuth` is populated with the client's `auth` payload (raw JSON, nil if not provided) |
| EventClose      | `close`      | Fired when the connection is actively closed from the server. Different from client disconnection                                                          |
| EventError      | `error`      | Fired when some error appears useful also for debugging websockets                                                                                         |

Custom events map directly to the event name used in `socket.emit("myEvent", …)` on the client and `kws.EmitEvent("myEvent", data)` on the server.

## Event Payload object

| Variable         | Type                | Description                                                                                       |
|:-----------------|:--------------------|:--------------------------------------------------------------------------------------------------|
| Kws              | `*Websocket`        | The connection object                                                                             |
| Name             | `string`            | The name of the event                                                                             |
| SocketUUID       | `string`            | Unique connection UUID                                                                            |
| SocketAttributes | `map[string]any`    | Optional websocket attributes                                                                     |
| Error            | `error`             | (optional) Fired from disconnection or error events                                               |
| Data             | `[]byte`            | Raw JSON of the event payload (first argument of `socket.emit`)                                   |
| Args             | `[][]byte`          | All raw JSON arguments after the event name; useful when the client emits multiple values         |
| AckID            | `uint64`            | Ack id assigned by the client when it emitted with a callback (0 if `HasAck` is false)            |
| HasAck           | `bool`              | True when the inbound event expects an ack reply; respond via `EventPayload.Ack(args...)`         |
| HandshakeAuth    | `json.RawMessage`   | Raw JSON auth payload from the Socket.IO handshake; populated on `EventConnect` listeners (use `Kws.HandshakeAuth()` elsewhere) |

## Socket instance functions

| Name                | Type               | Description                                                                                  |
|:--------------------|:-------------------|:---------------------------------------------------------------------------------------------|
| SetAttribute        | `void`             | Set a specific attribute for the specific socket connection                                  |
| GetUUID             | `string`           | Get socket connection UUID                                                                   |
| SetUUID             | `error`            | Set socket connection UUID                                                                   |
| GetAttribute        | `string`           | Get a specific attribute from the socket attributes                                          |
| EmitToList          | `void`             | Emit the message to a specific socket uuids list                                             |
| EmitTo              | `error`            | Emit to a specific socket connection                                                         |
| Broadcast           | `void`             | Broadcast to all the active connections except broadcasting the message to itself            |
| Fire                | `void`             | Fire custom event                                                                            |
| Emit                | `void`             | Send data as a `"message"` socket.io event (data must be valid JSON)                         |
| EmitEvent           | `void`             | Send a named socket.io event (data must be valid JSON)                                       |
| EmitArgs            | `void`             | Emit a named event with multiple already-encoded JSON arguments                              |
| EmitWithAck         | `void`             | Emit an event and invoke `cb(ack)` when the client acks (uses `OutboundAckTimeout`)          |
| EmitWithAckTimeout  | `void`             | Like `EmitWithAck` but with a per-call timeout and a structured `AckCallback`                |
| EmitWithAckArgs     | `void`             | Multi-arg variant; `cb([][]byte, error)` receives the ack tuple (uses `OutboundAckTimeout`)  |
| HandshakeAuth       | `json.RawMessage`  | Raw JSON auth payload sent by the client at connect time (nil if absent)                     |
| IsAlive             | `bool`             | Reports whether the underlying connection is still open and the heartbeat loop is running    |
| Close               | `void`             | Actively close the connection from the server                                                |

**Note: the FastHTTP connection can be accessed directly from the instance**

```go
kws.Conn
```

