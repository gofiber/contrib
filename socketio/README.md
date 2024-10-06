---
id: socketio
---

# Socket.io

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=socketio*)
![Test](https://github.com/gofiber/contrib/workflows/Tests/badge.svg)
![Security](https://github.com/gofiber/contrib/workflows/Security/badge.svg)
![Linter](https://github.com/gofiber/contrib/workflows/Linter/badge.svg)

WebSocket wrapper for [Fiber](https://github.com/gofiber/fiber) with events support and inspired by [Socket.io](https://github.com/socketio/socket.io)

**Note: Requires Go 1.20 and above**

## Install

```
go get -u github.com/gofiber/fiber/v2
go get -u github.com/gofiber/contrib/socketio
```

## Signatures

```go
// Initialize new socketio in the callback this will
// execute a callback that expects kws *Websocket Object
// and optional config websocket.Config
func New(callback func(kws *Websocket), config ...websocket.Config) func(*fiber.Ctx) error
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

## Example

```go
package main

import (
    "encoding/json"
    "fmt"
    "log"

    "github.com/gofiber/contrib/socketio"
    "github.com/gofiber/contrib/websocket"
    "github.com/gofiber/fiber/v2"
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
    app.Use(func(c *fiber.Ctx) error {
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

        //Broadcast to all the connected users the newcomer
        kws.Broadcast([]byte(fmt.Sprintf("New user connected: %s and UUID: %s", userId, kws.UUID)), true, socketio.TextMessage)
        //Write welcome message
        kws.Emit([]byte(fmt.Sprintf("Hello user: %s with UUID: %s", userId, kws.UUID)), socketio.TextMessage)
    }))

    log.Fatal(app.Listen(":3000"))
}

```

---

## Supported events

| Const           | Event        | Description                                                                                                                                                |
|:----------------|:-------------|:-----------------------------------------------------------------------------------------------------------------------------------------------------------|
| EventMessage    | `message`    | Fired when a Text/Binary message is received                                                                                                               |
| EventPing       | `ping`       | [More details here](https://developer.mozilla.org/en-US/docs/Web/API/WebSockets_API/Writing_WebSocket_servers#Pings_and_Pongs_The_Heartbeat_of_WebSockets) |
| EventPong       | `pong`       | Refer to ping description                                                                                                                                  |
| EventDisconnect | `disconnect` | Fired on disconnection. The error provided in disconnection event as defined in RFC 6455, section 11.7.                                                    |
| EventConnect    | `connect`    | Fired on first connection                                                                                                                                  |
| EventClose      | `close`      | Fired when the connection is actively closed from the server. Different from client disconnection                                                          |
| EventError      | `error`      | Fired when some error appears useful also for debugging websockets                                                                                         |

## Event Payload object

| Variable         | Type                | Description                                                                     |
|:-----------------|:--------------------|:--------------------------------------------------------------------------------|
| Kws              | `*Websocket`        | The connection object                                                           |
| Name             | `string`            | The name of the event                                                           |
| SocketUUID       | `string`            | Unique connection UUID                                                          |
| SocketAttributes | `map[string]string` | Optional websocket attributes                                                   |
| Error            | `error`             | (optional) Fired from disconnection or error events                             |
| Data             | `[]byte`            | Data used on Message and on Error event, contains the payload for custom events |

## Socket instance functions

| Name         | Type     | Description                                                                       |
|:-------------|:---------|:----------------------------------------------------------------------------------|
| SetAttribute | `void`   | Set a specific attribute for the specific socket connection                       |
| GetUUID      | `string` | Get socket connection UUID                                                        |
| SetUUID      | `error`   | Set socket connection UUID                                                        |
| GetAttribute | `string` | Get a specific attribute from the socket attributes                               |
| EmitToList   | `void`   | Emit the message to a specific socket uuids list                                  |
| EmitTo       | `error`  | Emit to a specific socket connection                                              |
| Broadcast    | `void`   | Broadcast to all the active connections except broadcasting the message to itself |
| Fire         | `void`   | Fire custom event                                                                 |
| Emit         | `void`   | Emit/Write the message into the given connection                                  |
| Close        | `void`   | Actively close the connection from the server                                     |

**Note: the FastHTTP connection can be accessed directly from the instance**

```go
kws.Conn
```
