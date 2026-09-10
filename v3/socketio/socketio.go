package socketio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
)

// Engine.IO v4 packet type bytes
const (
	eioOpen    = '0' // Server → Client: sent right after upgrade, carries session info
	eioClose   = '1' // Either side: request to close the transport
	eioPing    = '2' // Server → Client: heartbeat ping
	eioPong    = '3' // Client → Server: heartbeat pong (response to eioPing)
	eioMessage = '4' // Either side: wraps a Socket.IO packet
	eioUpgrade = '5' // Client → Server: signals transport upgrade complete
	eioNoop    = '6' // Either side: no-operation
)

// Engine.IO framing constants shared between WebSocket and HTTP polling.
const (
	// eioPacketSeparator is the ASCII record-separator (0x1E, RS) byte
	// Engine.IO v4 uses to delimit multiple packets in one batched
	// frame (WebSocket text frame or HTTP polling body).
	eioPacketSeparator byte = 0x1E
	// defaultMaxPayload is the fallback advertised in the EIO OPEN
	// packet when MaxPayload is unset or non-positive. Matches the
	// engine.io reference server default (1 MB).
	defaultMaxPayload int64 = 1_000_000
)

// Socket.IO v5 packet type bytes (carried inside an eioMessage payload)
const (
	sioConnect      = '0' // Client → Server: connect to a namespace
	sioDisconnect   = '1' // Either side: disconnect from a namespace
	sioEvent        = '2' // Either side: named event with JSON payload
	sioAck          = '3' // Either side: acknowledge a previous event
	sioConnectError = '4' // Server → Client: namespace connection error
	sioBinaryEvent  = '5' // Either side: named event with binary payload
	sioBinaryAck    = '6' // Either side: binary acknowledge
)

// Source @url:https://github.com/gorilla/websocket/blob/master/conn.go#L61
// The message types are defined in RFC 6455, section 11.8.
const (
	// TextMessage denotes a text data message. The text message payload is
	// interpreted as UTF-8 encoded text data.
	TextMessage = 1
	// BinaryMessage denotes a binary data message.
	BinaryMessage = 2
	// CloseMessage denotes a close control message. The optional message
	// payload contains a numeric code and text. Use the FormatCloseMessage
	// function to format a close message payload.
	CloseMessage = 8
	// PingMessage denotes a ping control message. The optional message payload
	// is UTF-8 encoded text.
	PingMessage = 9
	// PongMessage denotes a pong control message. The optional message payload
	// is UTF-8 encoded text.
	PongMessage = 10
)

// closeFrameMarker is the internal queue entry that asks the send goroutine
// to perform the closing handshake once every frame queued before it is on
// the wire: the optional SIO DISCONNECT packet carried in message.data,
// then a Close control frame.
const closeFrameMarker = -1

// controlWriteTimeout bounds the Pong reply to a peer Ping and the Close
// frame written by the send goroutine, so a peer that stopped reading
// cannot park either goroutine.
const controlWriteTimeout = 5 * time.Second

// Supported event list
const (
	// EventMessage is fired when a text or binary message is received that is
	// not bound to a named Socket.IO event (i.e. wire-level "message" events
	// or raw binary frames).
	EventMessage = "message"
	// EventPing is fired when a WebSocket PING control frame is received
	// from the peer. EventPayload.Data carries the ping payload. See
	// https://developer.mozilla.org/en-US/docs/Web/API/WebSockets_API/Writing_WebSocket_servers#Pings_and_Pongs_The_Heartbeat_of_WebSockets
	EventPing = "ping"
	// EventPong is fired when a WebSocket PONG control frame, or an
	// Engine.IO PONG packet, is received from the peer.
	EventPong = "pong"
	// EventDisconnect is fired exactly once when the connection is torn
	// down, regardless of which side initiated the close. The
	// EventPayload.Error field carries the cause when the teardown was
	// not a clean close: nil for Websocket.Close, for a client that sent
	// SIO DISCONNECT, and for a peer Close frame with code 1000, 1001 or
	// no code (RFC 6455 section 11.7).
	EventDisconnect = "disconnect"
	// EventConnect is fired exactly once after the Engine.IO and Socket.IO
	// handshake completes, before the read loop starts dispatching events.
	// EventPayload.HandshakeAuth carries the client's auth payload, if any.
	EventConnect = "connect"
	// EventClose is fired exactly once when the connection is closed from
	// the server side via Websocket.Close, before the SIO DISCONNECT and
	// Close frames are queued: frames a listener emits still reach the
	// peer ahead of them.
	EventClose = "close"
	// EventError is fired when an error occurs on the connection (read,
	// write, parse, or listener panic). EventPayload.Error carries the
	// error value.
	EventError = "error"
)

var (
	// ErrorInvalidConnection is returned when the addressed Conn connection is no
	// longer available; the error data carries the UUID of that connection.
	ErrorInvalidConnection = errors.New("message cannot be delivered invalid/gone connection")
	// ErrorUUIDDuplication is returned when the requested UUID already exists in
	// the active connections pool.
	ErrorUUIDDuplication = errors.New("UUID already exists in the available connections pool")
	// ErrAckNotRequested is returned by EventPayload.Ack when the event did
	// not include an ack id (i.e. the client emitted without a callback).
	ErrAckNotRequested = errors.New("socketio: event has no ack id")
	// ErrAckAlreadySent is returned by EventPayload.Ack when Ack() has
	// already been called once for this payload.
	ErrAckAlreadySent = errors.New("socketio: ack already sent")
	// ErrAckTimeout is delivered to outbound ack callbacks (registered via
	// EmitWithAckTimeout) when the client does not respond within the
	// configured timeout.
	ErrAckTimeout = errors.New("socketio: ack timeout")
	// ErrAckDisconnected is delivered to outbound ack callbacks when the
	// connection is torn down before an ack arrives.
	ErrAckDisconnected = errors.New("socketio: connection closed before ack")
	// ErrReservedEventName is surfaced via EventError when user code tries
	// to emit a name the JS socket.io client treats as a reserved
	// lifecycle event (e.g. "connect", "disconnect"). The emit is dropped
	// before the frame reaches the wire.
	ErrReservedEventName = errors.New("socketio: reserved event name cannot be emitted")
	// ErrAckIDOverflow is returned from inbound SIO frame parsing when the
	// numeric ack id prefix would overflow uint64. Surfaced via EventError.
	ErrAckIDOverflow = errors.New("socketio: ack id overflow")
	// ErrEmptyEventArray is returned from inbound SIO EVENT parsing when
	// the JSON-array payload is empty (no event name). Surfaced via
	// EventError.
	ErrEmptyEventArray = errors.New("socketio: empty event array")
	// ErrHandshakeClosed is returned from the EIO/SIO handshake when the
	// peer closes the connection before completing the SIO CONNECT step.
	ErrHandshakeClosed = errors.New("socketio: connection closed during handshake")
	// ErrInvalidNamespace is returned from the SIO CONNECT handshake when
	// the namespace prefix fails charset validation. The handshake is
	// rejected with CONNECT_ERROR before any user code runs.
	ErrInvalidNamespace = errors.New("socketio: invalid namespace in SIO CONNECT")
	// ErrInvalidAuthPayload is returned from the SIO CONNECT handshake
	// when the auth field is malformed JSON, exceeds MaxAuthPayload, or
	// is not a JSON object per socket.io-protocol v5.
	ErrInvalidAuthPayload = errors.New("socketio: invalid auth payload in SIO CONNECT")
	// ErrHeartbeatTimeout is the disconnection cause delivered to
	// EventDisconnect when no client PONG arrives within
	// PingInterval + PingTimeout.
	ErrHeartbeatTimeout = errors.New("socketio: heartbeat timeout")
	// ErrSendQueueClosed is the disconnection cause delivered to
	// EventDisconnect when the send queue is full and DropFramesOnOverflow
	// is false (the legacy hard-teardown path). Compare against
	// ErrSendQueueOverflow which fires on per-frame drops when
	// DropFramesOnOverflow is true.
	ErrSendQueueClosed = errors.New("socketio: send queue overflow")
	// ErrBatchPacketsExceeded is surfaced via EventError when a batched
	// EIO frame contains more than MaxBatchPackets record-separated
	// packets. Remaining packets in the frame are dropped.
	ErrBatchPacketsExceeded = errors.New("socketio: batched frame exceeds MaxBatchPackets")
	// ErrPollingBodyTooLarge is delivered to EventDisconnect when an
	// inbound polling POST exceeds PollingMaxBufferSize. The session
	// is torn down to mirror the WebSocket SetReadLimit behaviour;
	// repeated oversized POSTs would otherwise keep sessions alive
	// until heartbeat reaped them.
	ErrPollingBodyTooLarge = errors.New("socketio: polling POST body exceeds PollingMaxBufferSize")
	// ErrPollingBeforeConnect is delivered to EventDisconnect when a
	// polling session sends any Socket.IO payload before the first
	// SIO CONNECT packet completes auth/namespace validation.
	ErrPollingBeforeConnect = errors.New("socketio: polling packet received before SIO CONNECT")
	// ErrUnknownEIOPacket is surfaced via EventError when the inbound EIO
	// packet type byte does not match any recognised Engine.IO opcode.
	ErrUnknownEIOPacket = errors.New("socketio: unknown EIO packet type")
	// ErrTooManyArgs is surfaced via EventError when an inbound EVENT
	// array holds more elements than MaxEventArgs; an ACK array over the
	// limit is dropped silently, like any other malformed ACK.
	ErrTooManyArgs = errors.New("socketio: packet exceeds MaxEventArgs")
)

// Tunable package-level knobs. Mutate them before calling New so each new
// connection captures the desired value; in-flight connections retain the
// values in effect at handshake time and are not affected by later changes.
// These vars are not safe for concurrent mutation once connections are open.
var (
	// PongTimeout is the legacy heartbeat timeout knob.
	//
	// Deprecated: PongTimeout is no longer consulted by the heartbeat
	// implementation. Use PingTimeout instead.
	PongTimeout = 20 * time.Second
	// RetrySendTimeout was the back-off between retries of a write against
	// a connection the websocket middleware had already released.
	//
	// Deprecated: the middleware allocates a connection per upgrade and
	// never releases it while the handler runs, so there is nothing to
	// retry; the send goroutine writes each frame exactly once. Setting
	// it has no effect.
	RetrySendTimeout = 20 * time.Millisecond
	// MaxSendRetry was the retry budget paired with RetrySendTimeout.
	//
	// Deprecated: see RetrySendTimeout; setting it has no effect.
	MaxSendRetry = 5
	// ReadTimeout is no longer consulted by the read loop, which blocks in
	// a single ReadMessage call rather than busy-polling with a sleep.
	//
	// Deprecated: kept only for backward compatibility with code that
	// still references the variable; setting it has no effect.
	ReadTimeout = 10 * time.Millisecond
	// PingInterval is how often the server sends Engine.IO PING packets
	// to the client. The client must reply with a PONG packet to keep
	// the connection alive. Read once per connection at handshake time;
	// mutating it does not affect already-open sockets.
	PingInterval = 25 * time.Second
	// PingTimeout is advertised to the client in the EIO OPEN packet and
	// is also used by the server-side heartbeat enforcer: a connection
	// is dropped when no frame arrives within PingInterval + PingTimeout.
	// Read once per connection at handshake time.
	PingTimeout = 20 * time.Second
	// HandshakeTimeout caps how long the server waits for the client SIO
	// CONNECT packet ("40") after sending the EIO OPEN packet. Set to
	// zero to disable.
	HandshakeTimeout = 10 * time.Second
	// CloseTimeout bounds the closing handshake. After Websocket.Close
	// (or a client SIO DISCONNECT) the server sends its Close frame and
	// keeps reading, discarding frames, until the peer's Close frame or
	// EOF arrives, so the SIO DISCONNECT packet is never lost to a TCP
	// reset; the socket is closed regardless once CloseTimeout elapses.
	// It also bounds how long Close waits for a saturated send queue to
	// accept the closing frames. Set to zero to close the socket as soon
	// as the frames are queued. Read once per Close call.
	CloseTimeout = 5 * time.Second
	// WriteTimeout bounds a single WebSocket frame write by the send
	// goroutine. A peer that stops reading is otherwise only detected by
	// the heartbeat (PingInterval + PingTimeout). Zero disables the
	// deadline. Read once per frame.
	WriteTimeout time.Duration
	// MaxPayload is the maximum size in bytes of an inbound WebSocket
	// frame. It is advertised to the client in the EIO OPEN packet and
	// enforced via SetReadLimit on the underlying connection: frames
	// exceeding this size are rejected and the connection is closed. Set
	// to zero or negative to disable the limit (not recommended).
	MaxPayload int64 = 1_000_000
	// MaxAuthPayload is the maximum size in bytes of the JSON auth
	// payload supplied by the client in the SIO CONNECT ("40") packet.
	// Per socket.io-protocol v5 the auth field is a JSON object (or
	// absent); any payload exceeding this cap is rejected with
	// CONNECT_ERROR and the handshake fails. Defaults to 8 KiB. Set to
	// zero to disable.
	MaxAuthPayload int = 8 * 1024
	// OutboundAckTimeout is the default deadline for ack callbacks
	// registered via Websocket.EmitWithAck and Websocket.EmitWithAckArgs.
	// Use Websocket.EmitWithAckTimeout for per-call overrides.
	OutboundAckTimeout = 30 * time.Second
	// MaxBatchPackets caps the number of EIO packets accepted in a
	// single 0x1E-batched WebSocket frame. Without this cap, a frame
	// consisting almost entirely of record separators forces a
	// multi-megabyte slice header allocation. 256 is comfortably above
	// any legitimate batch.
	MaxBatchPackets = 256
	// MaxEventNameLength bounds inbound SIO event name strings (in
	// bytes) so a hostile client cannot pin a multi-megabyte string per
	// frame inside the EventPayload dispatched to user listeners. Set
	// to zero to disable the bound (not recommended).
	MaxEventNameLength = 256
	// MaxEventArgs caps the number of elements accepted in an inbound
	// SIO EVENT or ACK array, the event name included. Each element
	// costs a slice header, so without the cap a payload of MaxPayload
	// bytes made of one-byte elements would allocate an order of
	// magnitude more than its own size before any listener runs. 256 is
	// comfortably above any legitimate argument list. Set to zero to
	// disable the bound (not recommended).
	MaxEventArgs = 256
	// SendQueueSize is the buffered capacity of the per-connection
	// outbound frame queue. Tune it before connections are accepted;
	// existing sockets retain the size in effect at New() time.
	SendQueueSize = 100
	// DropFramesOnOverflow controls behavior when the per-connection
	// send queue is full. When false (default) the connection is torn
	// down with a "send queue overflow" error (legacy behavior); when
	// true the individual frame is dropped and EventError fires with
	// ErrSendQueueOverflow, allowing the connection to survive bursty
	// producers.
	DropFramesOnOverflow = false
	// ErrSendQueueOverflow is surfaced via EventError when a frame is
	// dropped because the send queue was full and DropFramesOnOverflow
	// is true.
	ErrSendQueueOverflow = errors.New("socketio: send queue overflow, frame dropped")
)

