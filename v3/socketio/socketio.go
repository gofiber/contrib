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
	// EventMessage Fired when a Text/Binary message is received
	EventMessage = "message"
	// EventPing More details here:
	// @url https://developer.mozilla.org/en-US/docs/Web/API/WebSockets_API/Writing_WebSocket_servers#Pings_and_Pongs_The_Heartbeat_of_WebSockets
	EventPing = "ping"
	// EventPong is fired when a WebSocket pong control frame, or an
	// Engine.IO PONG packet, is received from the peer.
	EventPong = "pong"
	// EventDisconnect Fired on disconnection
	// The error provided in disconnection event
	// is defined in RFC 6455, section 11.7.
	// @url https://github.com/gofiber/websocket/blob/cd4720c435de415b864d975a9ca23a47eaf081ef/websocket.go#L192
	EventDisconnect = "disconnect"
	// EventConnect Fired on first connection
	EventConnect = "connect"
	// EventClose Fired when the connection is actively closed from the server
	EventClose = "close"
	// EventError Fired when some error appears useful also for debugging websockets
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
)

// reservedEventNames is the set of outbound event names the JS socket.io
// client treats as built-in lifecycle events. Emitting any of these by
// name from the server would either be silently swallowed or trigger
// spurious lifecycle handlers on the client.
var reservedEventNames = map[string]struct{}{
	"connect":        {},
	"connect_error":  {},
	"disconnect":     {},
	"disconnecting":  {},
	"newListener":    {},
	"removeListener": {},
}

// isReservedEventName reports whether name is a reserved socket.io
// lifecycle event name that must not be used as a custom event name.
func isReservedEventName(name string) bool {
	_, ok := reservedEventNames[name]
	return ok
}

var (
	// PongTimeout is the legacy heartbeat timeout knob.
	//
	// Deprecated: PongTimeout is no longer consulted by the heartbeat
	// implementation. Use PingTimeout instead.
	PongTimeout = 20 * time.Second
	// RetrySendTimeout is the back-off delay the send goroutine waits before
	// retrying a failed write to a temporarily unavailable connection.
	RetrySendTimeout = 20 * time.Millisecond
	// MaxSendRetry is the maximum number of times the send goroutine retries a
	// frame against a missing connection before dropping it.
	MaxSendRetry = 5
	// ReadTimeout is the small pause inserted between read iterations to keep
	// the read loop from saturating a CPU core under tight polling.
	ReadTimeout = 10 * time.Millisecond
	// PingInterval is how often the server sends Engine.IO PING packets to the client.
	// The client must reply with a PONG packet to keep the connection alive.
	PingInterval = 25 * time.Second
	// PingTimeout is advertised to the client in the EIO OPEN packet.
	// The client closes the connection if it does not receive a PING from the
	// server within PingInterval + PingTimeout.
	PingTimeout = 20 * time.Second
	// HandshakeTimeout caps how long the server waits for the client SIO CONNECT
	// packet ("40") after sending the EIO OPEN packet.
	HandshakeTimeout = 10 * time.Second
	// MaxPayload is the maximum size in bytes of an inbound WebSocket frame.
	// It is advertised to the client in the EIO OPEN packet AND enforced via
	// SetReadLimit on the underlying connection. Frames exceeding this size
	// are rejected and the connection is closed.
	MaxPayload int64 = 1_000_000
	// OutboundAckTimeout is the default deadline for ack callbacks
	// registered via EmitWithAck. Override with EmitWithAckTimeout for
	// per-call timeouts.
	OutboundAckTimeout = 30 * time.Second
	// MaxBatchPackets caps the number of EIO packets accepted in a single
	// 0x1E-batched WebSocket frame. Without this cap, a frame consisting
	// almost entirely of record separators forces a multi-megabyte slice
	// header allocation. 256 is comfortably above any legitimate batch.
	MaxBatchPackets = 256
	// MaxEventNameLength bounds inbound SIO event name strings so a hostile
	// client cannot pin a multi-megabyte string per frame as part of the
	// EventPayload it dispatches to user listeners.
	MaxEventNameLength = 256
)

