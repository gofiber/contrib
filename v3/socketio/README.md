---
id: socketio
---

# Socket.io

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*socketio*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20Socket.io/badge.svg)

WebSocket wrapper for [Fiber](https://github.com/gofiber/fiber) that implements the [Engine.IO v4](https://github.com/socketio/engine.io-protocol) / [Socket.IO v5](https://github.com/socketio/socket.io-protocol) wire protocol, making it fully compatible with the official [`socket.io-client`](https://socket.io/docs/v4/client-api/) library.

**Compatible with Fiber v3.**

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

`EmitWithAck` (and `EmitWithAckTimeout`) emit an event with an ack id and resolve once the client invokes its callback, or reject when the timeout expires. `EmitWithAck` uses `OutboundAckTimeout`; `EmitWithAckTimeout` takes a per-call duration.

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

args, err := kws.EmitWithAckTimeout(ctx, "ping", 3*time.Second, []byte(`"hello"`))
if err != nil {
    log.Printf("ack failed: %v", err)
    return
}
// args is the JSON array the client passed to its callback.
```

#### Client-initiated acks

When the client emits with a callback, the inbound event payload carries an ack id. Use `HasAck` and `AckID` to detect it, then send a single ack reply via `Kws.SendAck`:

```go
socketio.On("greet", func(ep *socketio.EventPayload) {
    if ep.HasAck {
        // ep.Args holds the raw JSON arguments the client sent.
        ep.Kws.SendAck(ep.AckID, []byte(`"ok"`))
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
func EmitToList(uuids []string, message []byte)
```

```go
// Emit to a specific socket connection
func EmitTo(uuid string, message []byte) error
```

```go
// Broadcast to all the active connections
// except avoid broadcasting the message to itself
func Broadcast(message []byte)
```

```go
// Fire custom event on all connections
func Fire(event string, data []byte) 
```

```go
// Emit a named event with multiple already-encoded JSON arguments
// (e.g. EmitArgs("greet", []byte(`"hi"`), []byte(`{"id":1}`)))
func (kws *Websocket) EmitArgs(event string, args ...[]byte) error
```

```go
// Emit a named event and wait for the client's ack, using OutboundAckTimeout.
// Returns the ack arguments as raw JSON.
func (kws *Websocket) EmitWithAck(ctx context.Context, event string, data []byte) ([]byte, error)
```

```go
// Like EmitWithAck but with a per-call timeout that overrides OutboundAckTimeout.
func (kws *Websocket) EmitWithAckTimeout(ctx context.Context, event string, timeout time.Duration, data []byte) ([]byte, error)
```

```go
// Multi-argument variant of EmitWithAckTimeout.
func (kws *Websocket) EmitWithAckArgs(ctx context.Context, event string, timeout time.Duration, args ...[]byte) ([]byte, error)
```

```go
// HandshakeAuth returns the raw JSON auth payload sent by the client at
// connect time (nil if the client did not provide one).
func (kws *Websocket) HandshakeAuth() []byte
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
| HasAck           | `bool`              | True when the inbound event expects an ack reply; respond via `Kws.SendAck(AckID, ...)`           |
| HandshakeAuth    | `[]byte`            | Raw JSON auth payload from the Socket.IO handshake; populated on `EventConnect` (and later events) |

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
| EmitArgs            | `error`            | Emit a named event with multiple already-encoded JSON arguments                              |
| EmitWithAck         | `([]byte, error)`  | Emit an event and wait for the client's ack (uses `OutboundAckTimeout`)                      |
| EmitWithAckTimeout  | `([]byte, error)`  | Like `EmitWithAck` but with a per-call timeout                                               |
| EmitWithAckArgs     | `([]byte, error)`  | Multi-argument variant of `EmitWithAckTimeout`                                               |
| HandshakeAuth       | `[]byte`           | Raw JSON auth payload sent by the client at connect time (nil if absent)                     |
| IsAlive             | `bool`             | Reports whether the underlying connection is still open and the heartbeat loop is running    |
| Close               | `void`             | Actively close the connection from the server                                                |

**Note: the FastHTTP connection can be accessed directly from the instance**

```go
kws.Conn
```

