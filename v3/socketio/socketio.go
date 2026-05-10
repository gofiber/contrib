package socketio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
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
	defaultMaxPayload = 1_000_000
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

// Supported event list
const (
	// EventMessage is fired when a text or binary message is received that is
	// not bound to a named Socket.IO event (i.e. wire-level "message" events
	// or raw binary frames).
	EventMessage = "message"
	// EventPing is fired when a WebSocket PING control frame is received
	// from the peer. See
	// https://developer.mozilla.org/en-US/docs/Web/API/WebSockets_API/Writing_WebSocket_servers#Pings_and_Pongs_The_Heartbeat_of_WebSockets
	EventPing = "ping"
	// EventPong is fired when a WebSocket PONG control frame, or an
	// Engine.IO PONG packet, is received from the peer.
	EventPong = "pong"
	// EventDisconnect is fired exactly once when the connection is torn
	// down, regardless of which side initiated the close. The
	// EventPayload.Error field carries the close reason (RFC 6455 section
	// 11.7) when available, or nil for a clean shutdown.
	EventDisconnect = "disconnect"
	// EventConnect is fired exactly once after the Engine.IO and Socket.IO
	// handshake completes, before the read loop starts dispatching events.
	// EventPayload.HandshakeAuth carries the client's auth payload, if any.
	EventConnect = "connect"
	// EventClose is fired exactly once when the connection is closed from
	// the server side via Websocket.Close.
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
	// ErrUnknownEIOPacket is surfaced via EventError when the inbound EIO
	// packet type byte does not match any recognised Engine.IO opcode.
	ErrUnknownEIOPacket = errors.New("socketio: unknown EIO packet type")
)

// reservedEventNames is the set of outbound event names the JS socket.io
// client treats as built-in lifecycle events. Emitting any of these by
// name from the server would either be silently swallowed or trigger
// spurious lifecycle handlers on the client.
//
// Only wire-level reserved names are blocked here. Node EventEmitter
// internals (disconnecting, newListener, removeListener) never appear
// on the wire and must not constrain user event names.
var reservedEventNames = map[string]struct{}{
	"connect":       {},
	"connect_error": {},
	"disconnect":    {},
}

// isReservedEventName reports whether name is a reserved socket.io
// lifecycle event name that must not be used as a custom event name.
func isReservedEventName(name string) bool {
	_, ok := reservedEventNames[name]
	return ok
}

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
	// RetrySendTimeout is the back-off delay the send goroutine waits
	// before retrying a failed write to a temporarily unavailable
	// connection.
	RetrySendTimeout = 20 * time.Millisecond
	// MaxSendRetry is the maximum number of times the send goroutine
	// retries a frame against a missing connection before dropping it.
	MaxSendRetry = 5
	// ReadTimeout is no longer consulted by the read loop, which now
	// blocks in a single ReadMessage call gated by SetReadDeadline rather
	// than busy-polling with a sleep.
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
// heartbeat goroutine puts on the wire. Sharing one slice across every
// emit avoids a per-tick allocation; the underlying bytes are never
// mutated by Conn.WriteMessage.
var eioPingFrame = []byte{eioPing}

// Raw form of websocket message
type message struct {
	// Message type
	mType int
	// Message data
	data []byte
	// Message send retries when error
	retries int
}

// EventPayload is the read-only value passed to every event listener. It
// carries the originating connection (Kws), the event arguments (Args, with
// Data as a shortcut to the first arg), ack bookkeeping (AckID, HasAck, Ack)
// and the handshake auth payload (HandshakeAuth, populated for EventConnect
// listeners only).
//
// Byte-slice fields (Args, Data, HandshakeAuth) carry their own backing
// storage independent of the read buffer: parseSIOEvent copies inbound
// args, the read goroutine copies binary frames before dispatch, and
// HandshakeAuth is captured during the handshake. Listeners may safely
// retain these slices across goroutine boundaries.
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
	// the live connection; use Kws.SetAttribute for that.
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