// Logger is an optional package-level hook that, when non-nil, receives
// every internal warning/error the socketio package emits. The default
// (nil) preserves the historical "silent" behavior, so this is a non-
// breaking addition.
//
// level is one of "warn" or "error". msg is a short, stable description
// of the event class (e.g. "handshake_failure", "queue_overflow",
// "ack_timeout"). fields is a flat key/value list of structured context
// suitable for forwarding to slog/zap/zerolog/etc., e.g.
//
//	Logger = func(level, msg string, fields ...any) {
//	    slog.Default().Log(context.Background(), levelOf(level), msg, fields...)
//	}
//
// Implementations MUST be safe for concurrent use and MUST NOT block;
// the hook is invoked from goroutines on the connection hot path.
//
// Logger is read without synchronisation; assign it once during process
// startup before serving connections.
var Logger func(level, msg string, fields ...any)

// logf is the internal trampoline that fans warnings/errors out to the
// Logger hook when configured. Safe to call when Logger is nil; recovers
// from any panic in the user-supplied hook so a buggy logger cannot kill
// a connection.
func logf(level, msg string, fields ...any) {
	if Logger == nil {
		return
	}
	defer func() { _ = recover() }()
	Logger(level, msg, fields...)
}

// AckCallback receives the result of a server-initiated EmitWithAck.
//
// On a successful ack, ack holds the raw JSON of the client's FIRST ack
// argument (or nil when the client acked with no arguments) and err is nil.
// Multi-argument acks are not surfaced through this single-arg shape; use
// EmitWithAckArgs when the client may ack with more than one argument or
// when "single arg that is itself a JSON array" must be distinguished from
// "multiple args".
//
// On timeout err is ErrAckTimeout. On connection close err is
// ErrAckDisconnected. In both error cases ack is nil.
type AckCallback func(ack []byte, err error)

// pendingAck tracks one outstanding outbound ack: the callback to invoke
// and an optional timer that fires ErrAckTimeout if the client never
// responds.
//
// The fired atomic.Bool is the authoritative single-fire guard. While the
// outboundAcksMu map-delete-wins pattern already serialises which path
// removes the entry, the timer is registered after the inserting goroutine
// drops the lock, so a delivery that arrives in that window would race
// with the timer-field write under the Go race detector. fired is the
// source of truth: whichever caller wins fired.CompareAndSwap(false, true)
// invokes cb exactly once, the others bail out. This decouples the
// at-most-once invariant from the timer-field publication ordering.
type pendingAck struct {
	// cb receives the structured ack arguments as they arrived on the wire:
	// "43<id>[a,b,c]" produces args = [a,b,c] (one entry per JSON value in
	// the array). The single-arg AckCallback shape is preserved for the
	// public API by adapting "args[0] or nil" inside EmitWithAckTimeout.
	// Carrying [][]byte internally keeps multi-arg and "single arg that is
	// itself an array" paths distinguishable for EmitWithAckArgs.
	cb    func(args [][]byte, err error)
	timer *time.Timer
	fired atomic.Bool
}

// eioPingFrame is the cached single-byte EIO PING packet that the
// heartbeat puts on the wire. Sharing one slice across every emit avoids
// a per-tick allocation; the underlying bytes are never mutated by
// Conn.WriteMessage.
var eioPingFrame = []byte{eioPing}

// closeFramePayload is the Close control frame payload the server sends
// when it initiates the closing handshake.
var closeFramePayload = websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Connection closed")

// Raw form of websocket message
type message struct {
	// Message type
	mType int
	// Message data
	data []byte
}

// EventPayload is the read-only value passed to every event listener. It
// carries the originating connection (Kws), the event arguments (Args, with
// Data as a shortcut to the first arg), ack bookkeeping (AckID, HasAck, Ack)
// and the handshake auth payload (HandshakeAuth, populated for EventConnect
// listeners only).
//
// Byte-slice fields (Args, Data, HandshakeAuth) are backed by memory that
// belongs to the connection, never by a buffer the transport reuses: the
// WebSocket transport hands over a fresh buffer per message, the polling
// transport copies each request body once, and HandshakeAuth is captured
// during the handshake. Listeners may safely retain these slices across
// goroutine boundaries.
type EventPayload struct {
	// Kws is the connection that fired the event. Use it to call
	// Emit/EmitEvent/Close from inside the listener.
	Kws *Websocket
	// Name is the event name as registered with On (e.g. "message",
	// EventConnect, or any custom event).
	Name string
	// SocketUUID is the unique identifier of the originating connection,
	// captured at dispatch time so it remains stable even if the
	// connection is concurrently closed.
	SocketUUID string
	// SocketAttributes is a defensive snapshot of the connection's
	// attribute map taken at dispatch time. Mutating it does not affect
	// the live connection; use Kws.SetAttribute for that. It is nil when
	// the connection has no attributes.
	SocketAttributes map[string]any
	// Error is the cause associated with lifecycle events such as
	// EventDisconnect and EventError; nil for ordinary user events.
	Error error
	// Data is the first event argument (if any). Kept for backwards
	// compatibility; equivalent to Args[0] when len(Args) > 0, else nil.
	Data []byte
	// Args are the raw-JSON arguments the client sent with the event.
	// Each entry is one JSON value; nil for events without args. Use
	// this to consume socket.emit("event", a, b, c) from the JS client.
	Args [][]byte
	// AckID is the Socket.IO ack id attached to this event by the
	// client. It is meaningful only when HasAck is true. Use Ack to
	// respond.
	AckID uint64
	// HasAck reports whether the client requested an ack for this
	// event (i.e. the JS side called socket.emit("event", data,
	// callback)).
	HasAck bool
	// HandshakeAuth is the raw JSON auth payload the client supplied
	// in its SIO CONNECT packet, copied for safety. nil if the client
	// connected without an auth payload. Populated for EventConnect
	// listeners; for other events use Kws.HandshakeAuth instead.
	HandshakeAuth json.RawMessage
	// ackSent is the CAS guard that makes Ack idempotent. It is shared
	// across all EventPayload instances dispatched for the same inbound
	// SIO event (one per listener), so that two listeners both calling
	// Ack on their own payload still produce only one wire frame.
	ackSent *atomic.Bool
}

// Ack sends a Socket.IO ACK ("43") response back to the client for the event
// represented by this payload. The variadic args ...[]byte signature accepts
// zero arguments (empty ack), one argument (single value), or many arguments
// that are emitted as comma-separated values, mirroring the JS-side
// callback(a, b, c) shape. Valid JSON args are passed through; raw text args
// are encoded as JSON strings. Nil or empty entries are skipped.
//
// Ack is idempotent across all listeners dispatched for the same inbound
// event: only the first invocation produces a wire frame, and subsequent
// calls (whether on the same payload or on a sibling payload handed to
// another listener) return ErrAckAlreadySent.
//
// Returns an error if the event has no ack id, the connection is closed, or
// the ack has already been sent for this payload.
func (ep *EventPayload) Ack(args ...[]byte) error {
	if !ep.HasAck {
		return ErrAckNotRequested
	}
	if ep.Kws == nil || !ep.Kws.IsAlive() {
		return ErrorInvalidConnection
	}
	if ep.ackSent == nil {
		// Defensive: should always be set by fireEventWithAck. Treat a
		// missing guard as if no ack has been sent yet.
		var b atomic.Bool
		ep.ackSent = &b
	}
	if !ep.ackSent.CompareAndSwap(false, true) {
		return ErrAckAlreadySent
	}
	ep.Kws.write(TextMessage, buildSIOAck(ep.Kws.getNamespace(), ep.AckID, args))
	return nil
}

// runUserCallback invokes the user's New() callback inside a recover
// block so a panicking callback cannot leak the session. Returns the
// recovered panic value (nil on clean return). Used by both the
// WebSocket and polling open paths so a single recover discipline
// applies regardless of transport.
func runUserCallback(callback func(*Websocket), kws *Websocket) (recovered interface{}) {
	defer func() { recovered = recover() }()
	callback(kws)
	return nil
}

// ws is the connection surface the pool stores. Everything except
// fireEvent is public API; fireEvent lets package-level Fire reach every
// connection.
type ws interface {
	IsAlive() bool
	GetUUID() string
	SetUUID(uuid string) error
	SetAttribute(key string, attribute interface{})
	GetAttribute(key string) interface{}
	GetIntAttribute(key string) int
	GetStringAttribute(key string) string
	EmitToList(uuids []string, message []byte, mType ...int)
	EmitTo(uuid string, message []byte, mType ...int) error
	Broadcast(message []byte, except bool, mType ...int)
	Fire(event string, data []byte)
	Emit(message []byte, mType ...int)
	EmitEvent(event string, data []byte)
	EmitArgs(event string, args ...[]byte)
	EmitWithAck(event string, data []byte, cb func(ack []byte))
	EmitWithAckTimeout(event string, data []byte, timeout time.Duration, cb AckCallback)
	EmitWithAckArgs(event string, args [][]byte, cb func([][]byte, error))
	Close()
	fireEvent(event string, data []byte, error error)
}

