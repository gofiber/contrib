// Package legacy redirects the old plain WebSocket event-bus API to
// github.com/gofiber/contrib/v3/websocket/event.
//
// Deprecated: use github.com/gofiber/contrib/v3/websocket/event directly.
package legacy

import (
	"github.com/gofiber/contrib/v3/websocket"
	wsevent "github.com/gofiber/contrib/v3/websocket/event"
	"github.com/gofiber/fiber/v3"
)

// Websocket is the legacy plain WebSocket event connection type.
type Websocket = wsevent.Websocket

// EventPayload is the legacy plain WebSocket event payload type.
type EventPayload = wsevent.EventPayload

const (
	TextMessage   = wsevent.TextMessage
	BinaryMessage = wsevent.BinaryMessage
	CloseMessage  = wsevent.CloseMessage
	PingMessage   = wsevent.PingMessage
	PongMessage   = wsevent.PongMessage
)

const (
	EventMessage    = wsevent.EventMessage
	EventPing       = wsevent.EventPing
	EventPong       = wsevent.EventPong
	EventDisconnect = wsevent.EventDisconnect
	EventConnect    = wsevent.EventConnect
	EventClose      = wsevent.EventClose
	EventError      = wsevent.EventError
)

var (
	ErrorInvalidConnection = wsevent.ErrorInvalidConnection
	ErrorUUIDDuplication   = wsevent.ErrorUUIDDuplication
)

// New returns the legacy plain WebSocket event handler.
//
// Deprecated: use github.com/gofiber/contrib/v3/websocket/event.New directly.
func New(callback func(kws *Websocket), config ...websocket.Config) fiber.Handler {
	return wsevent.New(callback, config...)
}

// On adds a listener callback for an event.
//
// Deprecated: use github.com/gofiber/contrib/v3/websocket/event.On directly.
func On(event string, callback func(payload *EventPayload)) {
	wsevent.On(event, callback)
}

// EmitToList emits a message to a list of connection UUIDs and ignores errors.
//
// Deprecated: use github.com/gofiber/contrib/v3/websocket/event.EmitToList directly.
func EmitToList(uuids []string, message []byte, mType ...int) {
	wsevent.EmitToList(uuids, message, mType...)
}

// EmitTo emits a message to a connection UUID.
//
// Deprecated: use github.com/gofiber/contrib/v3/websocket/event.EmitTo directly.
func EmitTo(uuid string, message []byte, mType ...int) error {
	return wsevent.EmitTo(uuid, message, mType...)
}

// Broadcast emits to all active connections.
//
// Deprecated: use github.com/gofiber/contrib/v3/websocket/event.Broadcast directly.
func Broadcast(message []byte, mType ...int) {
	wsevent.Broadcast(message, mType...)
}

// Fire fires a custom event on all active connections.
//
// Deprecated: use github.com/gofiber/contrib/v3/websocket/event.Fire directly.
func Fire(event string, data []byte) {
	wsevent.Fire(event, data)
}