// eioOpenPacket holds the JSON payload sent in the Engine.IO OPEN packet.
type eioOpenPacket struct {
	SID          string   `json:"sid"`
	Upgrades     []string `json:"upgrades"`
	PingInterval int      `json:"pingInterval"`
	PingTimeout  int      `json:"pingTimeout"`
	MaxPayload   int      `json:"maxPayload"`
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

// buildEIOOpenFrame returns the full Engine.IO OPEN frame bytes
// (`0{"sid":...,"upgrades":[],"pingInterval":N,"pingTimeout":N,"maxPayload":N}`)
// for a freshly opened session. Used by both the WebSocket handshake
// path and the polling open handler. The empty "upgrades" array
// signals "no transport upgrade available" per Engine.IO v4.
func buildEIOOpenFrame(sid string) ([]byte, error) {
	maxPayload := int(MaxPayload)
	if maxPayload <= 0 {
		maxPayload = defaultMaxPayload
	}
	data, err := json.Marshal(eioOpenPacket{
		SID:          sid,
		Upgrades:     []string{},
		PingInterval: int(PingInterval.Milliseconds()),
		PingTimeout:  int(PingTimeout.Milliseconds()),
		MaxPayload:   maxPayload,
	})
	if err != nil {
		return nil, err
	}
	return append([]byte{eioOpen}, data...), nil
}

// buildSIOEvent encodes a Socket.IO EVENT packet ready to send over the wire.
// Format: 4 2 [/<namespace>,] [ "<event>" , <data> ]
//
// data may be valid JSON (object, array, string, number, etc.) or raw text.
// Raw text is encoded as a JSON string for compatibility with earlier
// versions that accepted arbitrary bytes in Emit.
// namespace may be nil for the root namespace.
func buildSIOEvent(namespace []byte, event string, data []byte) []byte {
	if len(data) == 0 {
		return buildSIOEventWithAck(namespace, 0, false, event, nil)
	}
	return buildSIOEventWithAck(namespace, 0, false, event, [][]byte{data})
}

// buildSIOEventWithAck is the ack-id aware multi-arg variant of buildSIOEvent.
//
// args is the slice of arguments to encode after the event name (matches the
// JS-side socket.emit("event", a, b, c) shape). Entries that are valid JSON are
// passed through unchanged; raw text entries are encoded as JSON strings.
// Nil/empty entries are skipped.
//
// The output buffer is pre-sized so a typical event allocates exactly
// once instead of growing through 8/16/32/... append boundaries.
func buildSIOEventWithAck(namespace []byte, ackID uint64, hasAck bool, event string, args [][]byte) []byte {
	name, _ := json.Marshal(event)
	var buf []byte
	buf = append(buf, eioMessage, sioEvent)
	if len(namespace) > 0 {
		buf = append(buf, namespace...)
		buf = append(buf, ',')
	}
	if hasAck {
		buf = strconv.AppendUint(buf, ackID, 10)
	}
	buf = append(buf, '[')
	buf = append(buf, name...)
	for _, a := range args {
		if len(a) == 0 {
			continue
		}
		buf = append(buf, ',')
		buf = append(buf, normalizeJSONArg(a)...)
	}
	buf = append(buf, ']')
	return buf
}

func normalizeJSONArg(data []byte) []byte {
	if len(data) == 0 || json.Valid(data) {
		return data
	}
	encoded, err := json.Marshal(string(data))
	if err != nil {
		return []byte("null")
	}
	return encoded
}

// buildSIOAck encodes a Socket.IO ACK ("43") packet.
//
// Format: 4 3 [/<namespace>,] <ackID> [ <args> ]
// args may be nil/empty to send `43<id>[]`. Valid JSON args are passed
// through; raw text args are encoded as JSON strings.
func buildSIOAck(namespace []byte, ackID uint64, args [][]byte) []byte {
	var buf []byte
	buf = append(buf, eioMessage, sioAck)
	if len(namespace) > 0 {
		buf = append(buf, namespace...)
		buf = append(buf, ',')
	}
	buf = strconv.AppendUint(buf, ackID, 10)
	buf = append(buf, '[')
	first := true
	for _, a := range args {
		if len(a) == 0 {
			continue
		}
		if !first {
			buf = append(buf, ',')
		}
		buf = append(buf, normalizeJSONArg(a)...)
		first = false
	}
	buf = append(buf, ']')
	return buf
}

// splitSIOAckID extracts the optional leading numeric ack ID from a SIO
// EVENT or ACK payload (the bytes after any namespace stripping). It
// returns the ID, a flag whether one was present, and the remaining bytes.
//
// Hand-rolled digit accumulation avoids the string allocation that
// strconv.ParseUint(string(data[:i])) would force on the hot inbound path.
func splitSIOAckID(data []byte) (id uint64, has bool, rest []byte, err error) {
	var v uint64
	i := 0
	for i < len(data) && data[i] >= '0' && data[i] <= '9' {
		d := uint64(data[i] - '0')
		if v > (math.MaxUint64-d)/10 {
			return 0, false, data, ErrAckIDOverflow
		}
		v = v*10 + d
		i++
	}
	if i == 0 {
		return 0, false, data, nil
	}
	return v, true, data[i:], nil
}

// parseSIOEvent parses the JSON-array payload of a Socket.IO EVENT packet.
//
// payload is the bytes after the "42" prefix, e.g. `["message",{"key":"val"}]`.
// It returns the event name and a slice of raw-JSON arguments (one entry
// per element after the event name). args is nil for events without
// arguments.
//
// Argument bytes are copied so callers may safely retain them past the
// next ReadMessage() (the underlying read buffer in github.com/fasthttp/websocket
// is reused on the next read). One pooled allocation amortises the copy.
func parseSIOEvent(payload []byte) (string, [][]byte, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(payload, &arr); err != nil {
		return "", nil, fmt.Errorf("socketio: failed to parse event payload: %w", err)
	}
	if len(arr) == 0 {
		return "", nil, ErrEmptyEventArray
	}
	var eventName string
	if err := json.Unmarshal(arr[0], &eventName); err != nil {
		return "", nil, fmt.Errorf("socketio: failed to parse event name: %w", err)
	}
	if MaxEventNameLength > 0 && len(eventName) > MaxEventNameLength {
		return "", nil, fmt.Errorf("socketio: event name exceeds MaxEventNameLength (%d)", MaxEventNameLength)
	}
	if len(arr) == 1 {
		return eventName, nil, nil
	}
	// Allocate one contiguous backing buffer for all args and slice into it
	// so we never alias the read buffer. One alloc total instead of N.
	total := 0
	for _, raw := range arr[1:] {
		total += len(raw)
	}
	buf := make([]byte, total)
	args := make([][]byte, 0, len(arr)-1)
	off := 0
	for _, raw := range arr[1:] {
		n := copy(buf[off:], raw)
		args = append(args, buf[off:off+n:off+n])
		off += n
	}
	return eventName, args, nil
}

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
	pong(ctx context.Context)
	write(messageType int, messageBytes []byte)
	run()
	read(ctx context.Context)
	disconnected(err error)
	createUUID() string
	randomUUID() string
	fireEvent(event string, data []byte, error error)
}