// Websocket represents a single Socket.IO connection on top of the underlying
// Fiber WebSocket. It carries the per-connection state (UUID, namespace,
// attributes, ack bookkeeping) and exposes the Emit/Broadcast/Close API that
// user code interacts with from inside listener callbacks.
//
// Goroutines per WebSocket connection: the upgrade handler goroutine runs
// the read loop and one send goroutine serialises writes; the heartbeat is
// a runtime timer. Polling sessions own no goroutine at all.
type Websocket struct {
	// once guards disconnected: the tear-down runs exactly once.
	once sync.Once
	// finishOnce guards finishRun: the final cleanup (socket close,
	// send goroutine join, closed channel) runs exactly once.
	finishOnce sync.Once
	// closeStarted flips on the first Close call; concurrent or re-entrant
	// callers return immediately.
	closeStarted atomic.Bool
	// closeRequested reports that the closing handshake has been queued:
	// disconnected then keeps the socket open, bounded by closeGrace, so
	// the read loop can consume the peer's Close frame.
	closeRequested atomic.Bool
	// closeGrace is CloseTimeout as captured when the session was created
	// and again when Close was called, in nanoseconds, so the tear-down
	// never reads the global while another goroutine may set it.
	closeGrace atomic.Int64
	// writeTimeout is WriteTimeout as captured when the session was
	// created; only the send goroutine reads it.
	writeTimeout time.Duration
	// teardownDeadline is the read deadline (unix nanoseconds) the
	// tear-down armed to return the read loop, so a concurrent idle
	// refresh that lost the race can reinstate it.
	teardownDeadline atomic.Int64
	// closeDeadline is the end (unix nanoseconds) of the tear-down budget:
	// CloseTimeout from the moment the tear-down began, set by whichever
	// of beginClose and disconnected ran first. Waiting for queue space,
	// the closing handshake and an in-flight write all share what is left
	// of it. Zero when CloseTimeout is zero: nothing waits.
	closeDeadline atomic.Int64
	// mu guards UUID, attributes and handshakeAuth.
	mu sync.RWMutex
	// Conn is the underlying Fiber WebSocket connection. Treat it as
	// read-only from listener callbacks; writes must go through the
	// Emit/EmitEvent/Broadcast methods so the send goroutine remains
	// the sole writer. nil for polling sessions.
	Conn *websocket.Conn
	// isAlive reports whether the connection is alive. Accessed lock-free
	// from every emit path (write/EmitTo/etc.) and the read loop.
	isAlive atomic.Bool
	// Queue of messages sent from the socket
	queue chan message
	// done is closed by disconnected once the connection is torn down.
	done chan struct{}
	// closed is closed by finishRun once the socket is closed and the
	// send goroutine has exited; Shutdown waits on it.
	closed chan struct{}
	// sendDone is closed when the send goroutine exits. finishRun waits
	// on it, bounded by CloseTimeout, so the closing frames reach the
	// wire before the socket is closed.
	sendDone chan struct{}
	// ctx is the lifetime context of the send goroutine.
	ctx context.Context
	// cancelCtx cancels ctx when the connection is torn down.
	cancelCtx context.CancelFunc
	// namespace is the Socket.IO namespace this connection belongs to.
	// nil means the root namespace. Captured during the handshake from
	// the client's CONNECT packet so outbound events can mirror it;
	// read lock-free on every emit.
	namespace atomic.Pointer[[]byte]
	// handshakeAuth is the raw JSON auth payload supplied by the client in
	// its SIO CONNECT packet (e.g. `{"token":"..."}`). nil for clients that
	// connect without an auth payload.
	handshakeAuth json.RawMessage
	// lastPongNanos is the unix-nano timestamp of the last frame received
	// from the client. The heartbeat uses it to enforce the timeout
	// (PingInterval + PingTimeout) and disconnect dead peers.
	lastPongNanos atomic.Int64
	// lastPingNanos is when the heartbeat last put a PING on the wire.
	lastPingNanos atomic.Int64
	// heartbeat is the runtime timer driving the heartbeat; the settings
	// are captured once when it starts.
	heartbeat  atomic.Pointer[time.Timer]
	hbInterval time.Duration
	hbDeadline time.Duration
	hbTick     time.Duration
	// outboundAckSeq is the monotonic counter for ack ids on emits issued
	// via EmitWithAck. It is incremented under outboundAcksMu.
	outboundAckSeq uint64
	// outboundAcks tracks pending callbacks for server-initiated emits that
	// asked for an ack from the client. The map is keyed by ack id and
	// holds a callback plus an optional timeout timer.
	outboundAcks   map[uint64]*pendingAck
	outboundAcksMu sync.Mutex
	// Attributes map collection for the connection
	attributes map[string]interface{}
	// UUID is the unique identifier assigned to this connection and used as
	// its key in the active connections pool.
	UUID string
	// Locals wraps the Fiber Locals lookup so listener callbacks can reach
	// values stored on the originating request context.
	Locals func(key string) interface{}
	// Params wraps the Fiber Params lookup so listener callbacks can read
	// route parameters from the originating request.
	Params func(key string, defaultValue ...string) string
	// Query wraps the Fiber Query lookup so listener callbacks can read
	// query-string values from the originating request.
	Query func(key string, defaultValue ...string) string
	// Cookies wraps the Fiber Cookies lookup so listener callbacks can read
	// cookie values from the originating request.
	Cookies func(key string, defaultValue ...string) string
	// pollCallback stores the user's New callback until the polling transport
	// receives its first SIO CONNECT packet. This mirrors the WebSocket path:
	// user code runs only after the Socket.IO namespace/auth handshake has
	// completed, so Emits from the callback are ordered after the CONNECT ack.
	pollCallback func(*Websocket)
	// pollQ is the per-session outbound buffer for HTTP long-polling
	// sessions. Non-nil identifies a polling session; nil means the
	// session is bound to the WebSocket transport. See polling.go.
	pollQ *pollQueue
	// pollGate ensures at most one concurrent long-poll GET per polling
	// session. Engine.IO mandates a single in-flight poll per sid; a
	// second GET while another is blocked is rejected with HTTP 400 and
	// engine.io error code 3.
	pollGate atomic.Bool
	// connectFired flips true after EventConnect has been dispatched for
	// this session. Used by the polling first-CONNECT path to fire the
	// listener exactly once when SIO CONNECT arrives via POST. Unused on
	// the WebSocket path (handshake() fires EventConnect synchronously).
	connectFired atomic.Bool
	// postGate serialises polling POST handlers per session. Engine.IO
	// clients send POSTs sequentially, but a misbehaving or hostile
	// client could fire two simultaneously; without serialisation the
	// dispatch order across the two POSTs would race and break the
	// per-session FIFO guarantee that user listeners rely on. Held for
	// the whole of ingestPolling - the body is materialised under it, so
	// queued POSTs wait before allocating rather than after - but never
	// held across other locks.
	postGate sync.Mutex
	// handshakeTimer is the time.AfterFunc scheduled by openPollingSession
	// to enforce HandshakeTimeout on polling sessions. Stopped in
	// disconnected so a gracefully-closed session does not keep the
	// closure (and therefore *Websocket) pinned on the runtime timer
	// heap for the full HandshakeTimeout budget. Stored via
	// atomic.Pointer so the disconnected() reader sees a consistent
	// value even if the timer fires before the assignment in
	// openPollingSession is visible.
	handshakeTimer atomic.Pointer[time.Timer]
}

// newWebsocket allocates the transport-independent part of a session: the
// send queue, the lifecycle channels, the UUID and the liveness flags.
// attributes and outboundAcks are lazy-initialised on first SetAttribute /
// EmitWithAck* call; most idle connections never touch them.
func newWebsocket() *Websocket {
	queueSize := SendQueueSize
	if queueSize < 1 {
		queueSize = 1
	}
	kws := &Websocket{
		queue:        make(chan message, queueSize),
		done:         make(chan struct{}),
		closed:       make(chan struct{}),
		sendDone:     make(chan struct{}),
		writeTimeout: WriteTimeout,
	}
	kws.closeGrace.Store(int64(CloseTimeout))
	kws.isAlive.Store(true)
	kws.lastPongNanos.Store(time.Now().UnixNano())
	kws.UUID = kws.createUUID()
	return kws
}

type safePool struct {
	sync.RWMutex
	// List of the connections alive
	conn map[string]ws
	// snap caches the last snapshot until membership changes, so a
	// broadcast storm on a stable pool never copies the map.
	snap []ws
}

// Pool with the active connections
var pool = safePool{
	conn: make(map[string]ws),
}

func (p *safePool) set(ws ws) {
	p.Lock()
	p.conn[ws.GetUUID()] = ws
	p.snap = nil
	p.Unlock()
}

// snapshot returns the live connections. The slice is shared until
// membership changes and must not be modified.
func (p *safePool) snapshot() []ws {
	p.RLock()
	s := p.snap
	p.RUnlock()
	if s != nil {
		return s
	}
	p.Lock()
	if p.snap == nil {
		s = make([]ws, 0, len(p.conn))
		for _, kws := range p.conn {
			s = append(s, kws)
		}
		p.snap = s
	}
	s = p.snap
	p.Unlock()
	return s
}

// all returns a copy of the pool map. Only tests use it; production paths
// iterate snapshot.
//
//nolint:unused // test helper
func (p *safePool) all() map[string]ws {
	p.RLock()
	ret := make(map[string]ws, len(p.conn))
	maps.Copy(ret, p.conn)
	p.RUnlock()
	return ret
}

func (p *safePool) get(key string) (ws, error) {
	p.RLock()
	ret, ok := p.conn[key]
	p.RUnlock()
	if !ok {
		return nil, ErrorInvalidConnection
	}
	return ret, nil
}

func (p *safePool) delete(key string) {
	p.Lock()
	delete(p.conn, key)
	p.snap = nil
	p.Unlock()
}

//nolint:all
func (p *safePool) reset() {
	p.Lock()
	p.conn = make(map[string]ws)
	p.snap = nil
	p.Unlock()
}

// draining holds WebSocket sessions that have left the pool but still own
// their socket: the closing handshake, or the last write, is in flight
// until finishRun. Shutdown waits for these too, so a session closed a
// moment before Shutdown cannot outlive it. A session is added before it
// leaves the pool, so it is in at least one of the two sets at any time.
var draining = struct {
	sync.Mutex
	m map[*Websocket]struct{}
}{m: make(map[*Websocket]struct{})}

func (kws *Websocket) markDraining() {
	draining.Lock()
	draining.m[kws] = struct{}{}
	draining.Unlock()
}

func (kws *Websocket) unmarkDraining() {
	draining.Lock()
	delete(draining.m, kws)
	draining.Unlock()
}

func drainingSessions() []*Websocket {
	draining.Lock()
	out := make([]*Websocket, 0, len(draining.m))
	for kws := range draining.m {
		out = append(out, kws)
	}
	draining.Unlock()
	return out
}

//nolint:unused // test helper
func resetDraining() {
	draining.Lock()
	draining.m = make(map[*Websocket]struct{})
	draining.Unlock()
}

// safeListeners is a copy-on-write registry of event callbacks.
//
// Reads are lock-free: a single atomic.Pointer load yields the current
// immutable map, then a map index yields the slice. The returned slice
// is the same backing array all subsequent readers observe and MUST NOT
// be mutated by callers; listeners are append-only so values are
// read-only after publication.
//
// Writes serialise on writeMu, clone the map, append to a fresh slice
// and atomic.Store the new pointer. A get() racing a set() may or may
// not observe the new listener; eventual consistency is acceptable for
// registration-time mutations.
type safeListeners struct {
	writeMu sync.Mutex
	m       atomic.Pointer[map[string][]eventCallback]
}

func (l *safeListeners) set(event string, callback eventCallback) {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()

	cur := l.m.Load()
	next := make(map[string][]eventCallback, len(*cur)+1)
	maps.Copy(next, *cur)
	old := next[event]
	cp := make([]eventCallback, len(old), len(old)+1)
	copy(cp, old)
	next[event] = append(cp, callback)

	l.m.Store(&next)
}

func (l *safeListeners) get(event string) []eventCallback {
	return (*l.m.Load())[event]
}

//nolint:unused
func (l *safeListeners) reset() {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	empty := make(map[string][]eventCallback)
	l.m.Store(&empty)
}

// List of the listeners for the events.
var listeners = func() *safeListeners {
	l := &safeListeners{}
	empty := make(map[string][]eventCallback)
	l.m.Store(&empty)
	return l
}()

// unsupportedEIOVersionBody is the JSON body returned when a client requests
// an Engine.IO protocol version other than v4. The shape (code 5, message
// "Unsupported protocol version") matches the reference socket.io server so
// that existing clients surface a recognisable error.
const unsupportedEIOVersionBody = `{"code":5,"message":"Unsupported protocol version"}`