// AckCallback receives the result of a server-initiated EmitWithAck.
//
// On a successful ack, ack holds the raw JSON of the client's first ack
// argument (or a JSON-array of all args when the client called the ack
// with multiple values) and err is nil.
//
// On timeout err is ErrAckTimeout. On connection close err is
// ErrAckDisconnected. In both error cases ack is nil.
type AckCallback func(ack []byte, err error)

// pendingAck tracks one outstanding outbound ack: the callback to invoke
// and an optional timer that fires ErrAckTimeout if the client never
// responds.
type pendingAck struct {
	cb    AckCallback
	timer *time.Timer
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

// EventPayload is the object passed to every event listener. It carries the
// originating connection, the event arguments in Args (with Data kept as a
// shortcut to the first arg), the ack bookkeeping fields AckID, HasAck and
// Ack, and HandshakeAuth (the raw JSON auth payload supplied during the
// initial SIO CONNECT, populated for EventConnect listeners).
type EventPayload struct {
	// The connection object
	Kws *Websocket
	// The name of the event
	Name string
	// Unique connection UUID
	SocketUUID string
	// Optional websocket attributes
	SocketAttributes map[string]any
	// Optional error when are fired events like
	// - Disconnect
	// - Error
	Error error
	// Data is the first event argument (if any). Kept for backwards
	// compatibility; equivalent to Args[0] when len(Args) > 0, else nil.
	Data []byte
	// Args are the raw-JSON arguments the client sent with the event.
	// Each entry is one JSON value; nil for events without args. Use this
	// to consume socket.emit("event", a, b, c) from the JS client side.
	Args [][]byte
	// AckID is the Socket.IO ack id attached to this event by the client.
	// It is meaningful only when HasAck is true. Use Ack() to respond.
	AckID uint64
	// HasAck reports whether the client requested an ack for this event
	// (i.e. the JS side called socket.emit("event", data, callback)).
	HasAck bool
	// HandshakeAuth is the raw JSON auth payload the client supplied in its
	// SIO CONNECT packet, copied for safety. nil if the client connected
	// without an auth payload. Populated for EventConnect; for other
	// events use Kws.HandshakeAuth() if needed.
	HandshakeAuth json.RawMessage
	// ackSent is the CAS guard that makes Ack() idempotent. It is shared
	// across all EventPayload instances dispatched for the same inbound
	// SIO event (one per listener), so that two listeners both calling
	// Ack on their own payload still produce only one wire frame.
	ackSent *atomic.Bool
}

// Ack sends a Socket.IO ACK ("43") response back to the client for the event
// represented by this payload. The variadic args ...[]byte signature accepts
// zero arguments (empty ack), one argument (single value), or many arguments
// that are emitted as comma-separated raw-JSON values, mirroring the JS-side
// callback(a, b, c) shape. Each non-empty argument must be valid JSON; nil
// or empty entries are skipped.
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

// buildSIOEvent encodes a Socket.IO EVENT packet ready to send over the wire.
// Format: 4 2 [/<namespace>,] [ "<event>" , <data> ]
//
// data must be valid JSON (object, array, string, number, etc.) or nil.
// namespace may be nil for the root namespace.
func buildSIOEvent(namespace []byte, event string, data []byte) []byte {
	if len(data) == 0 {
		return buildSIOEventWithAck(namespace, 0, false, event, nil)
	}
	return buildSIOEventWithAck(namespace, 0, false, event, [][]byte{data})
}

// buildSIOEventWithAck is the ack-id aware multi-arg variant of buildSIOEvent.
//
// args is the slice of raw-JSON arguments to encode after the event name
// (matches the JS-side socket.emit("event", a, b, c) shape). Each entry
// must be valid JSON; nil/empty entries are skipped.
//
// The output buffer is pre-sized so a typical event allocates exactly
// once instead of growing through 8/16/32/... append boundaries.
func buildSIOEventWithAck(namespace []byte, ackID uint64, hasAck bool, event string, args [][]byte) []byte {
	name, _ := json.Marshal(event)
	size := 2 + len(namespace) + 1 + 20 + 2 + len(name)
	for _, a := range args {
		if len(a) > 0 {
			size += 1 + len(a)
		}
	}
	buf := make([]byte, 0, size)
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
		buf = append(buf, a...)
	}
	buf = append(buf, ']')
	return buf
}