// Websocket represents a single Socket.IO connection on top of the underlying
// Fiber WebSocket. It carries the per-connection state (UUID, namespace,
// attributes, ack bookkeeping) and exposes the Emit/Broadcast/Close API that
// user code interacts with from inside listener callbacks.
type Websocket struct {
	once sync.Once
	// closeOnce guards the synchronous DISCONNECT + Close-frame write block
	// in Close() so that exactly one goroutine ever performs those writes,
	// regardless of how many concurrent Close() callers race in. Without
	// this, a second Close() racing in could acquire kws.mu after the first
	// released it but before disconnected() flipped isAlive=false, double-
	// writing the close frames and (worse) racing the upgrade handler's
	// deferred releaseConn() that nils the embedded *fasthttp.Conn.
	closeOnce sync.Once
	mu        sync.RWMutex
	// Conn is the underlying Fiber WebSocket connection. Treat it as
	// read-only from listener callbacks; writes must go through the
	// Emit/EmitEvent/Broadcast methods so the send goroutine remains
	// the sole writer.
	Conn *websocket.Conn
	// isAlive reports whether the connection is alive. Accessed lock-free
	// from every emit path (write/EmitTo/etc.) and the read goroutine.
	isAlive atomic.Bool
	// handlerDone flips just before run returns to the websocket upgrader.
	// After that point the vendored websocket package may release and nil the
	// embedded fasthttp connection, so external Close callers must not touch
	// Conn anymore.
	handlerDone atomic.Bool
	// Queue of messages sent from the socket
	queue chan message
	// Channel to signal when this websocket is closed
	// so go routines will stop gracefully
	done chan struct{}
	// ctx is the lifetime context for read/send/pong goroutines.
	ctx context.Context
	// cancelCtx cancels ctx when the connection is torn down.
	cancelCtx context.CancelFunc
	// namespace is the Socket.IO namespace this connection belongs to. Empty
	// means the root namespace. Captured during the handshake from the
	// client's CONNECT packet so outbound events can mirror it.
	namespace []byte
	// handshakeAuth is the raw JSON auth payload supplied by the client in
	// its SIO CONNECT packet (e.g. `{"token":"..."}`). nil for clients that
	// connect without an auth payload.
	handshakeAuth json.RawMessage
	// lastPongNanos is the unix-nano timestamp of the last frame received
	// from the client. The pong ticker uses it to enforce the heartbeat
	// timeout (PingInterval + PingTimeout) and disconnect dead peers.
	lastPongNanos atomic.Int64
	// outboundAckSeq is the monotonic counter for ack ids on emits issued
	// via EmitWithAck. It is incremented under outboundAcksMu.
	outboundAckSeq uint64
	// outboundAcks tracks pending callbacks for server-initiated emits that
	// asked for an ack from the client. The map is keyed by ack id and
	// holds a callback plus an optional timeout timer.
	outboundAcks   map[uint64]*pendingAck
	outboundAcksMu sync.Mutex
	// workersWg tracks the send/pong/read goroutines so the upgrade handler
	// does not return (and the framework does not release Conn) until every
	// goroutine that touches kws.Conn has exited.
	workersWg sync.WaitGroup
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
	// per-session FIFO guarantee that user listeners rely on. Held only
	// for the duration of ingestPolling's parse loop; never held across
	// other locks.
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

type safePool struct {
	sync.RWMutex
	// List of the connections alive
	conn map[string]ws
}

// Pool with the active connections
var pool = safePool{
	conn: make(map[string]ws),
}

func (p *safePool) set(ws ws) {
	p.Lock()
	p.conn[ws.GetUUID()] = ws
	p.Unlock()
}

func (p *safePool) all() map[string]ws {
	p.RLock()
	ret := make(map[string]ws, 0)
	for wsUUID, kws := range p.conn {
		ret[wsUUID] = kws
	}
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
	p.Unlock()
}

//nolint:all
func (p *safePool) reset() {
	p.Lock()
	p.conn = make(map[string]ws)
	p.Unlock()
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
	for k, v := range *cur {
		next[k] = v
	}
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
// per-connection state before the read and heartbeat goroutines start.
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
		kws := &Websocket{
			Conn: c,
			Locals: func(key string) interface{} {
				return c.Locals(key)
			},
			Params: func(key string, defaultValue ...string) string {
				return c.Params(key, defaultValue...)
			},
			Query: func(key string, defaultValue ...string) string {
				return c.Query(key, defaultValue...)
			},
			Cookies: func(key string, defaultValue ...string) string {
				return c.Cookies(key, defaultValue...)
			},
			queue: make(chan message, SendQueueSize),
			done:  make(chan struct{}, 1),
			// attributes and outboundAcks are lazy-initialised on first
			// SetAttribute / EmitWithAck* call. Most idle connections never
			// touch them; deferring the allocation saves ~560 B per conn at
			// scale (1k idle conns => ~560 KB).
		}
		kws.isAlive.Store(true)
		kws.lastPongNanos.Store(time.Now().UnixNano())

		// Generate uuid
		kws.UUID = kws.createUUID()

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
			return
		}

		// 2. Start the send goroutine before invoking the user callback so that
		//    Emit/Broadcast calls inside it are flushed in order.
		ctx, cancelCtx := context.WithCancel(context.Background())
		kws.ctx = ctx
		kws.cancelCtx = cancelCtx
		kws.workersWg.Add(1)
		go func() { defer kws.workersWg.Done(); kws.send(ctx) }()

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
		// user code already rejected.
		if !kws.IsAlive() {
			kws.finishRun()
			return
		}

		// 4. Notify listeners that the socket is ready.
		kws.fireEvent(EventConnect, nil, nil)

		// 5. Read / heartbeat goroutines and block until the connection closes.
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
func (kws *Websocket) handshake() error {
	// Enforce the advertised payload size: prevent malicious clients from
	// streaming arbitrarily large frames into our memory.
	if MaxPayload > 0 {
		kws.Conn.SetReadLimit(MaxPayload)
	}

	// 1. Send EIO OPEN
	frame, err := buildEIOOpenFrame(kws.UUID)
	if err != nil {
		return fmt.Errorf("socketio: marshal EIO OPEN: %w", err)
	}
	if err := kws.Conn.WriteMessage(TextMessage, frame); err != nil {
		return fmt.Errorf("socketio: write EIO OPEN: %w", err)
	}

	// 2. Wait for client SIO CONNECT, with a deadline so dead clients do not
	//    pin a goroutine forever.
	deadline := time.Time{}
	if HandshakeTimeout > 0 {
		deadline = time.Now().Add(HandshakeTimeout)
	}
	_ = kws.Conn.SetReadDeadline(deadline)
	defer func() { _ = kws.Conn.SetReadDeadline(time.Time{}) }()

	mType, msg, err := kws.Conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("socketio: read SIO CONNECT: %w", err)
	}
	if mType == CloseMessage {
		return ErrHandshakeClosed
	}
	if mType != TextMessage || len(msg) < 2 || msg[0] != eioMessage || msg[1] != sioConnect {
		return fmt.Errorf("socketio: expected SIO CONNECT (40), got type=%d payload=%q", mType, msg)
	}

	// Extract optional namespace AND optional auth payload from the CONNECT
	// packet. Wire format: "40" [ "/namespace," ] [ <json_auth> ]
	namespace, authPayload := extractSIOConnect(msg[2:])

	// Validate namespace charset to reject malformed prefixes that would
	// otherwise be echoed back verbatim into every outbound emit.
	if !isValidNamespace(namespace) {
		logf("warn", "invalid_namespace", "uuid", kws.UUID, "namespace", string(namespace))
		_ = kws.writeConnectError(namespace, `{"message":"invalid namespace"}`)
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
		_ = kws.writeConnectError(namespace, `{"message":"Invalid auth payload"}`)
		return ErrInvalidAuthPayload
	}

	// Store as a fresh slice (msg's backing buffer is owned by the read
	// loop and may be reused) so concurrent readers via getNamespace see
	// stable bytes.
	nsCopy := make([]byte, len(namespace))
	copy(nsCopy, namespace)
	kws.mu.Lock()
	kws.namespace = nsCopy
	if len(authPayload) > 0 {
		kws.handshakeAuth = make(json.RawMessage, len(authPayload))
		copy(kws.handshakeAuth, authPayload)
	}
	kws.mu.Unlock()

	// 3. Send SIO CONNECT confirmation, mirroring the namespace.
	payload, err := json.Marshal(struct {
		SID string `json:"sid"`
	}{SID: kws.UUID})
	if err != nil {
		return fmt.Errorf("socketio: marshal SIO CONNECT: %w", err)
	}
	ack := buildSIOConnectAck(namespace, payload)
	if err := kws.Conn.WriteMessage(TextMessage, ack); err != nil {
		return fmt.Errorf("socketio: write SIO CONNECT: %w", err)
	}
	return nil
}