// New returns a Fiber handler that upgrades the request to a Socket.IO-
// compatible WebSocket, performs the Engine.IO / Socket.IO handshake, and
// invokes callback with the established Websocket so user code can register
// per-connection state before the read loop and heartbeat start.
//
// Before delegating to the WebSocket upgrader, the handler validates the
// Engine.IO protocol version supplied via the "EIO" query parameter. Only
// EIO v4 is supported; an empty value defaults to v4. Any other value is
// rejected with HTTP 400 and a JSON error body matching the reference
// socket.io server response, so older clients (e.g. EIO v3) surface a
// recognisable, transport-level handshake error instead of being silently
// upgraded into an incompatible session.
func New(callback func(kws *Websocket), config ...websocket.Config) func(fiber.Ctx) error {
	wsHandler := websocket.New(func(c *websocket.Conn) {
		kws := newWebsocket()
		kws.Conn = c
		// The middleware allocates c per upgrade and never reuses it, so
		// closures over it stay valid for the life of the session.
		kws.Locals = func(key string) interface{} {
			return c.Locals(key)
		}
		kws.Params = func(key string, defaultValue ...string) string {
			return c.Params(key, defaultValue...)
		}
		kws.Query = func(key string, defaultValue ...string) string {
			return c.Query(key, defaultValue...)
		}
		kws.Cookies = func(key string, defaultValue ...string) string {
			return c.Cookies(key, defaultValue...)
		}

		// register the connection into the pool
		pool.set(kws)

		// 1. Perform the EIO/SIO handshake synchronously so that the EIO OPEN
		//    packet is the very first frame on the wire. Without this, any
		//    Emit / EmitEvent calls performed inside the user callback would
		//    arrive at the client before the handshake completes and would be
		//    silently dropped by socket.io-client.
		if err := kws.handshake(); err != nil {
			// Handshake errors are particularly easy to miss: at this
			// point the user callback has not run, so no per-conn
			// EventError listener could be attached anyway. Surface
			// through the package-level Logger hook so operators see
			// rejected handshakes in production.
			logf("error", "handshake_failure", "uuid", kws.UUID, "err", err.Error())
			kws.disconnected(err)
			kws.finishRun()
			return
		}

		// 2. Start the send goroutine before invoking the user callback so that
		//    Emit/Broadcast calls inside it are flushed in order.
		ctx, cancelCtx := context.WithCancel(context.Background())
		kws.ctx = ctx
		kws.cancelCtx = cancelCtx
		go kws.send(ctx)

		// 3. Execute the user callback on a fully established socket.
		//    Recover panics so the framework's worker pool stays
		//    healthy and the session is torn down cleanly. Mirrors
		//    the polling path in openPollingSession.
		if r := runUserCallback(callback, kws); r != nil {
			logf("error", "ws_callback_panic", "uuid", kws.UUID, "panic", fmt.Sprintf("%v", r))
			kws.disconnected(fmt.Errorf("socketio: WebSocket callback panic: %v", r))
			kws.finishRun()
			return
		}

		// If the callback actively closed the socket (for example after
		// inspecting HandshakeAuth), do not emit EventConnect for a connection
		// user code already rejected. The closing handshake it queued still
		// runs to completion: read drains until the peer's Close frame or
		// the CloseTimeout deadline, exactly as it would after run.
		if !kws.IsAlive() {
			kws.read()
			kws.finishRun()
			return
		}

		// 4. Notify listeners that the socket is ready.
		kws.fireEvent(EventConnect, nil, nil)

		// 5. Heartbeat and read loop; blocks until the connection closes.
		kws.run()
	}, config...)

	return func(c fiber.Ctx) error {
		// Reject unsupported Engine.IO protocol versions BEFORE the
		// WebSocket upgrade. The reference server returns HTTP 400 with
		// {"code":5,"message":"Unsupported protocol version"} so socket.io
		// clients surface the error instead of opening a half-broken
		// session against an EIO v3 server expectation. An empty EIO
		// query parameter is permitted and defaults to v4 to keep
		// backwards compatibility with non-strict callers and tests
		// that dial the WebSocket endpoint directly.
		if eio := c.Query("EIO"); eio != "" && eio != "4" {
			logf("warn", "eio_version_mismatch", "requested", eio, "supported", "4", "remote", c.IP())
			c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
			return c.Status(fiber.StatusBadRequest).SendString(unsupportedEIOVersionBody)
		}
		// HTTP long-polling fallback: dispatch when the package-level
		// EnablePolling switch is true and the request carries
		// transport=polling. Polling-only sessions speak the same
		// Engine.IO v4 / Socket.IO v5 protocol over HTTP GET/POST that
		// the WebSocket transport carries over a single full-duplex
		// frame stream. See polling.go.
		if EnablePolling {
			if handled, err := handlePolling(c, callback); handled {
				return err
			}
			// Reject requests that carry an explicit transport value
			// other than polling/websocket so a stray "transport=foo"
			// query produces engine.io error code 0 instead of being
			// silently routed to the WebSocket upgrader.
			if t := c.Query("transport"); t != "" && t != "websocket" {
				c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
				return c.Status(fiber.StatusBadRequest).
					SendString(`{"code":0,"message":"Transport unknown"}`)
			}
		}
		return wsHandler(c)
	}
}

// GetUUID returns the unique identifier of this connection in a
// concurrency-safe manner.
func (kws *Websocket) GetUUID() string {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return kws.UUID
}

// handshake performs the Engine.IO / Socket.IO handshake synchronously,
// using direct WebSocket reads/writes (not the message queue) so that the
// EIO OPEN packet is the very first frame the client receives. Without this,
// any Emit calls performed inside the user callback would be queued before
// EIO OPEN and silently dropped by socket.io-client during its opening state.
//
// Sequence:
//  1. Server -> Client: 0{...sid,pingInterval,pingTimeout,maxPayload}
//  2. Client -> Server: 40 (optionally with namespace, e.g. "40/admin,")
//  3. Server -> Client: 40{"sid":"..."}
//
// A rejected CONNECT is answered with CONNECT_ERROR followed by the closing
// handshake (see rejectHandshake) so the client learns why before the
// socket goes away.
func (kws *Websocket) handshake() error {
	conn := kws.Conn
	// Enforce the advertised payload size: prevent malicious clients from
	// streaming arbitrarily large frames into our memory.
	if MaxPayload > 0 {
		conn.SetReadLimit(MaxPayload)
	}

	// 1. Send EIO OPEN
	frame, err := buildEIOOpenFrame(kws.UUID)
	if err != nil {
		return fmt.Errorf("socketio: marshal EIO OPEN: %w", err)
	}
	if err := conn.WriteMessage(TextMessage, frame); err != nil {
		return fmt.Errorf("socketio: write EIO OPEN: %w", err)
	}

	// 2. Wait for client SIO CONNECT, with a deadline so dead clients do not
	//    pin a goroutine forever.
	deadline := time.Time{}
	if HandshakeTimeout > 0 {
		deadline = time.Now().Add(HandshakeTimeout)
	}
	_ = conn.SetReadDeadline(deadline)
	mType, msg, err := conn.ReadMessage()
	if err != nil {
		if websocket.IsUnexpectedCloseError(err) {
			return fmt.Errorf("%w: %w", ErrHandshakeClosed, err)
		}
		return fmt.Errorf("socketio: read SIO CONNECT: %w", err)
	}
	if mType != TextMessage || len(msg) < 2 || msg[0] != eioMessage || msg[1] != sioConnect {
		kws.rejectHandshake(nil, `{"message":"Expected SIO CONNECT"}`)
		return fmt.Errorf("socketio: expected SIO CONNECT (40), got type=%d payload=%q", mType, msg)
	}

	// Extract optional namespace AND optional auth payload from the CONNECT
	// packet. Wire format: "40" [ "/namespace," ] [ <json_auth> ]
	namespace, authPayload := extractSIOConnect(msg[2:])

	// Validate namespace charset to reject malformed prefixes that would
	// otherwise be echoed back verbatim into every outbound emit.
	if !isValidNamespace(namespace) {
		logf("warn", "invalid_namespace", "uuid", kws.UUID, "namespace", string(namespace))
		kws.rejectHandshake(namespace, `{"message":"invalid namespace"}`)
		return ErrInvalidNamespace
	}

	// Validate the auth payload shape per socket.io-protocol v5: auth MUST
	// be either absent (nil) or a syntactically valid JSON object. Reject
	// non-object literals (arrays, scalars, strings) and malformed JSON
	// (truncated, garbage). Also enforce a hard size cap so a malicious
	// client cannot stage a large allocation through the handshake before
	// any user code runs.
	if !isValidAuthPayload(authPayload) {
		logf("warn", "invalid_auth_payload", "uuid", kws.UUID, "namespace", string(namespace), "size", len(authPayload))
		kws.rejectHandshake(namespace, `{"message":"Invalid auth payload"}`)
		return ErrInvalidAuthPayload
	}

	kws.bindNamespace(namespace, authPayload)

	// 3. Send SIO CONNECT confirmation, mirroring the namespace.
	if err := conn.WriteMessage(TextMessage, buildSIOConnectAckSID(kws.getNamespace(), kws.UUID)); err != nil {
		return fmt.Errorf("socketio: write SIO CONNECT: %w", err)
	}
	return nil
}

// rejectHandshake answers a rejected SIO CONNECT: CONNECT_ERROR, then the
// closing handshake. Reading on until the peer's Close frame keeps the
// error packet from being lost to a TCP reset that closing with unread
// inbound data would provoke. One absolute deadline, CloseTimeout from
// now, bounds all of it - the CONNECT_ERROR write, the Close frame and the
// drain - so a peer that never answers costs the budget once. Runs before
// the send goroutine exists, so it writes directly.
func (kws *Websocket) rejectHandshake(namespace []byte, jsonMessage string) {
	conn := kws.Conn
	grace := time.Duration(kws.closeGrace.Load())
	if grace <= 0 {
		// No closing handshake: finishRun closes the socket right away.
		_ = conn.WriteMessage(TextMessage, buildSIOConnectError(namespace, jsonMessage))
		return
	}
	deadline := time.Now().Add(grace)
	_ = conn.SetWriteDeadline(deadline)
	if err := conn.WriteMessage(TextMessage, buildSIOConnectError(namespace, jsonMessage)); err != nil {
		return
	}
	if err := conn.WriteControl(CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "handshake rejected"), deadline); err != nil {
		return
	}
	_ = conn.SetReadDeadline(deadline)
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// bindNamespace records the namespace and auth payload negotiated in the
// SIO CONNECT packet. Both are copied: the frame they arrived in belongs
// to the transport.
func (kws *Websocket) bindNamespace(namespace, auth []byte) {
	if len(namespace) > 0 {
		ns := utils.CopyBytes(namespace)
		kws.namespace.Store(&ns)
	}
	if len(auth) > 0 {
		kws.mu.Lock()
		kws.handshakeAuth = json.RawMessage(utils.CopyBytes(auth))
		kws.mu.Unlock()
	}
}

// SetUUID replaces this connection's UUID, updating the active connections
// pool atomically. It returns ErrorUUIDDuplication when the requested UUID is
// already taken by another live connection.
func (kws *Websocket) SetUUID(uuid string) error {
	pool.Lock()
	defer pool.Unlock()
	kws.mu.Lock()
	defer kws.mu.Unlock()

	prevUUID := kws.UUID
	if prevUUID == uuid {
		return nil
	}

	if existing, ok := pool.conn[uuid]; ok && existing != kws {
		return ErrorUUIDDuplication
	}
	kws.UUID = uuid

	if prevUUID != "" {
		delete(pool.conn, prevUUID)
	}
	pool.conn[uuid] = kws
	return nil
}

// SetAttribute stores a per-connection key/value pair. Concurrency-safe.
// Listeners receive a defensive snapshot via EventPayload.SocketAttributes.
func (kws *Websocket) SetAttribute(key string, attribute interface{}) {
	kws.mu.Lock()
	defer kws.mu.Unlock()
	if kws.attributes == nil {
		kws.attributes = make(map[string]interface{})
	}
	kws.attributes[key] = attribute
}

// GetAttribute returns the per-connection attribute previously stored under
// key, or nil if no such attribute exists. Concurrency-safe.
func (kws *Websocket) GetAttribute(key string) interface{} {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	value, ok := kws.attributes[key]
	if ok {
		return value
	}
	return nil
}

// GetIntAttribute returns the per-connection attribute under key as an int,
// or 0 if no such attribute exists. Panics if the stored value is not an int.
func (kws *Websocket) GetIntAttribute(key string) int {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	value, ok := kws.attributes[key]
	if ok {
		return value.(int)
	}
	return 0
}

// GetStringAttribute returns the per-connection attribute under key as a
// string, or "" if no such attribute exists. Panics if the stored value is
// not a string.
func (kws *Websocket) GetStringAttribute(key string) string {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	value, ok := kws.attributes[key]
	if ok {
		return value.(string)
	}
	return ""
}

// EmitToList sends message to every connection whose UUID appears in uuids.
// Per-target failures are surfaced as EventError on kws by EmitTo (not
// returned). See Websocket.Emit for the meaning of mType.
func (kws *Websocket) EmitToList(uuids []string, message []byte, mType ...int) {
	for _, wsUUID := range uuids {
		_ = kws.EmitTo(wsUUID, message, mType...)
	}
}

// EmitToList is the package-level form of Websocket.EmitToList. It sends
// message to every connection whose UUID appears in uuids. Errors are
// silently ignored; use the method form to receive them via EventError.
func EmitToList(uuids []string, message []byte, mType ...int) {
	for _, wsUUID := range uuids {
		_ = EmitTo(wsUUID, message, mType...)
	}
}

// EmitTo sends message to the connection identified by uuid. Returns the
// underlying pool error (typically ErrorInvalidConnection) when the target is
// unknown or already closed; in either case an EventError is fired on kws so
// fan-out callers (EmitToList, Broadcast) do not have to re-fire it. See
// Websocket.Emit for the meaning of mType.
func (kws *Websocket) EmitTo(uuid string, message []byte, mType ...int) error {
	conn, err := pool.get(uuid)
	if err != nil {
		kws.fireEvent(EventError, []byte(uuid), err)
		return err
	}
	if !conn.IsAlive() {
		kws.fireEvent(EventError, []byte(uuid), ErrorInvalidConnection)
		return ErrorInvalidConnection
	}

	conn.Emit(message, mType...)
	return nil
}

// EmitTo is the package-level form of Websocket.EmitTo. It sends message to
// the connection identified by uuid and returns ErrorInvalidConnection when
// the target is unknown or already closed.
func EmitTo(uuid string, message []byte, mType ...int) error {
	conn, err := pool.get(uuid)
	if err != nil {
		return err
	}

	if !conn.IsAlive() {
		return ErrorInvalidConnection
	}

	conn.Emit(message, mType...)
	return nil
}