// buildSIOAck encodes a Socket.IO ACK ("43") packet.
//
// Format: 4 3 [/<namespace>,] <ackID> [ <args> ]
// args may be nil/empty to send `43<id>[]`. Each non-empty arg is one
// raw-JSON value, comma-separated.
func buildSIOAck(namespace []byte, ackID uint64, args [][]byte) []byte {
	size := 2 + len(namespace) + 1 + 20 + 2
	for _, a := range args {
		if len(a) > 0 {
			size += 1 + len(a)
		}
	}
	buf := make([]byte, 0, size)
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
		buf = append(buf, a...)
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
			return 0, false, data, errors.New("socketio: ack id overflow")
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
		return "", nil, errors.New("socketio: empty event array")
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
	mu   sync.RWMutex
	// The Fiber.Websocket connection
	Conn *websocket.Conn
	// isAlive reports whether the connection is alive. Accessed lock-free
	// from every emit path (write/EmitTo/etc.) and the read goroutine.
	isAlive atomic.Bool
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

func (p *safePool) contains(key string) bool {
	p.RLock()
	_, ok := p.conn[key]
	p.RUnlock()
	return ok
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

// New returns a Fiber handler that upgrades the request to a Socket.IO-
// compatible WebSocket, performs the Engine.IO / Socket.IO handshake, and
// invokes callback with the established Websocket so user code can register
// per-connection state before the read and heartbeat goroutines start.
func New(callback func(kws *Websocket), config ...websocket.Config) func(fiber.Ctx) error {
	return websocket.New(func(c *websocket.Conn) {
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
			queue:        make(chan message, 100),
			done:         make(chan struct{}, 1),
			attributes:   make(map[string]interface{}),
			outboundAcks: make(map[uint64]*pendingAck),
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
		callback(kws)

		// 4. Notify listeners that the socket is ready.
		kws.fireEvent(EventConnect, nil, nil)

		// 5. Read / heartbeat goroutines and block until the connection closes.
		kws.run()
	}, config...)
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
	maxPayload := int(MaxPayload)
	if maxPayload <= 0 {
		maxPayload = 1_000_000
	}
	open := eioOpenPacket{
		SID:          kws.UUID,
		Upgrades:     []string{},
		PingInterval: int(PingInterval.Milliseconds()),
		PingTimeout:  int(PingTimeout.Milliseconds()),
		MaxPayload:   maxPayload,
	}
	data, err := json.Marshal(open)
	if err != nil {
		return fmt.Errorf("socketio: marshal EIO OPEN: %w", err)
	}
	if err := kws.Conn.WriteMessage(TextMessage, append([]byte{eioOpen}, data...)); err != nil {
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
		return errors.New("socketio: connection closed during handshake")
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
		_ = kws.writeConnectError(namespace, `{"message":"invalid namespace"}`)
		return errors.New("socketio: invalid namespace in SIO CONNECT")
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

// SetAttribute Set a specific attribute for the specific socket connection
func (kws *Websocket) SetAttribute(key string, attribute interface{}) {
	kws.mu.Lock()
	defer kws.mu.Unlock()
	kws.attributes[key] = attribute
}

// GetAttribute Get a specific attribute from the socket attributes
func (kws *Websocket) GetAttribute(key string) interface{} {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	value, ok := kws.attributes[key]
	if ok {
		return value
	}
	return nil
}

// GetIntAttribute Convenience method to retrieve an attribute as an int.
// Will panic if attribute is not an int.
func (kws *Websocket) GetIntAttribute(key string) int {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	value, ok := kws.attributes[key]
	if ok {
		return value.(int)
	}
	return 0
}

// GetStringAttribute Convenience method to retrieve an attribute as a string.
func (kws *Websocket) GetStringAttribute(key string) string {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	value, ok := kws.attributes[key]
	if ok {
		return value.(string)
	}
	return ""
}

// EmitToList Emit the message to a specific socket uuids list
func (kws *Websocket) EmitToList(uuids []string, message []byte, mType ...int) {
	for _, wsUUID := range uuids {
		err := kws.EmitTo(wsUUID, message, mType...)
		if err != nil {
			kws.fireEvent(EventError, message, err)
		}
	}
}

// EmitToList Emit the message to a specific socket uuids list
// Ignores all errors
func EmitToList(uuids []string, message []byte, mType ...int) {
	for _, wsUUID := range uuids {
		_ = EmitTo(wsUUID, message, mType...)
	}
}

// EmitTo Emit to a specific socket connection
func (kws *Websocket) EmitTo(uuid string, message []byte, mType ...int) error {

	conn, err := pool.get(uuid)
	if err != nil {
		return err
	}
	if !pool.contains(uuid) || !conn.IsAlive() {
		kws.fireEvent(EventError, []byte(uuid), ErrorInvalidConnection)
		return ErrorInvalidConnection
	}

	conn.Emit(message, mType...)
	return nil
}

// EmitTo Emit to a specific socket connection
func EmitTo(uuid string, message []byte, mType ...int) error {
	conn, err := pool.get(uuid)
	if err != nil {
		return err
	}

	if !pool.contains(uuid) || !conn.IsAlive() {
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

// Broadcast sends message to every active connection in the pool. The
// optional mType selects the WebSocket frame type: omit it (or pass
// TextMessage) to wrap message as a Socket.IO "message" event; pass
// BinaryMessage to send the bytes verbatim as a binary frame.
func Broadcast(message []byte, mType ...int) {
	for _, kws := range pool.all() {
		kws.Emit(message, mType...)
	}
}

// Fire custom event
func (kws *Websocket) Fire(event string, data []byte) {
	kws.fireEvent(event, data, nil)
}

// Fire custom event on all connections
func Fire(event string, data []byte) {
	fireGlobalEvent(event, data, nil)
}

// Emit sends a message to the client as a socket.io "message" event.
// The message parameter should be valid JSON (object, array, string literal, etc.).
// For named events use EmitEvent instead. The connection's namespace
// (captured during the handshake) is mirrored on the wire.
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
// should be valid JSON. The connection's namespace (captured during the
// handshake) is mirrored on the wire.
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

// EmitArgs sends a named socket.io event with multiple raw-JSON arguments,
// matching the JS-side call socket.emit("event", a, b, c). Each arg must
// be valid JSON. Empty entries are skipped. The connection's namespace
// (captured during the handshake) is mirrored on the wire.
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
// The data parameter must be valid JSON or nil. The callback receives the
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
	if !kws.IsAlive() {
		cb(nil, ErrAckDisconnected)
		return
	}

	kws.outboundAcksMu.Lock()
	kws.outboundAckSeq++
	id := kws.outboundAckSeq
	p := &pendingAck{cb: cb}
	kws.outboundAcks[id] = p
	kws.outboundAcksMu.Unlock()

	if timeout > 0 {
		p.timer = time.AfterFunc(timeout, func() { kws.fireAckTimeout(id) })
	}

	var args [][]byte
	if len(data) > 0 {
		args = [][]byte{data}
	}
	kws.write(TextMessage, buildSIOEventWithAck(kws.getNamespace(), id, true, event, args))
}

// EmitWithAckArgs is the multi-arg + structured-error variant of
// EmitWithAck. It sends a named event carrying multiple raw-JSON arguments
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
	if !kws.IsAlive() {
		cb(nil, ErrAckDisconnected)
		return
	}

	kws.outboundAcksMu.Lock()
	kws.outboundAckSeq++
	id := kws.outboundAckSeq
	// Adapt the user's [][]byte callback to the internal AckCallback shape.
	// deliverOutboundAck encodes single-arg as the raw JSON value, multi-arg
	// as a JSON array literal; this adapter restores the structured slice.
	adapter := func(ack []byte, err error) {
		if err != nil {
			cb(nil, err)
			return
		}
		if len(ack) == 0 {
			cb(nil, nil)
			return
		}
		if ack[0] == '[' {
			var arr []json.RawMessage
			if json.Unmarshal(ack, &arr) == nil {
				out := make([][]byte, 0, len(arr))
				for _, r := range arr {
					out = append(out, []byte(r))
				}
				cb(out, nil)
				return
			}
		}
		cb([][]byte{ack}, nil)
	}
	p := &pendingAck{cb: adapter}
	kws.outboundAcks[id] = p
	kws.outboundAcksMu.Unlock()

	if OutboundAckTimeout > 0 {
		p.timer = time.AfterFunc(OutboundAckTimeout, func() { kws.fireAckTimeout(id) })
	}

	kws.write(TextMessage, buildSIOEventWithAck(kws.getNamespace(), id, true, event, args))
}

// deliverOutboundAck dispatches an incoming ACK to the registered callback,
// if any, and removes it from the pending map. The map-delete is the
// single source of truth: timer-fire and ack-arrival race through the
// same mutex, so the callback is invoked exactly once.
func (kws *Websocket) deliverOutboundAck(id uint64, data []byte) {
	kws.outboundAcksMu.Lock()
	p, ok := kws.outboundAcks[id]
	if ok {
		delete(kws.outboundAcks, id)
	}
	kws.outboundAcksMu.Unlock()
	if !ok || p == nil {
		return
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	if p.cb != nil {
		func() {
			defer func() { _ = recover() }()
			p.cb(data, nil)
		}()
	}
}

// fireAckTimeout is called from time.AfterFunc when the configured ack
// deadline elapses. The map-delete-wins pattern ensures a racing ack
// arrival cannot also fire the callback.
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
	defer func() { _ = recover() }()
	p.cb(nil, ErrAckTimeout)
}

// Close Actively close the connection from the server.
//
// Idempotent. Synchronously sends a Socket.IO DISCONNECT packet (mirroring
// the namespace) followed by a WebSocket close frame, fires EventClose
// exactly once, then runs the normal disconnected tear-down.
//
// Synchronous writes (bypassing kws.queue) are required so the frames
// reach the wire BEFORE disconnected() closes done and the send goroutine
// shuts down. The kws.mu write lock serialises with the send goroutine
// (which writes under kws.mu.RLock) so the Conn is never written
// concurrently.
func (kws *Websocket) Close() {
	if !kws.IsAlive() {
		return
	}

	// Build the SIO DISCONNECT frame. Per socket.io-protocol v5, namespaced
	// packets are "41/<ns>," with a trailing comma separating the namespace
	// from the (empty) payload.
	disconnect := []byte{eioMessage, sioDisconnect}
	if ns := kws.getNamespace(); len(ns) > 0 {
		disconnect = append(disconnect, ns...)
		disconnect = append(disconnect, ',')
	}

	kws.mu.Lock()
	if kws.Conn != nil {
		_ = kws.Conn.WriteMessage(TextMessage, disconnect)
		_ = kws.Conn.WriteMessage(CloseMessage, []byte("Connection closed"))
	}
	kws.mu.Unlock()

	kws.fireEvent(EventClose, nil, nil)
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
// Returns ErrorInvalidConnection if the underlying conn is nil. Callers in the
// post-handshake read path may invoke us during teardown after the conn was
// released; without the guard the dereference would panic in the read goroutine.
func (kws *Websocket) writeConnectError(namespace []byte, jsonMessage string) error {
	if kws.Conn == nil {
		return ErrorInvalidConnection
	}
	out := []byte{eioMessage, sioConnectError}
	if len(namespace) > 0 {
		out = append(out, namespace...)
		out = append(out, ',')
	}
	out = append(out, jsonMessage...)
	return kws.Conn.WriteMessage(TextMessage, out)
}

// IsAlive reports whether the connection is still considered active and able
// to deliver outbound frames. Lock-free.
func (kws *Websocket) IsAlive() bool {
	return kws.isAlive.Load()
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

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			last := kws.lastPongNanos.Load()
			if last > 0 && time.Since(time.Unix(0, last)) > deadline {
				kws.disconnected(errors.New("socketio: heartbeat timeout"))
				return
			}
			kws.write(TextMessage, eioPingFrame)
		case <-ctx.Done():
			return
		}
	}
}

// write enqueues a message for the send goroutine.
//
// The queue is buffered (cap 100). When the queue is full, write returns
// without blocking and surfaces an error event so the caller is not
// deadlocked when the send goroutine has died (e.g. after disconnected
// fired). Calls on already-disconnected sockets are a no-op.
func (kws *Websocket) write(messageType int, messageBytes []byte) {
	if !kws.IsAlive() {
		return
	}
	msg := message{mType: messageType, data: messageBytes}
	select {
	case kws.queue <- msg:
	default:
		// Queue is full and send is not draining; tear down rather than
		// pin the calling goroutine.
		kws.disconnected(errors.New("socketio: send queue overflow"))
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

	if kws.cancelCtx != nil {
		kws.cancelCtx()
	}

	// Wait for send / pong / read to actually exit before letting the
	// upgrade handler return and the websocket framework release Conn.
	kws.workersWg.Wait()
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
		if bytes.IndexByte(msg, 0x1E) < 0 {
			kws.dispatchEIOPacket(msg)
		} else {
			rest, count := msg, 0
			for len(rest) > 0 {
				if count > MaxBatchPackets {
					kws.fireEvent(EventError, nil, errors.New("socketio: batched frame exceeds MaxBatchPackets"))
					break
				}
				idx := bytes.IndexByte(rest, 0x1E)
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
		kws.fireEvent(EventError, msg, errors.New("socketio: unknown EIO packet type"))
	}
}

// handleSIOPacket processes a Socket.IO packet (the bytes after the "4" EIO prefix).
func (kws *Websocket) handleSIOPacket(payload []byte) {
	if len(payload) == 0 {
		return
	}

	sioType := payload[0]
	data := payload[1:]

	// Strip optional namespace prefix (e.g., "/admin,")
	if len(data) > 0 && data[0] == '/' {
		if idx := bytes.IndexByte(data, ','); idx >= 0 {
			data = data[idx+1:]
		} else {
			data = nil
		}
	}

	switch sioType {
	case sioEvent:
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
		kws.disconnected(nil)

	case sioConnect:
		// Late namespace CONNECT (after the initial handshake): confirm via
		// the send queue, mirroring the namespace, so we do not race the read
		// loop. The namespace is validated against the same grammar as the
		// initial handshake so a malicious client cannot smuggle CRLF or
		// commas back into the ACK frame.
		ns := extractSIONamespace(payload[1:])
		if !isValidNamespace(ns) {
			_ = kws.writeConnectError(ns, `{"message":"Invalid namespace"}`)
			return
		}
		ackPayload, err := json.Marshal(struct {
			SID string `json:"sid"`
		}{SID: kws.GetUUID()})
		if err == nil {
			kws.write(TextMessage, buildSIOConnectAck(ns, ackPayload))
		}

	case sioAck:
		// 43[/ns,]<id>[<data>] - response to a server-initiated EmitWithAck.
		ackID, has, rest, err := splitSIOAckID(data)
		if err != nil || !has {
			return
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(rest, &arr); err != nil {
			return
		}
		var ackData []byte
		switch {
		case len(arr) == 0:
			ackData = nil
		case len(arr) == 1:
			ackData = []byte(arr[0])
		default:
			ackData, _ = json.Marshal(arr)
		}
		kws.deliverOutboundAck(ackID, ackData)

	default:
		// Unknown SIO packet type — surface payload as a raw message
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
	// err ErrAckTimeout).
	kws.outboundAcksMu.Lock()
	pending := kws.outboundAcks
	kws.outboundAcks = make(map[uint64]*pendingAck)
	kws.outboundAcksMu.Unlock()
	for _, p := range pending {
		if p == nil {
			continue
		}
		if p.timer != nil {
			p.timer.Stop()
		}
		if p.cb != nil {
			func(cb AckCallback) {
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

// On registers a callback to be invoked whenever the named event fires on
// any connection.
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