// extractSIONamespace returns the namespace bytes (including the leading "/")
// from a Socket.IO CONNECT or DISCONNECT payload (the bytes after the "40"/"41"
// type prefix). Returns nil for the root namespace.
func extractSIONamespace(data []byte) []byte {
	ns, _ := extractSIOConnect(data)
	return ns
}

// extractSIOConnect parses the bytes after the "40" type prefix of a SIO
// CONNECT packet and returns both the optional namespace (including the
// leading "/", or nil for root) AND the optional JSON auth payload.
//
// Wire format: [ "/" namespace "," ] [ <json> ]
//
// Examples:
//
//	""                   -> (nil, nil)
//	"{"token":"x"}"      -> (nil, `{"token":"x"}`)
//	"/admin,"            -> ("/admin", nil)
//	"/admin,{"k":1}"     -> ("/admin", `{"k":1}`)
//	"/admin"             -> ("/admin", nil)   // no comma, no auth
func extractSIOConnect(data []byte) (namespace, auth []byte) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[0] != '/' {
		return nil, data
	}
	if idx := bytes.IndexByte(data, ','); idx >= 0 {
		ns := data[:idx]
		rest := data[idx+1:]
		if len(rest) == 0 {
			return ns, nil
		}
		return ns, rest
	}
	// Namespace without trailing comma (no auth payload).
	return data, nil
}

// isValidNamespace returns true when ns matches the conservative subset of
// the socket.io namespace grammar: empty (root), or "/<segment>" with at
// least one byte and only [A-Za-z0-9._\-/] characters. We deliberately
// reject characters that would change framing if echoed verbatim into a
// "42<ns>,..." event packet.
func isValidNamespace(ns []byte) bool {
	if len(ns) == 0 {
		return true
	}
	if ns[0] != '/' {
		return false
	}
	if len(ns) == 1 {
		// Lone "/" is treated as the root namespace by socket.io and is
		// accepted here.
		return true
	}
	for i := 1; i < len(ns); i++ {
		c := ns[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == '.' || c == '/':
		default:
			return false
		}
	}
	return true
}