// frameType resolves the optional mType argument of the emit APIs.
func frameType(mType []int) int {
	if len(mType) > 0 {
		return mType[0]
	}
	return TextMessage
}

// broadcastFrames caches the encoded "message" event of one broadcast per
// namespace, so a fan-out to N connections builds each frame once rather
// than N times. The root namespace, by far the most common, has its own
// slot so a root-only pool costs a single allocation: the frame itself.
type broadcastFrames struct {
	root   []byte
	ns     [][]byte
	frames [][]byte
}

func (b *broadcastFrames) frame(namespace, message []byte) []byte {
	if len(namespace) == 0 {
		if b.root == nil {
			b.root = buildSIOEvent(nil, EventMessage, message)
		}
		return b.root
	}
	for i := range b.ns {
		if bytes.Equal(b.ns[i], namespace) {
			return b.frames[i]
		}
	}
	f := buildSIOEvent(namespace, EventMessage, message)
	b.ns = append(b.ns, namespace)
	b.frames = append(b.frames, f)
	return f
}

// emitShared is Emit for a fan-out: text messages reuse the per-namespace
// frame from cache. The frame is shared read-only between the send queues
// of every recipient; nothing on the write path mutates queued bytes.
func (kws *Websocket) emitShared(cache *broadcastFrames, message []byte, mType int) {
	if mType != TextMessage {
		kws.write(mType, message)
		return
	}
	kws.write(TextMessage, cache.frame(kws.getNamespace(), message))
}

// Broadcast sends message to every active connection in the pool. When except
// is true the originating connection is skipped. The optional mType selects
// the WebSocket frame type: omit it (or pass TextMessage) to wrap message as
// a Socket.IO "message" event; pass BinaryMessage to send the bytes verbatim
// as a binary frame.
//
// A target that has gone away between the pool snapshot and the emit is
// reported with EventError (ErrorInvalidConnection) on kws, as EmitTo does.
func (kws *Websocket) Broadcast(message []byte, except bool, mType ...int) {
	selfUUID := kws.GetUUID()
	t := frameType(mType)
	var cache broadcastFrames
	for _, target := range pool.snapshot() {
		uuid := target.GetUUID()
		if except && uuid == selfUUID {
			continue
		}
		if !target.IsAlive() {
			kws.fireEvent(EventError, []byte(uuid), ErrorInvalidConnection)
			continue
		}
		if w, ok := target.(*Websocket); ok {
			w.emitShared(&cache, message, t)
			continue
		}
		target.Emit(message, mType...)
	}
}

// Broadcast is the package-level form of Websocket.Broadcast. It sends
// message to every active connection in the pool, including the originator
// (use the method form to skip the originator). See Websocket.Emit for the
// meaning of mType.
func Broadcast(message []byte, mType ...int) {
	t := frameType(mType)
	var cache broadcastFrames
	for _, target := range pool.snapshot() {
		if w, ok := target.(*Websocket); ok {
			w.emitShared(&cache, message, t)
			continue
		}
		target.Emit(message, mType...)
	}
}

// Fire delivers a synthetic event to the listeners registered for event on
// this connection only. It does not produce a wire frame; use it to inject
// internal events from server-side code. The data slice is exposed as the
// listener's EventPayload.Data and as Args[0].
func (kws *Websocket) Fire(event string, data []byte) {
	kws.fireEvent(event, data, nil)
}

// Fire delivers a synthetic event to the listeners registered for event on
// every active connection. It does not produce a wire frame. See
// Websocket.Fire for the per-connection variant.
func Fire(event string, data []byte) {
	fireGlobalEvent(event, data, nil)
}

// Emit sends message to the client wrapped as a Socket.IO "message" event
// (use EmitEvent for named events). The message bytes may be valid JSON
// (object, array, string literal, number, etc.) or raw text; raw text is
// encoded as a JSON string so socket.io-client can parse the frame.
//
// The optional mType selects the WebSocket frame type: omit it (or pass
// TextMessage) for a Socket.IO "message" event; pass BinaryMessage to send
// the bytes verbatim as a binary WebSocket frame. The connection's
// namespace (captured during the handshake) is mirrored on the wire.
//
// Concurrency-safe: enqueues onto the per-connection send queue. Calls on
// already-disconnected sockets are a no-op. Behavior on a full queue is
// governed by DropFramesOnOverflow.
func (kws *Websocket) Emit(message []byte, mType ...int) {
	t := frameType(mType)
	if t == TextMessage {
		kws.write(TextMessage, buildSIOEvent(kws.getNamespace(), EventMessage, message))
	} else {
		kws.write(t, message)
	}
}

// EmitEvent sends a named socket.io event to the client. The data parameter
// may be valid JSON or raw text. The connection's namespace (captured during
// the handshake) is mirrored on the wire.
//
// Reserved event names ("connect", "connect_error", "disconnect") are
// rejected: the call is dropped and EventError fires with
// ErrReservedEventName instead. Concurrency-safe.
func (kws *Websocket) EmitEvent(event string, data []byte) {
	if isReservedEventName(event) {
		kws.fireEvent(EventError, []byte(event), ErrReservedEventName)
		return
	}
	kws.write(TextMessage, buildSIOEvent(kws.getNamespace(), event, data))
}

// EmitArgs sends a named socket.io event with multiple arguments, matching the
// JS-side call socket.emit("event", a, b, c). Valid JSON args are passed
// through; raw text args are encoded as JSON strings. Empty entries are
// skipped. The connection's namespace (captured during the handshake) is
// mirrored on the wire. Reserved event names are rejected as in EmitEvent.
// Concurrency-safe.
func (kws *Websocket) EmitArgs(event string, args ...[]byte) {
	if isReservedEventName(event) {
		kws.fireEvent(EventError, []byte(event), ErrReservedEventName)
		return
	}
	kws.write(TextMessage, buildSIOEventWithAck(kws.getNamespace(), 0, false, event, args))
}

// EmitWithAck sends a named socket.io event and registers a callback that
// is invoked exactly once with the client's ack response. The connection's
// namespace (captured during the handshake) is mirrored on the wire.
//
// The data parameter may be valid JSON or raw text. The callback receives the
// raw JSON bytes from the client's ack ([] for an empty ack, the first
// arg as JSON, or a JSON-array when the client called the ack with
// multiple args).
//
// On a healthy round-trip the callback fires with the ack bytes.
// If the client does not respond within OutboundAckTimeout the callback
// fires with nil. If the connection closes before any ack arrives the
// callback fires with nil. Because both error paths surface as a nil ack,
// callers that need to distinguish "client never replied" from "connection
// torn down" should use EmitWithAckTimeout, which delivers ErrAckTimeout
// versus ErrAckDisconnected through its structured AckCallback.
func (kws *Websocket) EmitWithAck(event string, data []byte, cb func(ack []byte)) {
	if isReservedEventName(event) {
		kws.fireEvent(EventError, []byte(event), ErrReservedEventName)
		if cb != nil {
			cb(nil)
		}
		return
	}
	if cb == nil {
		kws.EmitEvent(event, data)
		return
	}
	kws.EmitWithAckTimeout(event, data, OutboundAckTimeout, func(ack []byte, _ error) {
		cb(ack)
	})
}

// EmitWithAckTimeout is the timeout-aware variant of EmitWithAck. Pass
// timeout = 0 to disable the timeout (the callback only fires when the
// client acks or the connection closes). The connection's namespace
// (captured during the handshake) is mirrored on the wire.
//
// The callback's err is one of: nil (ack received), ErrAckTimeout, or
// ErrAckDisconnected.
func (kws *Websocket) EmitWithAckTimeout(event string, data []byte, timeout time.Duration, cb AckCallback) {
	if isReservedEventName(event) {
		kws.fireEvent(EventError, []byte(event), ErrReservedEventName)
		if cb != nil {
			cb(nil, ErrReservedEventName)
		}
		return
	}
	if cb == nil {
		kws.EmitEvent(event, data)
		return
	}
	id, ok := kws.registerAck(adaptSingleArgAck(cb), timeout)
	if !ok {
		cb(nil, ErrAckDisconnected)
		return
	}
	var args [][]byte
	if len(data) > 0 {
		args = [][]byte{data}
	}
	kws.write(TextMessage, buildSIOEventWithAck(kws.getNamespace(), id, true, event, args))
}

// EmitWithAckArgs is the multi-arg + structured-error variant of
// EmitWithAck. It sends a named event carrying multiple arguments
// and registers a callback invoked exactly once when the client acks, on
// timeout (OutboundAckTimeout), or on connection close. The connection's
// namespace (captured during the handshake) is mirrored on the wire.
//
// On success cb is called with (args, nil) where args is the slice of
// raw-JSON ack arguments the client sent. On timeout cb is called with
// (nil, ErrAckTimeout); on disconnect (nil, ErrAckDisconnected).
func (kws *Websocket) EmitWithAckArgs(event string, args [][]byte, cb func([][]byte, error)) {
	if isReservedEventName(event) {
		kws.fireEvent(EventError, []byte(event), ErrReservedEventName)
		if cb != nil {
			cb(nil, ErrReservedEventName)
		}
		return
	}
	if cb == nil {
		kws.write(TextMessage, buildSIOEventWithAck(kws.getNamespace(), 0, false, event, args))
		return
	}
	// The internal pendingAck callback already takes [][]byte, matching the
	// user signature exactly, so no lossy adapter is needed here. Wire-level
	// "43<id>[a,b]" arrives as args=[a,b]; "43<id>[[a,b]]" arrives as
	// args=[[a,b]]. The two cases are distinguishable for callers.
	id, ok := kws.registerAck(cb, OutboundAckTimeout)
	if !ok {
		cb(nil, ErrAckDisconnected)
		return
	}
	kws.write(TextMessage, buildSIOEventWithAck(kws.getNamespace(), id, true, event, args))
}

// registerAck allocates the next ack id and records cb under it, arming the
// timeout when positive. It reports false when the connection is already
// torn down, in which case nothing was registered.
//
// IsAlive is re-checked while holding the ack mutex: disconnected() takes
// the same mutex when it swaps the pending map. Without this re-check, a
// disconnect that started after the caller's IsAlive() probe but before us
// acquiring the mutex would land our entry in the post-swap (empty) map,
// where it could leak (timeout = 0) or fire with ErrAckTimeout instead of
// the correct ErrAckDisconnected (timeout > 0).
//
// The timeout is armed while still holding the lock so p.timer is published
// to every reader (deliverOutboundAck, fireAckTimeout, disconnected drain)
// under the same mutex that guards the map. fireAckTimeout itself acquires
// the lock asynchronously inside the AfterFunc closure, so this cannot
// deadlock even if the timer fires immediately on a stalled scheduler.
func (kws *Websocket) registerAck(cb func(args [][]byte, err error), timeout time.Duration) (uint64, bool) {
	kws.outboundAcksMu.Lock()
	defer kws.outboundAcksMu.Unlock()
	if !kws.isAlive.Load() {
		return 0, false
	}
	kws.outboundAckSeq++
	id := kws.outboundAckSeq
	p := &pendingAck{cb: cb}
	if timeout > 0 {
		p.timer = time.AfterFunc(timeout, func() { kws.fireAckTimeout(id) })
	}
	if kws.outboundAcks == nil {
		kws.outboundAcks = make(map[uint64]*pendingAck)
	}
	kws.outboundAcks[id] = p
	return id, true
}

