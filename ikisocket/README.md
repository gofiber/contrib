
---

id: ikisocket
---

# Ikisocket

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=ikisocket*)
![Test](https://github.com/gofiber/contrib/workflows/Tests/badge.svg)
![Security](https://github.com/gofiber/contrib/workflows/Security/badge.svg)
![Linter](https://github.com/gofiber/contrib/workflows/Linter/badge.svg)

WebSocket wrapper for [Fiber](https://github.com/gofiber/fiber) with events support and inspired by [Socket.io](https://github.com/socketio/socket.io)

**Note: Requires Go 1.18 and above**

## Install

```
go get -u github.com/gofiber/fiber/v2
go get -u github.com/gofiber/contrib/ikisocket
```

## Signatures

```go
// Initialize new ikisocket in the callback this will
// execute a callback that expects kws *Websocket Object
func New(callback func(kws *Websocket)) func(*fiber.Ctx) error
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

 "github.com/gofiber/contrib/ikisocket"
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
 ikisocket.On(ikisocket.EventConnect, func(ep *ikisocket.EventPayload) {
  fmt.Println(fmt.Sprintf("Connection event 1 - User: %s", ep.Kws.GetStringAttribute("user_id")))
 })

 // Custom event handling supported
 ikisocket.On("CUSTOM_EVENT", func(ep *ikisocket.EventPayload) {
  fmt.Println(fmt.Sprintf("Custom event - User: %s", ep.Kws.GetStringAttribute("user_id")))
  // --->

  // DO YOUR BUSINESS HERE

  // --->
 })

 // On message event
 ikisocket.On(ikisocket.EventMessage, func(ep *ikisocket.EventPayload) {

  fmt.Println(fmt.Sprintf("Message event - User: %s - Message: %s", ep.Kws.GetStringAttribute("user_id"), string(ep.Data)))

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
  err = ep.Kws.EmitTo(clients[message.To], ep.Data, ikisocket.TextMessage)
  if err != nil {
   fmt.Println(err)
  }
 })

 // On disconnect event
 ikisocket.On(ikisocket.EventDisconnect, func(ep *ikisocket.EventPayload) {
  // Remove the user from the local clients
  delete(clients, ep.Kws.GetStringAttribute("user_id"))
  fmt.Println(fmt.Sprintf("Disconnection event - User: %s", ep.Kws.GetStringAttribute("user_id")))
 })

 // On close event
 // This event is called when the server disconnects the user actively with .Close() method
 ikisocket.On(ikisocket.EventClose, func(ep *ikisocket.EventPayload) {
  // Remove the user from the local clients
  delete(clients, ep.Kws.GetStringAttribute("user_id"))
  fmt.Println(fmt.Sprintf("Close event - User: %s", ep.Kws.GetStringAttribute("user_id")))
 })

 // On error event
 ikisocket.On(ikisocket.EventError, func(ep *ikisocket.EventPayload) {
  fmt.Println(fmt.Sprintf("Error event - User: %s", ep.Kws.GetStringAttribute("user_id")))
 })

 app.Get("/ws/:id", ikisocket.New(func(kws *ikisocket.Websocket) {

  // Retrieve the user id from endpoint
  userId := kws.Params("id")

  // Add the connection to the list of the connected clients
  // The UUID is generated randomly and is the key that allow
  // ikisocket to manage Emit/EmitTo/Broadcast
  clients[userId] = kws.UUID

  // Every websocket connection has an optional session key => value storage
  kws.SetAttribute("user_id", userId)

  //Broadcast to all the connected users the newcomer
  kws.Broadcast([]byte(fmt.Sprintf("New user connected: %s and UUID: %s", userId, kws.UUID)), true, ikisocket.TextMessage)
  //Write welcome message
  kws.Emit([]byte(fmt.Sprintf("Hello user: %s with UUID: %s", userId, kws.UUID)), ikisocket.TextMessage)
 }))

 log.Fatal(app.Listen(":3000"))
}

```

---


## Supported events

```go
// Supported event list
const (
 // Fired when a Text/Binary message is received
 EventMessage = "message"
 // More details here:
 // @url https://developer.mozilla.org/en-US/docs/Web/API/WebSockets_API/Writing_WebSocket_servers#Pings_and_Pongs_The_Heartbeat_of_WebSockets
 EventPing = "ping"
 EventPong = "pong"
 // Fired on disconnection
 // The error provided in disconnection event
 // is defined in RFC 6455, section 11.7.
 // @url https://github.com/gofiber/websocket/blob/cd4720c435de415b864d975a9ca23a47eaf081ef/websocket.go#L192
 EventDisconnect = "disconnect"
 // Fired on first connection
 EventConnect = "connect"
 // Fired when the connection is actively closed from the server
 EventClose = "close"
 // Fired when some error appears useful also for debugging websockets
 EventError = "error"
)
```

## Event Payload object

```go
// Event Payload is the object that
// stores all the information about the event and
// the connection
type EventPayload struct {
 // The connection object
 Kws *Websocket
 // The name of the event
 Name string
 // Unique connection UUID
 SocketUUID string
 // Optional websocket attributes
 SocketAttributes map[string]string
 // Optional error when are fired events like
 // - Disconnect
 // - Error
 Error error
 // Data is used on Message and on Error event
 Data []byte
}
```

---

**The FastHTTP connection can be accessed directly from the struct**

```go
type Websocket struct {
    // The FastHTTP connection
    Conn *websocket.Conn
}
```

Can be accessed from

```go
kws.Conn
```

## Socket instance functions

```go
// Set a specific attribute for the specific socket connection
func (kws *Websocket) SetAttribute(key string, attribute string)
```

```go
// Get socket connection UUID
func (kws *Websocket) GetUUID() string
```

```go
// Set socket connection UUID
func (kws *Websocket) SetUUID(uuid string)
```

```go
// Get a specific attribute from the socket attributes
func (kws *Websocket) GetAttribute(key string) string
```

```go
// Emit the message to a specific socket uuids list
func (kws *Websocket) EmitToList(uuids []string, message []byte) 
```

```go
// Emit to a specific socket connection
func (kws *Websocket) EmitTo(uuid string, message []byte) error
```

```go
// Broadcast to all the active connections
// except avoid broadcasting the message to itself
func (kws *Websocket) Broadcast(message []byte, except bool)
```

```go
// Fire custom event
func (kws *Websocket) Fire(event string, data []byte)
```

```go
// Emit/Write the message into the given connection
func (kws *Websocket) Emit(message []byte)
```

```go
// Actively close the connection from the server
func (kws *Websocket) Close() 
```