// isValidAuthPayload returns true when the auth payload extracted from a
// SIO CONNECT packet conforms to socket.io-protocol v5: either absent (nil
// / empty), or a syntactically valid JSON object (i.e. first non-whitespace
// byte is '{'). Arrays, scalars, strings, and malformed JSON are rejected.
//
// Also enforces the global MaxAuthPayload cap so an oversized auth payload
// can never reach kws.handshakeAuth or user-visible state.
func isValidAuthPayload(auth []byte) bool {
	if len(auth) == 0 {
		return true
	}
	if MaxAuthPayload > 0 && len(auth) > MaxAuthPayload {
		return false
	}
	// First non-whitespace byte must be '{' (JSON object). RFC 8259
	// whitespace: SP / HT / LF / CR.
	first := byte(0)
	for i := 0; i < len(auth); i++ {
		c := auth[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		first = c
		break
	}
	if first != '{' {
		return false
	}
	return json.Valid(auth)
}

// buildSIOConnectAck encodes a SIO CONNECT ack frame ("40[/ns,]<json>").
func buildSIOConnectAck(namespace, payload []byte) []byte {
	out := []byte{eioMessage, sioConnect}
	if len(namespace) > 0 {
		out = append(out, namespace...)
		out = append(out, ',')
	}
	return append(out, payload...)
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
	kws.UUID = uuid

	if existing, ok := pool.conn[uuid]; ok && existing != kws {
		kws.UUID = prevUUID
		return ErrorUUIDDuplication
	}

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
// Per-target failures are surfaced as EventError on kws (not returned). See
// Websocket.Emit for the meaning of mType.
func (kws *Websocket) EmitToList(uuids []string, message []byte, mType ...int) {
	for _, wsUUID := range uuids {
		err := kws.EmitTo(wsUUID, message, mType...)
		if err != nil {
			kws.fireEvent(EventError, message, err)
		}
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

// EmitTo sends message to the connection identified by uuid. Returns
// ErrorInvalidConnection when the target is unknown or already closed; in
// that case an EventError is also fired on kws. See Websocket.Emit for the
// meaning of mType.
func (kws *Websocket) EmitTo(uuid string, message []byte, mType ...int) error {

	conn, err := pool.get(uuid)
	if err != nil {
		return err
	}
	// pool.get already returned a hit; we only need to verify the conn is
	// still alive. Dropping the redundant pool.contains saves one RWMutex
	// RLock per call - meaningful in Broadcast/EmitToList fanout paths.
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

// Broadcast sends message to every active connection in the pool. When except
// is true the originating connection is skipped. The optional mType selects
// the WebSocket frame type: omit it (or pass TextMessage) to wrap message as
// a Socket.IO "message" event; pass BinaryMessage to send the bytes verbatim
// as a binary frame.
func (kws *Websocket) Broadcast(message []byte, except bool, mType ...int) {
	selfUUID := kws.GetUUID()
	for wsUUID := range pool.all() {
		if except && selfUUID == wsUUID {
			continue
		}
		err := kws.EmitTo(wsUUID, message, mType...)
		if err != nil {
			kws.fireEvent(EventError, message, err)
		}
	}
}

// Broadcast is the package-level form of Websocket.Broadcast. It sends
// message to every active connection in the pool, including the originator
// (use the method form to skip the originator). See Websocket.Emit for the
// meaning of mType.
func Broadcast(message []byte, mType ...int) {
	for _, kws := range pool.all() {
		kws.Emit(message, mType...)
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
	t := TextMessage
	if len(mType) > 0 {
		t = mType[0]
	}
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
	var args [][]byte
	if len(data) > 0 {
		args = [][]byte{data}
	}
	kws.write(TextMessage, buildSIOEventWithAck(kws.getNamespace(), 0, false, event, args))
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
	kws.outboundAcksMu.Lock()
	// Re-check IsAlive while holding the ack mutex: disconnected() takes the
	// same mutex when it swaps the pending map. Without this re-check, a
	// disconnect that started after the IsAlive() probe but before us
	// acquiring the mutex would land our entry in the post-swap (empty) map,
	// where it could leak (timeout = 0) or fire with ErrAckTimeout instead of
	// the correct ErrAckDisconnected (timeout > 0).
	if !kws.isAlive.Load() {
		kws.outboundAcksMu.Unlock()
		cb(nil, ErrAckDisconnected)
		return
	}
	kws.outboundAckSeq++
	id := kws.outboundAckSeq
	p := &pendingAck{cb: adaptSingleArgAck(cb)}
	// Arm the timeout while still holding the lock so p.timer is published
	// to every reader (deliverOutboundAck, fireAckTimeout, disconnected
	// drain) under the same mutex that guards the map. Without this the
	// race detector flags the timer-field write against the pre-fire read
	// in the deliver path. fireAckTimeout itself acquires the lock
	// asynchronously inside the AfterFunc closure, so this cannot deadlock
	// even if the timer fires immediately on a stalled scheduler.
	if timeout > 0 {
		p.timer = time.AfterFunc(timeout, func() { kws.fireAckTimeout(id) })
	}
	if kws.outboundAcks == nil {
		kws.outboundAcks = make(map[uint64]*pendingAck)
	}
	kws.outboundAcks[id] = p
	kws.outboundAcksMu.Unlock()

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
	kws.outboundAcksMu.Lock()
	// Re-check IsAlive while holding the ack mutex: see the matching comment
	// in EmitWithAckTimeout. Closes the disconnect-vs-insert race that would
	// otherwise either leak the entry (OutboundAckTimeout = 0) or surface as
	// ErrAckTimeout instead of ErrAckDisconnected.
	if !kws.isAlive.Load() {
		kws.outboundAcksMu.Unlock()
		cb(nil, ErrAckDisconnected)
		return
	}
	kws.outboundAckSeq++
	id := kws.outboundAckSeq
	// The internal pendingAck callback already takes [][]byte, matching the
	// user signature exactly, so no lossy adapter is needed here. Wire-level
	// "43<id>[a,b]" arrives as args=[a,b]; "43<id>[[a,b]]" arrives as
	// args=[[a,b]]. The two cases are now distinguishable for callers.
	p := &pendingAck{cb: cb}
	// Arm the timeout under the same lock that guards the map (see the
	// matching note in EmitWithAckTimeout) so p.timer is safely published
	// to deliverOutboundAck / fireAckTimeout / the disconnected drain.
	if OutboundAckTimeout > 0 {
		p.timer = time.AfterFunc(OutboundAckTimeout, func() { kws.fireAckTimeout(id) })
	}
	if kws.outboundAcks == nil {
		kws.outboundAcks = make(map[uint64]*pendingAck)
	}
	kws.outboundAcks[id] = p
	kws.outboundAcksMu.Unlock()

	kws.write(TextMessage, buildSIOEventWithAck(kws.getNamespace(), id, true, event, args))
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
// It is idempotent: the synchronous DISCONNECT plus close-frame write block
// runs at most once even when called concurrently. EventClose fires exactly
// once before the regular disconnected tear-down (which fires EventDisconnect).
// Callers may invoke Close from inside an event listener; it does not block
// on the listener's own goroutine.
//
// Synchronous writes (bypassing kws.queue) are required so the frames
// reach the wire BEFORE disconnected() closes done and the send goroutine
// shuts down. The kws.mu write lock serialises with the send goroutine
// (which writes under kws.mu.RLock) so the Conn is never written
// concurrently.
//
// closeOnce gates the actual write block so concurrent Close() callers
// do not double-write the disconnect frames and, more importantly, do
// not race the upgrade handler's deferred releaseConn() that nils the
// embedded *fasthttp.Conn the moment run() returns. The companion
// kws.mu.Lock()/Unlock() fence at the tail of run() guarantees an
// in-flight Close() write has completed before the handler returns
// and releaseConn() fires.
func (kws *Websocket) Close() {
	if !kws.IsAlive() {
		return
	}

	kws.closeOnce.Do(func() {
		// Build the SIO DISCONNECT frame. Per socket.io-protocol v5,
		// namespaced packets are "41/<ns>," with a trailing comma
		// separating the namespace from the (empty) payload.
		disconnect := []byte{eioMessage, sioDisconnect}
		if ns := kws.getNamespace(); len(ns) > 0 {
			disconnect = append(disconnect, ns...)
			disconnect = append(disconnect, ',')
		}

		if kws.pollQ != nil {
			// Polling: enqueue SIO DISCONNECT and EIO CLOSE so the next
			// drain (or any in-flight long-poll) delivers them; the
			// queue is closed by disconnected() below, after which
			// further enqueues are silent no-ops. There is no
			// equivalent of the WebSocket Close control frame on
			// polling - the EIO "1" packet is the protocol-level
			// disconnect signal.
			kws.pollQ.enqueue(disconnect)
			kws.pollQ.enqueue([]byte{eioClose})
		} else {
			kws.mu.Lock()
			// Do not read kws.Conn.Conn here. The vendored websocket
			// package nils that embedded pointer in releaseConn() after the
			// upgrade handler returns, without taking our mutex. handlerDone
			// is our race-free guard for that lifecycle boundary.
			if kws.Conn != nil && !kws.handlerDone.Load() {
				_ = kws.Conn.WriteMessage(TextMessage, disconnect)
				// Per RFC 6455 a Close control frame's payload must start with
				// a 2-byte big-endian status code optionally followed by a UTF-8
				// reason. Writing the raw string would produce an invalid frame
				// that strict clients (including socket.io-client's underlying
				// engine.io transport) close with a protocol error.
				_ = kws.Conn.WriteMessage(CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Connection closed"))
			}
			kws.mu.Unlock()
		}

		kws.fireEvent(EventClose, nil, nil)
	})

	kws.disconnected(nil)
}

// getNamespace returns the Socket.IO namespace this connection is bound to,
// or nil for the root namespace.
//
// The returned slice MUST NOT be mutated by callers. namespace is written
// exactly once during handshake (under kws.mu.Lock) and never reassigned;
// readers can therefore alias the underlying bytes safely. All internal
// callers feed it into append() which never mutates the source.
func (kws *Websocket) getNamespace() []byte {
	kws.mu.RLock()
	ns := kws.namespace
	kws.mu.RUnlock()
	return ns
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
	out := make(json.RawMessage, len(kws.handshakeAuth))
	copy(out, kws.handshakeAuth)
	return out
}

// writeConnectError sends a Socket.IO CONNECT_ERROR ("44") frame directly on
// the WebSocket conn (not via the queue, since this can run before the send
// goroutine starts during the handshake).
//
// Acquires kws.mu.Lock around the WriteMessage so post-handshake callers (the
// late-CONNECT path in the read goroutine) cannot race the send goroutine,
// which writes under kws.mu.RLock. Same pattern Close() uses for its
// synchronous DISCONNECT/CLOSE frames.
//
// Returns ErrorInvalidConnection if the underlying conn is nil or the upgrade
// handler has already returned.
func (kws *Websocket) writeConnectError(namespace []byte, jsonMessage string) error {
	out := []byte{eioMessage, sioConnectError}
	if len(namespace) > 0 {
		out = append(out, namespace...)
		out = append(out, ',')
	}
	out = append(out, jsonMessage...)
	if kws.pollQ != nil {
		// Polling: enqueue the CONNECT_ERROR frame for the next drain.
		kws.pollQ.enqueue(out)
		return nil
	}
	kws.mu.Lock()
	defer kws.mu.Unlock()
	// Do not inspect kws.Conn.Conn; releaseConn() mutates that embedded
	// pointer without taking our mutex. handlerDone is the race-free
	// lifecycle guard.
	if kws.Conn == nil || kws.handlerDone.Load() {
		return ErrorInvalidConnection
	}
	return kws.Conn.WriteMessage(TextMessage, out)
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

func (kws *Websocket) hasConn() bool {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return kws.Conn.Conn != nil
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

// pong sends Engine.IO PING packets to the client at PingInterval and
// enforces the heartbeat timeout: if no frame has been received from the
// peer within PingInterval + PingTimeout the connection is dropped.
//
// The interval is read once at goroutine start (handshake-completed time);
// later mutations to the global PingInterval do not affect a live
// connection, which keeps tests race-free.
//
// To bound dead-peer detection latency, the ticker fires at
// min(PingInterval, PingTimeout) so the deadline check runs at least once
// per PingTimeout. PINGs are still emitted only every PingInterval.
// Worst-case detection latency is therefore at most
// PingInterval + PingTimeout + tick (vs. up to 2*PingInterval+PingTimeout
// when the tick equalled PingInterval).
func (kws *Websocket) pong(ctx context.Context) {
	interval := PingInterval
	if interval <= 0 {
		interval = 25 * time.Second
	}
	timeout := PingTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	deadline := interval + timeout

	tick := interval
	if timeout < tick {
		tick = timeout
	}

	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	lastPing := time.Now()
	for {
		select {
		case <-ticker.C:
			last := kws.lastPongNanos.Load()
			if last > 0 && time.Since(time.Unix(0, last)) > deadline {
				logf("warn", "heartbeat_timeout", "uuid", kws.UUID, "deadline_ms", deadline.Milliseconds())
				kws.disconnected(ErrHeartbeatTimeout)
				return
			}
			// Emit a PING only every PingInterval, regardless of tick rate.
			if time.Since(lastPing) >= interval {
				kws.write(TextMessage, eioPingFrame)
				lastPing = time.Now()
			}
		case <-ctx.Done():
			return
		}
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
		}
		return
	}
	msg := message{mType: messageType, data: messageBytes}
	select {
	case kws.queue <- msg:
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

// Send out message queue
// send drains the outbound message queue and writes frames to the wire.
//
// On a missing Conn it sleeps RetrySendTimeout (synchronously, ctx-aware)
// up to MaxSendRetry times. The previous implementation spawned a fresh
// goroutine per retry that pushed back into the buffered queue; under
// sustained network slowness that fanned out into thousands of goroutines
// and could deadlock the queue.
func (kws *Websocket) send(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-kws.queue:
			for !kws.hasConn() {
				if msg.retries >= MaxSendRetry {
					// Give up; nothing more we can do for this frame.
					msg.retries = -1
					break
				}
				msg.retries++
				select {
				case <-ctx.Done():
					return
				case <-time.After(RetrySendTimeout):
				}
			}
			if msg.retries < 0 {
				continue
			}

			// Hold the kws.mu read-lock across the write so we serialise
			// against Close()'s Lock(), which writes the SIO DISCONNECT +
			// close frame directly. Multiple send goroutines do not exist
			// (only this one), so RLock here only blocks Close().
			kws.mu.RLock()
			err := kws.Conn.WriteMessage(msg.mType, msg.data)
			kws.mu.RUnlock()
			if err != nil {
				kws.disconnected(err)
				return
			}
		}
	}
}

// run starts the heartbeat and read goroutines and blocks until the
// connection is torn down. The handshake, send goroutine and EventConnect
// notification are intentionally performed in New() before run() is called,
// so that any Emit/EmitEvent calls inside the user callback are flushed onto
// an already established connection and are not interleaved with handshake
// frames.
//
// run blocks the caller (the WebSocket upgrade handler) until ALL child
// goroutines that touch kws.Conn have exited. This is required for race
// safety: once the upgrade handler returns, the underlying gofiber/contrib
// websocket package releases the *websocket.Conn back to its pool. Any
// goroutine still running and reading kws.Conn would race the release.
func (kws *Websocket) run() {
	ctx := kws.ctx
	if ctx == nil {
		// Defensive: should always be set by New() but allow standalone use.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		kws.ctx = ctx
		kws.cancelCtx = cancel
		kws.workersWg.Add(1)
		go func() { defer kws.workersWg.Done(); kws.send(ctx) }()
	}

	kws.workersWg.Add(2)
	go func() { defer kws.workersWg.Done(); kws.pong(ctx) }()
	go func() { defer kws.workersWg.Done(); kws.read(ctx) }()

	<-kws.done // block until disconnected closes the channel

	kws.finishRun()
}

func (kws *Websocket) finishRun() {
	if kws.cancelCtx != nil {
		kws.cancelCtx()
	}

	// Wait for send / pong / read to actually exit before letting the
	// upgrade handler return and the websocket framework release Conn.
	kws.workersWg.Wait()

	// Mark the upgrade handler as finished before the final barrier. A Close
	// caller that starts after this point will skip direct Conn writes; a
	// Close caller already inside its write block still holds kws.mu and is
	// waited on by the barrier below.
	kws.handlerDone.Store(true)

	// Fence against any in-flight Close() that is still inside its
	// kws.mu-protected write block. Close() is invoked from caller
	// goroutines that are NOT tracked by workersWg, so workersWg.Wait()
	// alone cannot guarantee they have finished writing to kws.Conn.
	// Acquiring and immediately releasing kws.mu here blocks until
	// every concurrent Close() writer has exited the critical section,
	// ensuring no goroutine is still touching the embedded *fasthttp.Conn
	// when this function returns and the vendored websocket package's
	// deferred releaseConn() nils that pointer.
	kws.mu.Lock()
	//nolint:staticcheck // intentional empty-locked region; this is a barrier.
	kws.mu.Unlock()
}

// Listen for incoming messages
// and filter by message type
func (kws *Websocket) read(ctx context.Context) {
	// Single-reader contract: only this goroutine ever calls ReadMessage on
	// kws.Conn. We do NOT hold kws.mu around it because ReadMessage blocks
	// indefinitely until a peer frame arrives, which would deadlock any
	// goroutine that subsequently takes kws.mu.Lock (e.g. Close(),
	// disconnected()). SetReadDeadline (called by disconnected to break
	// us out of ReadMessage) is delegated to net.Conn.SetReadDeadline,
	// which is safe to call concurrently with an in-flight Read per
	// the net.Conn contract.
	for {
		if !kws.hasConn() {
			return
		}

		mType, msg, err := kws.Conn.ReadMessage()

		// Any successful application-layer frame counts as proof of life
		// for the heartbeat enforcer. RFC 6455 control frames (Ping/Pong)
		// are answered by the underlying websocket library at the OS/TCP
		// boundary even when the EIO peer is stuck, so excluding them
		// prevents a wedged client from masking its own death.
		if err == nil && mType != PingMessage && mType != PongMessage {
			kws.lastPongNanos.Store(time.Now().UnixNano())
		}

		// Cancellation while reading.
		if ctx.Err() != nil {
			return
		}

		// WebSocket-level control frames.
		if mType == PingMessage {
			kws.fireEvent(EventPing, nil, nil)
			continue
		}
		if mType == PongMessage {
			kws.fireEvent(EventPong, nil, nil)
			continue
		}
		if mType == CloseMessage {
			kws.disconnected(nil)
			return
		}

		if err != nil {
			kws.disconnected(err)
			return
		}

		// Binary messages (socket.io binary events / raw binary data).
		// Copy the bytes off the read buffer; listeners may spawn goroutines
		// that observe payload.Data after the next ReadMessage() reuses msg.
		if mType == BinaryMessage {
			data := make([]byte, len(msg))
			copy(data, msg)
			kws.fireEvent(EventMessage, data, nil)
			continue
		}

		// Text messages: parse Engine.IO packet.
		if mType != TextMessage || len(msg) == 0 {
			continue
		}

		// EIO v4 supports batching multiple packets in one WebSocket
		// frame, separated by ASCII RS (0x1E). Single-packet frames
		// (no separator) take the zero-alloc fast path; the rare batched
		// form is parsed with a hand-rolled scanner that walks msg with
		// bytes.IndexByte so we never materialise a [][]byte for a
		// frame that an attacker could fill with separators (which
		// bytes.Split would amplify into millions of slice headers).
		if bytes.IndexByte(msg, eioPacketSeparator) < 0 {
			kws.dispatchEIOPacket(msg)
		} else {
			rest, count := msg, 0
			for len(rest) > 0 {
				if count > MaxBatchPackets {
					logf("warn", "batched_frame_overflow", "uuid", kws.UUID, "limit", MaxBatchPackets)
					kws.fireEvent(EventError, nil, ErrBatchPacketsExceeded)
					break
				}
				idx := bytes.IndexByte(rest, eioPacketSeparator)
				var packet []byte
				if idx < 0 {
					packet, rest = rest, nil
				} else {
					packet, rest = rest[:idx], rest[idx+1:]
				}
				if len(packet) == 0 {
					continue
				}
				kws.dispatchEIOPacket(packet)
				count++
			}
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
	data := payload[1:]

	// Capture optional namespace prefix (e.g., "/admin,") BEFORE stripping
	// so we can verify it matches the namespace bound to this connection.
	// ACK ids are per-namespace per the socket.io v5 spec, so a frame whose
	// namespace does not match the connection's bound namespace must NOT be
	// allowed to fire a pending callback (or event listener) registered on a
	// different namespace.
	var packetNS []byte
	if len(data) > 0 && data[0] == '/' {
		if idx := bytes.IndexByte(data, ','); idx >= 0 {
			packetNS = data[:idx]
			data = data[idx+1:]
		} else {
			packetNS = data
			data = nil
		}
	}

	switch sioType {
	case sioEvent:
		// Cross-namespace guard: reject events whose namespace prefix does
		// not match the namespace bound to this connection. Otherwise a
		// frame "42/admin,..." arriving on a "/" connection would fire
		// listeners registered on the root namespace.
		if !bytes.Equal(packetNS, kws.getNamespace()) {
			kws.fireEvent(EventError, payload, fmt.Errorf("socketio: cross-namespace event dropped: packet=%q conn=%q", packetNS, kws.getNamespace()))
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
		// Per socket.io-protocol v5, "41/<ns>," targets a single namespace and
		// must NOT tear down sibling namespaces sharing the same EIO
		// connection. This implementation is single-namespace-per-conn
		// (kws.namespace is one slice), so we cannot detach a namespace
		// without ending the conn. Compromise (iter 9): if the inbound
		// namespace matches the bound one, treat as a real disconnect; if it
		// does not match, ignore the frame so a malicious or buggy client
		// cannot kill the conn by addressing a foreign namespace. A full
		// per-conn namespaces map is deferred to a later iteration.
		ns := extractSIONamespace(payload[1:])
		if !bytes.Equal(ns, kws.getNamespace()) {
			return
		}
		kws.disconnected(nil)

	case sioConnect:
		// CONNECT path. For polling sessions the SIO CONNECT packet
		// arrives via the first POST after the OPEN handshake response;
		// the WebSocket path performs CONNECT synchronously inside
		// handshake() and never reaches this case for the initial
		// CONNECT. The polling first-CONNECT branch below mirrors the
		// validation, namespace/auth capture, and EventConnect dispatch
		// that handshake() does for WebSocket sessions.
		ns, auth := extractSIOConnect(payload[1:])
		if !isValidNamespace(ns) {
			_ = kws.writeConnectError(ns, `{"message":"Invalid namespace"}`)
			if kws.pollQ != nil {
				kws.disconnected(ErrInvalidNamespace)
			}
			return
		}
		if kws.pollQ != nil && !kws.connectFired.Load() {
			// Polling first-CONNECT: validate auth, persist namespace +
			// auth, send the CONNECT ack, then fire EventConnect once.
			if !isValidAuthPayload(auth) {
				logf("warn", "invalid_auth_payload", "uuid", kws.UUID, "namespace", string(ns), "size", len(auth))
				_ = kws.writeConnectError(ns, `{"message":"Invalid auth payload"}`)
				kws.disconnected(ErrInvalidAuthPayload)
				return
			}
			nsCopy := append([]byte(nil), ns...)
			kws.mu.Lock()
			kws.namespace = nsCopy
			if len(auth) > 0 {
				kws.handshakeAuth = make(json.RawMessage, len(auth))
				copy(kws.handshakeAuth, auth)
			}
			kws.mu.Unlock()
			ackPayload, err := json.Marshal(struct {
				SID string `json:"sid"`
			}{SID: kws.UUID})
			if err == nil {
				kws.write(TextMessage, buildSIOConnectAck(nsCopy, ackPayload))
			}
			if kws.connectFired.CompareAndSwap(false, true) {
				kws.fireEvent(EventConnect, nil, nil)
			}
			return
		}
		// Late namespace CONNECT (after the initial handshake): confirm
		// via the send queue, mirroring the namespace, so we do not race
		// the read loop.
		ackPayload, err := json.Marshal(struct {
			SID string `json:"sid"`
		}{SID: kws.GetUUID()})
		if err == nil {
			kws.write(TextMessage, buildSIOConnectAck(ns, ackPayload))
		}

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
		var arr []json.RawMessage
		if err := json.Unmarshal(rest, &arr); err != nil {
			return
		}
		var args [][]byte
		if len(arr) > 0 {
			args = make([][]byte, len(arr))
			for i, raw := range arr {
				// Copy each raw JSON value off the read buffer so callers may
				// retain it past the next ReadMessage (the underlying
				// websocket library reuses the read buffer between frames).
				args[i] = append([]byte(nil), raw...)
			}
		}
		kws.deliverOutboundAck(ackID, args)

	default:
		// Unknown SIO packet type: surface payload as a raw message.
		kws.fireEvent(EventMessage, payload, nil)
	}
}

// disconnected is the single tear-down entry point.
//
// It is idempotent: only the first invocation fires EventDisconnect /
// EventError, removes the connection from the pool, drains pending ack
// callbacks and closes the done channel. Subsequent calls are no-ops.
// This guarantees "EventDisconnect fires exactly once" even when read,
// send, pong and handshake all hit an error simultaneously.
func (kws *Websocket) disconnected(err error) {
	first := false
	kws.once.Do(func() {
		first = true
		kws.setAlive(false)
		// Push an immediate read deadline so any in-flight ReadMessage in
		// the read goroutine returns and lets it exit. SetReadDeadline
		// is delegated to net.Conn, whose contract guarantees concurrent
		// callers are safe (it is the standard idiom for cancelling a
		// blocking Read), so no kws.mu lock is required and we cannot
		// deadlock against the read goroutine's blocking ReadMessage.
		if c := kws.Conn; c != nil {
			_ = c.SetReadDeadline(time.Unix(0, 1))
		}
		// Polling: release any blocked long-poll drain. Frames already
		// in the buffer are still drainable; subsequent enqueues become
		// no-ops.
		if kws.pollQ != nil {
			kws.pollQ.close()
		}
		// Stop the polling handshake timer if scheduled. Without this,
		// the AfterFunc closure keeps the *Websocket alive on the
		// runtime timer heap until HandshakeTimeout elapses (10s
		// default), bloating live-set under churn.
		if t := kws.handshakeTimer.Load(); t != nil {
			t.Stop()
		}
	})
	if !first {
		return
	}

	// Remove from the pool BEFORE firing user events so that listeners
	// observing pool.all() do not see this dying connection.
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

	// Close done last so run() unblocks AFTER user-visible events have fired.
	close(kws.done)
}

// Create random UUID for each connection
func (kws *Websocket) createUUID() string {
	return kws.randomUUID()
}

// Generate random UUID.
func (kws *Websocket) randomUUID() string {
	return uuid.New().String()
}

// Fires event on all connections.
func fireGlobalEvent(event string, data []byte, error error) {
	for _, kws := range pool.all() {
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
// with concurrent SetAttribute mutations.
func (kws *Websocket) fireEventWithAck(event string, args [][]byte, fireErr error, ackID uint64, hasAck bool) {
	callbacks := listeners.get(event)
	if len(callbacks) == 0 {
		return
	}

	kws.mu.RLock()
	uuid := kws.UUID
	attrs := make(map[string]any, len(kws.attributes))
	for k, v := range kws.attributes {
		attrs[k] = v
	}
	var auth json.RawMessage
	if event == EventConnect && len(kws.handshakeAuth) > 0 {
		auth = make(json.RawMessage, len(kws.handshakeAuth))
		copy(auth, kws.handshakeAuth)
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
		// up to logging.
		func(cb eventCallback) {
			defer func() {
				if r := recover(); r != nil {
					logf("error", "listener_panic", "uuid", uuid, "event", event, "panic", fmt.Sprintf("%v", r))
					kws.fireEvent(EventError, nil, fmt.Errorf("socketio: listener panic on %q: %v", event, r))
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
// for all of their per-connection goroutines (send, read, pong) to exit,
// or until ctx is cancelled.
//
// Wire this into fiber.App.Shutdown / fiber.App.ShutdownWithContext so an
// application shutdown deterministically tears down sockets instead of
// relying on the framework to close the underlying transport.
//
// Returns ctx.Err() when ctx is cancelled before all connections finished
// draining; otherwise returns nil.
func Shutdown(ctx context.Context) error {
	conns := pool.all()
	if len(conns) == 0 {
		return nil
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, c := range conns {
		kws, ok := c.(*Websocket)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(k *Websocket) {
			defer wg.Done()
			k.Close()
			k.workersWg.Wait()
		}(kws)
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