// deliverOutboundAck dispatches an incoming ACK to the registered callback,
// if any, and removes it from the pending map. args is the structured slice
// of raw-JSON arguments parsed from "43<id>[a,b,...]"; passing it through as
// [][]byte preserves the boundary between multi-arg acks and single-arg acks
// whose only argument is itself a JSON array.
//
// Two guards collaborate to enforce at-most-once delivery: (1) the
// map-delete-wins pattern under outboundAcksMu serialises which path removes
// the entry, (2) the pendingAck.fired atomic.Bool fences the callback
// invocation itself, so a timer that already captured p before disconnected()
// drained the map cannot race with the disconnect-path callback. fired is the
// authoritative single-fire guard; the mutex protects the map.
func (kws *Websocket) deliverOutboundAck(id uint64, args [][]byte) {
	kws.outboundAcksMu.Lock()
	p, ok := kws.outboundAcks[id]
	if ok {
		delete(kws.outboundAcks, id)
	}
	kws.outboundAcksMu.Unlock()
	if !ok || p == nil {
		return
	}
	if !p.fired.CompareAndSwap(false, true) {
		return
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	if p.cb != nil {
		func() {
			defer func() { _ = recover() }()
			p.cb(args, nil)
		}()
	}
}

// fireAckTimeout is called from time.AfterFunc when the configured ack
// deadline elapses. The map-delete-wins pattern under outboundAcksMu
// removes the entry, and pendingAck.fired (CAS false -> true) is the
// authoritative single-fire guard so a delivery-path callback that
// captured p before us cannot let us double-fire.
func (kws *Websocket) fireAckTimeout(id uint64) {
	kws.outboundAcksMu.Lock()
	p, ok := kws.outboundAcks[id]
	if ok {
		delete(kws.outboundAcks, id)
	}
	kws.outboundAcksMu.Unlock()
	if !ok || p == nil || p.cb == nil {
		return
	}
	if !p.fired.CompareAndSwap(false, true) {
		return
	}
	logf("warn", "ack_timeout", "uuid", kws.UUID, "ack_id", id)
	defer func() { _ = recover() }()
	p.cb(nil, ErrAckTimeout)
}

// adaptSingleArgAck returns an internal [][]byte-shaped ack callback that
// delegates to the public single-arg AckCallback shape: args[0] (or nil if
// args is empty) plus the error are passed through. Used by EmitWithAck and
// EmitWithAckTimeout so the public AckCallback signature stays unchanged.
func adaptSingleArgAck(cb AckCallback) func(args [][]byte, err error) {
	return func(args [][]byte, err error) {
		var ack []byte
		if len(args) > 0 {
			ack = args[0]
		}
		cb(ack, err)
	}
}

// Close actively closes the connection from the server side.
//
// It is idempotent: concurrent or re-entrant callers return immediately
// while the first call proceeds. EventClose fires exactly once, before the
// SIO DISCONNECT packet and the Close control frame are queued behind
// every frame emitted so far, so a listener may still send a final message
// that reaches the peer ahead of them. The regular disconnected tear-down
// (which fires EventDisconnect) follows synchronously; the socket itself
// is closed once the peer answers the Close frame or CloseTimeout elapses.
// Callers may invoke Close from inside an event listener; it does not
// block on the listener's own goroutine.
func (kws *Websocket) Close() {
	if !kws.IsAlive() {
		return
	}
	if !kws.closeStarted.CompareAndSwap(false, true) {
		return
	}

	kws.fireEvent(EventClose, nil, nil)

	// Re-capture the grace period at call time so a caller that lowered
	// CloseTimeout right before closing (a test cleanup, a shutdown hook)
	// is honoured.
	kws.closeGrace.Store(int64(CloseTimeout))

	disconnect := buildSIODisconnect(kws.getNamespace())
	if kws.pollQ != nil {
		// Polling: queue SIO DISCONNECT and EIO CLOSE so the next
		// drain (or any in-flight long-poll) delivers them. They are
		// appended past PollQueueMaxFrames: an EventClose listener
		// may have filled the queue, and the peer must still learn
		// the session is over rather than meet an unknown sid. The
		// queue is closed by disconnected() below, after which
		// further enqueues are silent no-ops. There is no
		// equivalent of the WebSocket Close control frame on
		// polling - the EIO "1" packet is the protocol-level
		// disconnect signal.
		kws.pollQ.enqueueTerminal(disconnect, []byte{eioClose})
	} else {
		kws.beginClose(disconnect)
	}

	kws.disconnected(nil)
}

// beginClose queues the closing handshake behind everything already in the
// send queue: the optional SIO DISCONNECT frame, then a Close control
// frame, both written by the send goroutine. It sets closeRequested so the
// tear-down leaves the socket open for the read loop to consume the peer's
// Close frame. A saturated queue means the peer stopped reading; the wait
// for a slot, the closing handshake and the stalled write then share one
// tear-down budget, CloseTimeout from now, and once it is spent the marker
// is dropped and disconnected closes the socket outright.
func (kws *Websocket) beginClose(disconnect []byte) {
	msg := message{mType: closeFrameMarker, data: disconnect}
	deadline := kws.armCloseDeadline()
	select {
	case kws.queue <- msg:
		kws.closeRequested.Store(true)
		return
	default:
	}
	if deadline.IsZero() {
		return
	}
	wait := time.Until(deadline)
	if wait <= 0 {
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case kws.queue <- msg:
		kws.closeRequested.Store(true)
	case <-timer.C:
		logf("warn", "close_queue_stalled", "uuid", kws.UUID, "queue_cap", cap(kws.queue))
	case <-kws.done:
	}
}

// armCloseDeadline starts the tear-down budget, CloseTimeout from now, if
// no earlier caller did, and returns when it ends. The zero time means
// there is no budget: CloseTimeout is zero and nothing waits.
func (kws *Websocket) armCloseDeadline() time.Time {
	if d := kws.closeDeadline.Load(); d != 0 {
		return time.Unix(0, d)
	}
	grace := time.Duration(kws.closeGrace.Load())
	if grace <= 0 {
		return time.Time{}
	}
	deadline := time.Now().Add(grace)
	if !kws.closeDeadline.CompareAndSwap(0, deadline.UnixNano()) {
		return time.Unix(0, kws.closeDeadline.Load())
	}
	return deadline
}

// getNamespace returns the Socket.IO namespace this connection is bound to,
// or nil for the root namespace.
//
// The returned slice MUST NOT be mutated by callers. namespace is written
// exactly once during the handshake and never reassigned; readers can
// therefore alias the underlying bytes safely. All internal callers feed
// it into append() which never mutates the source.
func (kws *Websocket) getNamespace() []byte {
	if ns := kws.namespace.Load(); ns != nil {
		return *ns
	}
	return nil
}

// HandshakeAuth returns the raw JSON auth payload supplied by the client in
// its SIO CONNECT packet (e.g. `{"token":"..."}`). It returns nil for
// clients that did not send an auth payload.
//
// Typical use: validate the auth payload from inside the New() callback or
// the EventConnect listener, then call kws.Close() if invalid.
func (kws *Websocket) HandshakeAuth() json.RawMessage {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	if len(kws.handshakeAuth) == 0 {
		return nil
	}
	return json.RawMessage(utils.CopyBytes(kws.handshakeAuth))
}

// writeConnectError queues a Socket.IO CONNECT_ERROR ("44") frame for a
// session whose handshake already completed (a late CONNECT the server
// cannot honour). The handshake itself answers rejections directly through
// rejectHandshake, before the send goroutine exists.
func (kws *Websocket) writeConnectError(namespace []byte, jsonMessage string) {
	kws.write(TextMessage, buildSIOConnectError(namespace, jsonMessage))
}

// IsAlive reports whether the connection is still considered active and able
// to deliver outbound frames. Lock-free.
func (kws *Websocket) IsAlive() bool {
	return kws.isAlive.Load()
}

// IsPolling reports whether this session is bound to the HTTP long-
// polling transport rather than to a WebSocket. When true, kws.Conn is
// nil; user code that touches kws.Conn directly must guard with this
// check (or just use the transport-agnostic Emit / Broadcast / Ack /
// Close API, which works on both transports).
func (kws *Websocket) IsPolling() bool {
	return kws.pollQ != nil
}

func (kws *Websocket) setAlive(alive bool) {
	kws.isAlive.Store(alive)
}

//nolint:all
func (kws *Websocket) queueLength() int {
	// kws.queue is a chan whose header is set once in New() and never
	// reassigned; len(chan) is itself atomic. No lock needed.
	return len(kws.queue)
}

// startHeartbeat arms the Engine.IO heartbeat: a PING every PingInterval
// and a tear-down when no frame arrived within PingInterval + PingTimeout.
//
// The settings are read once here; later mutations of the globals do not
// affect a live connection, which keeps tests race-free. The timer fires
// every min(PingInterval, PingTimeout) so the deadline check runs at least
// once per PingTimeout while PINGs still go out only every PingInterval;
// worst-case dead-peer detection latency is PingInterval + PingTimeout +
// tick. A runtime timer replaces the goroutine the heartbeat used to own:
// a thousand idle connections no longer pin a thousand goroutine stacks.
func (kws *Websocket) startHeartbeat() {
	interval := PingInterval
	if interval <= 0 {
		interval = 25 * time.Second
	}
	timeout := PingTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	kws.hbInterval = interval
	kws.hbDeadline = interval + timeout
	kws.hbTick = min(interval, timeout)
	kws.lastPingNanos.Store(time.Now().UnixNano())
	// Published before it is armed, so a tick can never observe a nil
	// timer and lose the chain.
	t := time.AfterFunc(time.Hour, kws.heartbeatTick)
	kws.heartbeat.Store(t)
	t.Reset(kws.hbTick)
}

// heartbeatTick runs on the runtime timer goroutine.
func (kws *Websocket) heartbeatTick() {
	if !kws.IsAlive() {
		return
	}
	now := time.Now()
	if last := kws.lastPongNanos.Load(); last > 0 && now.Sub(time.Unix(0, last)) > kws.hbDeadline {
		logf("warn", "heartbeat_timeout", "uuid", kws.UUID, "deadline_ms", kws.hbDeadline.Milliseconds())
		kws.disconnected(ErrHeartbeatTimeout)
		return
	}
	// Emit a PING only every PingInterval, regardless of tick rate.
	if now.Sub(time.Unix(0, kws.lastPingNanos.Load())) >= kws.hbInterval {
		kws.write(TextMessage, eioPingFrame)
		kws.lastPingNanos.Store(now.UnixNano())
	}
	if t := kws.heartbeat.Load(); t != nil && kws.IsAlive() {
		t.Reset(kws.hbTick)
		// A tear-down that ran between the check above and the Reset has
		// already called Stop; take the Reset back so the session is not
		// retained on the timer heap for another tick.
		if !kws.IsAlive() {
			t.Stop()
		}
	}
}

func (kws *Websocket) stopHeartbeat() {
	if t := kws.heartbeat.Load(); t != nil {
		t.Stop()
	}
}

// write enqueues a message for the send goroutine.
//
// The queue is buffered (cap SendQueueSize). When the queue is full,
// behavior is controlled by DropFramesOnOverflow: when false (default),
// the connection is torn down (legacy behavior); when true, the frame is
// dropped and EventError fires with ErrSendQueueOverflow so the caller is
// not deadlocked when the send goroutine has died (e.g. after disconnected
// fired). Calls on already-disconnected sockets are a no-op.
func (kws *Websocket) write(messageType int, messageBytes []byte) {
	if !kws.IsAlive() {
		return
	}
	if kws.pollQ != nil {
		// Polling transport: append the encoded EIO/SIO frame bytes to
		// the per-session outbound buffer. The next GET long-poll drains
		// it. Engine.IO v4 polling represents binary as the inline
		// "b<base64>" text packet, not a distinct frame type, so binary
		// outbound messages are encoded here so user code can call
		// Emit(data, BinaryMessage) without knowing the active
		// transport.
		frame := messageBytes
		if messageType == BinaryMessage {
			frame = encodePollingBinary(messageBytes)
		}
		switch kws.pollQ.enqueue(frame) {
		case enqueueDroppedQueueFull:
			logf("warn", "poll_queue_overflow_drop", "uuid", kws.UUID, "cap", PollQueueMaxFrames)
			kws.fireEvent(EventError, nil, ErrSendQueueOverflow)
		case enqueueRejectedDisconnect:
			logf("error", "poll_queue_overflow_disconnect", "uuid", kws.UUID, "cap", PollQueueMaxFrames)
			kws.disconnected(ErrSendQueueClosed)
		case enqueueOK:
		}
		return
	}
	select {
	case kws.queue <- message{mType: messageType, data: messageBytes}:
	default:
		if DropFramesOnOverflow {
			// Backpressure: drop the frame and surface an error event,
			// keeping the connection alive for legitimate burst traffic.
			logf("warn", "queue_overflow_drop", "uuid", kws.UUID, "queue_cap", cap(kws.queue))
			kws.fireEvent(EventError, nil, ErrSendQueueOverflow)
			return
		}
		// Queue is full and send is not draining; tear down rather than
		// pin the calling goroutine.
		logf("error", "queue_overflow_disconnect", "uuid", kws.UUID, "queue_cap", cap(kws.queue))
		kws.disconnected(ErrSendQueueClosed)
	}
}

// send drains the outbound message queue and writes frames to the wire. It
// is the only goroutine that writes data frames, so no lock guards the
// connection; the closing frames are written here too, in queue order,
// after which it exits. The middleware coalesces writes made while the
// peer still has frames queued for the read loop, so a burst of replies
// leaves in one syscall.
func (kws *Websocket) send(ctx context.Context) {
	defer close(kws.sendDone)
	conn := kws.Conn
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-kws.queue:
			if msg.mType == closeFrameMarker {
				kws.writeCloseFrames(msg.data)
				return
			}
			if kws.writeTimeout > 0 {
				_ = conn.SetWriteDeadline(time.Now().Add(kws.writeTimeout))
			}
			if err := conn.WriteMessage(msg.mType, msg.data); err != nil {
				kws.disconnected(err)
				return
			}
		}
	}
}

// writeCloseFrames puts the closing handshake on the wire: the SIO
// DISCONNECT packet, when the server initiated the close, then a Close
// control frame. After the Close frame the library refuses further data
// frames (ErrCloseSent), which is what ends the send goroutine's job.
func (kws *Websocket) writeCloseFrames(disconnect []byte) {
	conn := kws.Conn
	grace := kws.writeTimeout
	if grace <= 0 {
		grace = controlWriteTimeout
	}
	if len(disconnect) > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(grace))
		if err := conn.WriteMessage(TextMessage, disconnect); err != nil {
			return
		}
	}
	_ = conn.WriteControl(CloseMessage, closeFramePayload, time.Now().Add(grace))
}

// run arms the heartbeat, installs the control-frame handlers and runs
// the read loop on the calling goroutine (the WebSocket upgrade handler)
// until the connection is torn down; then finishRun releases everything.
// The handshake, send goroutine and EventConnect notification are
// intentionally performed in New() before run() is called, so that any
// Emit/EmitEvent calls inside the user callback are flushed onto an
// already established connection and are not interleaved with handshake
// frames.
func (kws *Websocket) run() {
	kws.installControlHandlers()
	kws.startHeartbeat()
	kws.read()
	kws.finishRun()
}

// installControlHandlers routes RFC 6455 Ping/Pong control frames, which
// the library consumes inside ReadMessage and never surfaces as messages,
// to EventPing / EventPong. A Ping is still answered with a Pong, as the
// library's default handler would. Control frames do not count as
// Engine.IO liveness: a stack that answers pings while the application is
// wedged must not mask its own death.
func (kws *Websocket) installControlHandlers() {
	conn := kws.Conn
	conn.SetPingHandler(func(data string) error {
		if kws.IsAlive() {
			kws.fireEvent(EventPing, []byte(data), nil)
		}
		err := conn.WriteControl(PongMessage, []byte(data), time.Now().Add(controlWriteTimeout))
		if errors.Is(err, websocket.ErrCloseSent) {
			return nil
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil
		}
		return err
	})
	conn.SetPongHandler(func(string) error {
		if kws.IsAlive() {
			kws.fireEvent(EventPong, nil, nil)
		}
		return nil
	})
}

// finishRun is the final cleanup of a session, run exactly once: it lets a
// queued closing handshake reach the wire, closes the socket, joins the
// send goroutine and signals closed. It is the point at which a connection
// stops holding any resource - the upgrade handler returns right after,
// and the middleware leaves the hijacked socket to us.
func (kws *Websocket) finishRun() {
	kws.finishOnce.Do(func() {
		kws.disconnected(nil)
		kws.stopHeartbeat()
		graceful := kws.closeRequested.Load()
		// A queued closing handshake must reach the wire before the
		// send goroutine is told to stop; anything else is stopped
		// first so no more frames go to a peer that is already gone.
		if kws.cancelCtx != nil && !graceful {
			kws.cancelCtx()
		}
		if kws.Conn != nil {
			// Let an in-flight write finish before closing under it:
			// closing is what returns a write stalled on a peer that
			// stopped reading, so the wait is bounded. The bound is one
			// budget for the whole tear-down: what is left of the
			// deadline armed when it began (waiting for queue space and
			// the closing handshake already came out of it), and nothing
			// at all when CloseTimeout is zero.
			var wait time.Duration
			if d := kws.closeDeadline.Load(); d != 0 {
				wait = time.Until(time.Unix(0, d))
			}
			if wait > 0 {
				kws.waitSendDone(wait)
			}
			if kws.cancelCtx != nil {
				kws.cancelCtx()
			}
			kws.closeConn()
			kws.waitSendDone(0)
		} else if kws.cancelCtx != nil {
			kws.cancelCtx()
		}
		kws.unmarkDraining()
		close(kws.closed)
	})
}

// waitSendDone blocks until the send goroutine has exited, or for at most
// d when d is positive. With d <= 0 it waits without bound, which is only
// safe once the socket is closed: a write can no longer stall.
func (kws *Websocket) waitSendDone(d time.Duration) {
	if kws.ctx == nil {
		return // never started
	}
	if d <= 0 {
		<-kws.sendDone
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-kws.sendDone:
	case <-timer.C:
	}
}

// armTeardownDeadline sets the read deadline that returns the read loop:
// the end of the tear-down budget when the closing handshake is queued, so
// the loop drains until the peer's Close frame; as good as immediately
// otherwise. The socket itself is closed by finishRun once the send
// goroutine is out of any write: SetReadDeadline is safe to call
// concurrently with a read, closing under a write is not on every
// net.Conn. The deadline is always a moment ahead rather than in the past
// because a conn that implements deadlines with a timer fires a deadline
// that was reset, not one that was stopped.
func (kws *Websocket) armTeardownDeadline(graceful bool) {
	deadline := time.Now().Add(time.Millisecond)
	if graceful {
		if d := kws.closeDeadline.Load(); d > deadline.UnixNano() {
			deadline = time.Unix(0, d)
		}
	}
	kws.teardownDeadline.Store(deadline.UnixNano())
	_ = kws.Conn.SetReadDeadline(deadline)
}

// closeConn closes the underlying socket, which returns a blocked
// ReadMessage or WriteMessage; closing twice is harmless. The middleware
// flushes what it still holds before closing. Only finishRun (and the
// Shutdown deadline) call it, once the send goroutine is out of its write
// or its grace period has elapsed.
func (kws *Websocket) closeConn() {
	if c := kws.Conn; c != nil {
		_ = c.Close()
	}
}

// read is the single reader of kws.Conn. It runs on the upgrade handler's
// goroutine and returns once ReadMessage fails, which is how every
// tear-down ends: the peer closes, the socket errors, disconnected closes
// it, or the closing-handshake deadline expires.
func (kws *Websocket) read() {
	conn := kws.Conn
	// An idle deadline stays armed on the socket, refreshed at most every
	// quarter of its length: it backs up the heartbeat (a peer that sends
	// nothing for PingInterval + PingTimeout is gone either way) and, more
	// importantly, it is what lets the tear-down's own deadline wake a
	// parked read on every net.Conn - one that implements deadlines with a
	// timer only fires a deadline set while a timer is already running.
	idle := kws.hbDeadline + kws.hbTick
	refreshEvery := idle / 4
	var nextRefresh time.Time
	for {
		if now := time.Now(); kws.IsAlive() && now.After(nextRefresh) {
			_ = conn.SetReadDeadline(now.Add(idle))
			nextRefresh = now.Add(refreshEvery)
			if !kws.IsAlive() {
				// Lost a race with the tear-down: its deadline stands.
				if d := kws.teardownDeadline.Load(); d != 0 {
					_ = conn.SetReadDeadline(time.Unix(0, d))
				}
			}
		}
		mType, msg, err := conn.ReadMessage()
		if err != nil {
			kws.disconnected(kws.readCause(err))
			return
		}

		// Closing handshake in progress: keep consuming frames so the
		// peer's Close frame (or EOF) ends the session cleanly, but
		// dispatch nothing after EventDisconnect.
		if !kws.IsAlive() {
			continue
		}

		// Any application-layer frame counts as proof of life for the
		// heartbeat enforcer.
		kws.lastPongNanos.Store(time.Now().UnixNano())

		switch mType {
		case BinaryMessage:
			// ReadMessage hands over a buffer the caller owns, so the
			// bytes go to listeners without a copy.
			kws.fireEvent(EventMessage, msg, nil)
		case TextMessage:
			if len(msg) > 0 {
				kws.dispatchEIOFrame(msg)
			}
		default:
			// Control frames never reach here: the library handles
			// them inside ReadMessage.
		}
	}
}

// readCause maps a read error to the EventDisconnect cause: nil for a
// clean close (a Close frame with code 1000, 1001 or no code),
// ErrHeartbeatTimeout when the idle deadline expired on a live session
// (the peer sent nothing for PingInterval + PingTimeout, which is what the
// heartbeat timer would have reported a moment later), the error itself
// otherwise.
func (kws *Websocket) readCause(err error) error {
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
		return nil
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() && kws.IsAlive() {
		return ErrHeartbeatTimeout
	}
	return err
}

// dispatchEIOFrame splits an inbound text frame into Engine.IO packets and
// routes each. EIO v4 batches multiple packets in one WebSocket frame,
// separated by ASCII RS (0x1E). Single-packet frames (no separator) take
// the fast path; a batched frame is walked with bytes.IndexByte so a frame
// an attacker fills with separators never materialises a [][]byte (which
// bytes.Split would amplify into millions of slice headers).
func (kws *Websocket) dispatchEIOFrame(msg []byte) {
	if bytes.IndexByte(msg, eioPacketSeparator) < 0 {
		kws.dispatchEIOPacket(msg)
		return
	}
	rest, count := msg, 0
	for len(rest) > 0 {
		if count >= MaxBatchPackets {
			logf("warn", "batched_frame_overflow", "uuid", kws.UUID, "limit", MaxBatchPackets)
			kws.fireEvent(EventError, nil, ErrBatchPacketsExceeded)
			return
		}
		var packet []byte
		if idx := bytes.IndexByte(rest, eioPacketSeparator); idx < 0 {
			packet, rest = rest, nil
		} else {
			packet, rest = rest[:idx], rest[idx+1:]
		}
		if len(packet) == 0 {
			continue
		}
		kws.dispatchEIOPacket(packet)
		count++
		if !kws.IsAlive() {
			return
		}
	}
}

// dispatchEIOPacket routes a single Engine.IO packet (one element of a
// possibly-batched frame).
func (kws *Websocket) dispatchEIOPacket(msg []byte) {
	if len(msg) == 0 {
		return
	}
	switch msg[0] {
	case eioPong:
		// EIO PONG: client's response to our PING.
		kws.fireEvent(EventPong, nil, nil)

	case eioPing:
		// In EIO v4 the SERVER sends PING and the CLIENT replies with
		// PONG. Receiving a PING from the peer means a non-conformant
		// client (or a v3 client). Ignore quietly rather than echoing
		// a PONG that would invert the heartbeat direction.

	case eioClose:
		kws.disconnected(nil)

	case eioUpgrade, eioNoop:
		// Transport upgrade / no-op: ignore.

	case eioMessage:
		// Socket.IO packet wrapped in an EIO MESSAGE.
		kws.handleSIOPacket(msg[1:])

	default:
		// Unknown EIO packet type: surface as an error event.
		kws.fireEvent(EventError, msg, ErrUnknownEIOPacket)
	}
}

// handleSIOPacket processes a Socket.IO packet (the bytes after the "4" EIO prefix).
func (kws *Websocket) handleSIOPacket(payload []byte) {
	if len(payload) == 0 {
		return
	}

	sioType := payload[0]
	// Polling sessions must complete the SIO CONNECT handshake before any
	// other Socket.IO packet is dispatched, mirroring the WebSocket path's
	// CONNECT gating. disconnected() marks the session dead, which also
	// stops ingestPolling's parse loop so frames batched behind the
	// rejected one (including a trailing CONNECT) are never processed.
	if kws.pollQ != nil && !kws.connectFired.Load() && sioType != sioConnect {
		kws.fireEvent(EventError, payload, fmt.Errorf("%w: packet type %q", ErrPollingBeforeConnect, sioType))
		kws.disconnected(ErrPollingBeforeConnect)
		return
	}

	// Capture the optional namespace prefix (e.g., "/admin,") BEFORE
	// stripping so we can verify it matches the namespace bound to this
	// connection. ACK ids are per-namespace per the socket.io v5 spec, so
	// a frame whose namespace does not match the connection's bound
	// namespace must NOT be allowed to fire a pending callback (or event
	// listener) registered on a different namespace.
	packetNS, data := splitSIONamespace(payload[1:])

	switch sioType {
	case sioEvent:
		// Cross-namespace guard: reject events whose namespace prefix does
		// not match the namespace bound to this connection. Otherwise a
		// frame "42/admin,..." arriving on a "/" connection would fire
		// listeners registered on the root namespace.
		if bound := kws.getNamespace(); !bytes.Equal(packetNS, bound) {
			kws.fireEvent(EventError, payload, fmt.Errorf("socketio: cross-namespace event dropped: packet=%q conn=%q", packetNS, bound))
			return
		}
		ackID, hasAck, rest, err := splitSIOAckID(data)
		if err != nil {
			kws.fireEvent(EventError, payload, err)
			return
		}
		eventName, eventArgs, err := parseSIOEvent(rest)
		if err != nil {
			kws.fireEvent(EventError, payload, err)
			return
		}
		// Reserved lifecycle event names ("connect", "disconnect",
		// "connect_error") are fired by the framework only. A client
		// emitting "42[\"connect\",...]" would otherwise double-fire
		// EventConnect listeners and bypass our internal lifecycle.
		if isReservedEventName(eventName) {
			kws.fireEvent(EventError, payload, fmt.Errorf("socketio: client may not emit reserved event %q", eventName))
			return
		}
		kws.fireEventWithAck(eventName, eventArgs, nil, ackID, hasAck)

	case sioDisconnect:
		// Per socket.io-protocol v5, "41/<ns>," targets a single namespace
		// and must NOT tear down sibling namespaces sharing the same EIO
		// connection. This implementation is single-namespace-per-conn,
		// so a matching namespace ends the connection and a foreign one
		// is ignored: a malicious or buggy client cannot kill the conn by
		// addressing a namespace it never joined.
		if !bytes.Equal(packetNS, kws.getNamespace()) {
			return
		}
		if kws.pollQ == nil {
			// Answer with the closing handshake so a peer that keeps
			// the transport open still ends the session promptly.
			kws.beginClose(nil)
		}
		kws.disconnected(nil)

	case sioConnect:
		// For polling sessions the SIO CONNECT packet arrives via the
		// first POST after the OPEN handshake response; the WebSocket
		// path performs CONNECT synchronously inside handshake() and
		// never reaches this case for the initial CONNECT. The polling
		// first-CONNECT branch below mirrors the validation,
		// namespace/auth capture, and EventConnect dispatch that
		// handshake() does for WebSocket sessions.
		ns, auth := extractSIOConnect(payload[1:])
		if !isValidNamespace(ns) {
			kws.writeConnectError(ns, `{"message":"Invalid namespace"}`)
			if kws.pollQ != nil {
				kws.disconnected(ErrInvalidNamespace)
			}
			return
		}
		if kws.pollQ != nil && !kws.connectFired.Load() {
			kws.connectPolling(ns, auth)
			return
		}
		// A CONNECT on an established connection. The same namespace is
		// acknowledged again; a different one cannot be served, because
		// a connection binds exactly one namespace and every event for
		// another would be dropped by the cross-namespace guard, so the
		// client is told instead of being acknowledged into silence.
		if bytes.Equal(ns, kws.getNamespace()) {
			kws.write(TextMessage, buildSIOConnectAckSID(ns, kws.GetUUID()))
			return
		}
		kws.writeConnectError(ns, `{"message":"Invalid namespace"}`)

	case sioAck:
		// 43[/ns,]<id>[<data>] - response to a server-initiated EmitWithAck.
		// Cross-namespace guard: ACK ids are per-namespace per the
		// socket.io v5 spec, so a frame "43/admin,7[...]" arriving on a
		// connection bound to "/" must NOT fire the root-namespace
		// pending callback id 7. Drop silently to keep the original
		// callback waiting for a properly-namespaced ack.
		if !bytes.Equal(packetNS, kws.getNamespace()) {
			return
		}
		ackID, has, rest, err := splitSIOAckID(data)
		if err != nil || !has {
			return
		}
		args, err := parseSIOAckArgs(rest)
		if err != nil {
			return
		}
		kws.deliverOutboundAck(ackID, args)

	default:
		// Unknown SIO packet type (including BINARY_EVENT / BINARY_ACK,
		// whose attachments are not reassembled): surface the payload
		// as a raw message.
		kws.fireEvent(EventMessage, payload, nil)
	}
}

// connectPolling completes the Socket.IO handshake of a polling session on
// its first CONNECT packet: validate auth, persist namespace and auth,
// queue the CONNECT ack, run the user callback, then fire EventConnect
// once. The callback intentionally runs after the CONNECT ack is queued,
// matching the WebSocket handshake order: Emit calls inside the callback
// cannot overtake the namespace connect confirmation.
func (kws *Websocket) connectPolling(ns, auth []byte) {
	if !isValidAuthPayload(auth) {
		logf("warn", "invalid_auth_payload", "uuid", kws.UUID, "namespace", string(ns), "size", len(auth))
		kws.writeConnectError(ns, `{"message":"Invalid auth payload"}`)
		kws.disconnected(ErrInvalidAuthPayload)
		return
	}
	if t := kws.handshakeTimer.Load(); t != nil {
		t.Stop()
	}
	kws.bindNamespace(ns, auth)
	kws.write(TextMessage, buildSIOConnectAckSID(kws.getNamespace(), kws.UUID))
	pollCallback := kws.pollCallback
	kws.pollCallback = nil
	if r := runUserCallback(pollCallback, kws); r != nil {
		logf("error", "polling_callback_panic", "uuid", kws.UUID, "panic", fmt.Sprintf("%v", r))
		kws.disconnected(fmt.Errorf("socketio: polling callback panic: %v", r))
		return
	}
	if !kws.IsAlive() {
		kws.disconnected(nil)
		return
	}
	if kws.connectFired.CompareAndSwap(false, true) {
		kws.fireEvent(EventConnect, nil, nil)
	}
}

// disconnected is the single tear-down entry point.
//
// It is idempotent: only the first invocation fires EventDisconnect /
// EventError, removes the connection from the pool, drains pending ack
// callbacks and closes the done channel. Subsequent calls are no-ops.
// This guarantees "EventDisconnect fires exactly once" even when read,
// send, heartbeat and handshake all hit an error simultaneously.
//
// The socket is closed here unless the closing handshake was queued with
// no error (Websocket.Close, or a client SIO DISCONNECT): then the read
// loop keeps draining, bounded by CloseTimeout through the read deadline,
// so the peer's Close frame is consumed and the SIO DISCONNECT packet
// cannot be lost to a reset. Closing the socket is also what returns a
// read parked in ReadMessage or a write stalled on a peer that stopped
// reading, so no error path can wedge the tear-down.
func (kws *Websocket) disconnected(err error) {
	first := false
	kws.once.Do(func() {
		first = true
		kws.setAlive(false)
		kws.stopHeartbeat()
		// Stop the polling handshake timer if scheduled. Without this,
		// the AfterFunc closure keeps the *Websocket alive on the
		// runtime timer heap until HandshakeTimeout elapses (10s
		// default), bloating live-set under churn.
		if t := kws.handshakeTimer.Load(); t != nil {
			t.Stop()
		}
		switch {
		case kws.pollQ != nil:
			// Polling: release any blocked long-poll drain. Frames
			// already in the buffer are still drainable; subsequent
			// enqueues become no-ops.
			kws.pollQ.close()
		case kws.Conn == nil:
		default:
			// A WebSocket session still owns its socket until finishRun,
			// so it is tracked as draining before anything can wake the
			// read loop: finishRun removes the entry, and a reader that
			// returned before the entry existed would leave a finished
			// session in the set for good. Shutdown waits for these.
			kws.markDraining()
			kws.armCloseDeadline()
			kws.armTeardownDeadline(err == nil && kws.closeRequested.Load())
		}
	})
	if !first {
		return
	}

	// Remove from the pool BEFORE firing user events so that listeners
	// observing the pool do not see this dying connection. A WebSocket
	// session joined the draining set above, so it is in at least one of
	// the two sets at any time.
	pool.delete(kws.GetUUID())

	// Drain pending outbound ack callbacks: invoke each with
	// ErrAckDisconnected so callers can distinguish "ack received" (cb
	// gets ack bytes, err nil) from "connection closed" (cb gets nil,
	// err ErrAckDisconnected) from "client never replied" (cb gets nil,
	// err ErrAckTimeout). The pendingAck.fired CAS guards against a
	// concurrent timer goroutine that already captured p before we
	// swapped the map: whichever caller flips fired false -> true wins,
	// so each callback fires exactly once.
	kws.outboundAcksMu.Lock()
	pending := kws.outboundAcks
	kws.outboundAcks = make(map[uint64]*pendingAck)
	kws.outboundAcksMu.Unlock()
	for _, p := range pending {
		if p == nil {
			continue
		}
		if !p.fired.CompareAndSwap(false, true) {
			continue
		}
		if p.timer != nil {
			p.timer.Stop()
		}
		if p.cb != nil {
			func(cb func(args [][]byte, err error)) {
				defer func() { _ = recover() }()
				cb(nil, ErrAckDisconnected)
			}(p.cb)
		}
	}

	kws.fireEvent(EventDisconnect, nil, err)
	if err != nil {
		kws.fireEvent(EventError, nil, err)
	}

	// Close done last so waiters unblock AFTER user-visible events have fired.
	close(kws.done)

	// A polling session owns no goroutine, so nothing remains to join:
	// release it right away.
	if kws.pollQ != nil {
		kws.finishRun()
	}
}

// Create random UUID for each connection
func (kws *Websocket) createUUID() string {
	return kws.randomUUID()
}

// Generate random UUID.
func (kws *Websocket) randomUUID() string {
	return utils.UUIDv4()
}

// Fires event on all connections.
func fireGlobalEvent(event string, data []byte, error error) {
	for _, kws := range pool.snapshot() {
		kws.fireEvent(event, data, error)
	}
}

// Checks if there is at least a listener for a given event
// and loop over the callbacks registered
func (kws *Websocket) fireEvent(event string, data []byte, error error) {
	var args [][]byte
	if data != nil {
		args = [][]byte{data}
	}
	kws.fireEventWithAck(event, args, error, 0, false)
}

// fireEventWithAck is the ack-id aware multi-arg variant. ackID/hasAck are
// forwarded to listeners via EventPayload so handlers can call
// payload.Ack(...).
//
// args holds the raw-JSON event arguments. Data is populated from args[0]
// for backwards compatibility with handlers that consume the single-arg
// shape. SocketAttributes is a defensive copy so listeners cannot race
// with concurrent SetAttribute mutations; a connection without attributes
// dispatches nil rather than allocating an empty map per event.
func (kws *Websocket) fireEventWithAck(event string, args [][]byte, fireErr error, ackID uint64, hasAck bool) {
	callbacks := listeners.get(event)
	if len(callbacks) == 0 {
		return
	}

	kws.mu.RLock()
	uuid := kws.UUID
	attrs := maps.Clone(kws.attributes)
	var auth json.RawMessage
	if event == EventConnect && len(kws.handshakeAuth) > 0 {
		auth = json.RawMessage(utils.CopyBytes(kws.handshakeAuth))
	}
	kws.mu.RUnlock()

	// Single ack-sent guard shared across every listener dispatch for this
	// event, so two listeners that both call payload.Ack(...) produce only
	// one "43" frame on the wire.
	var ackGuard *atomic.Bool
	if hasAck {
		ackGuard = new(atomic.Bool)
	}

	var firstArg []byte
	if len(args) > 0 {
		firstArg = args[0]
	}

	for _, callback := range callbacks {
		// Recover from listener panics so one buggy handler cannot kill
		// the read goroutine (and therefore the whole connection). Surface
		// the panic value as an EventError event so the user can wire it
		// up to logging. When the panicking handler is itself an EventError
		// listener we skip the re-fire to avoid an unbounded recursion (a
		// panicky EventError handler would otherwise keep refiring itself
		// until the goroutine stack overflows).
		func(cb eventCallback) {
			defer func() {
				if r := recover(); r != nil {
					logf("error", "listener_panic", "uuid", uuid, "event", event, "panic", fmt.Sprintf("%v", r))
					if event != EventError {
						kws.fireEvent(EventError, nil, fmt.Errorf("socketio: listener panic on %q: %v", event, r))
					}
				}
			}()
			cb(&EventPayload{
				Kws:              kws,
				Name:             event,
				SocketUUID:       uuid,
				SocketAttributes: attrs,
				Data:             firstArg,
				Args:             args,
				Error:            fireErr,
				AckID:            ackID,
				HasAck:           hasAck,
				HandshakeAuth:    auth,
				ackSent:          ackGuard,
			})
		}(callback)
	}
}

type eventCallback func(payload *EventPayload)

// On registers callback for the named event. The callback fires for every
// connection that receives event, in registration order. Multiple callbacks
// may be registered for the same event; all run synchronously on the
// connection's read goroutine.
//
// On is concurrency-safe and may be called at any time, including from
// inside another listener; later registrations may or may not be observed
// by concurrent dispatch loops (eventual consistency). Listeners cannot be
// unregistered. A listener that calls payload.Ack must do so synchronously
// or pass the payload to a goroutine that copies any byte slices it needs.
func On(event string, callback eventCallback) {
	listeners.set(event, callback)
}

// Shutdown closes every active socket.io connection in the pool and waits
// for each to release its socket and goroutines, or until ctx is cancelled.
// Connections that were already closed but are still draining their
// closing handshake are waited for as well; whatever is still in flight
// when ctx expires is closed outright.
//
// Wire this into fiber.App.Shutdown / fiber.App.ShutdownWithContext so an
// application shutdown deterministically tears down sockets instead of
// relying on the framework to close the underlying transport.
//
// Returns ctx.Err() when ctx is cancelled before all connections finished
// draining; otherwise returns nil.
func Shutdown(ctx context.Context) error {
	conns := pool.snapshot()
	inFlight := drainingSessions()
	if len(conns) == 0 && len(inFlight) == 0 {
		return nil
	}
	seen := make(map[*Websocket]struct{}, len(conns)+len(inFlight))
	sockets := make([]*Websocket, 0, len(conns)+len(inFlight))
	for _, c := range conns {
		if kws, ok := c.(*Websocket); ok {
			seen[kws] = struct{}{}
			sockets = append(sockets, kws)
		}
	}
	for _, kws := range inFlight {
		if _, dup := seen[kws]; !dup {
			sockets = append(sockets, kws)
		}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, kws := range sockets {
		wg.Add(1)
		go func(k *Websocket) {
			defer wg.Done()
			k.Close() // a no-op for a session that is already draining
			select {
			case <-k.closed:
			case <-ctx.Done():
			}
		}(kws)
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		for _, k := range sockets {
			k.closeConn()
		}
		return ctx.Err()
	}
}
