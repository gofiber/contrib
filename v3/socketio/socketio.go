package socketio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
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
	// ErrorInvalidConnection The addressed Conn connection is not available anymore
	// error data is the uuid of that connection
	ErrorInvalidConnection = errors.New("message cannot be delivered invalid/gone connection")
	// ErrorUUIDDuplication The UUID already exists in the pool
	ErrorUUIDDuplication = errors.New("UUID already exists in the available connections pool")
)

var (
	// PongTimeout is deprecated. Use PingTimeout instead.
	PongTimeout = 20 * time.Second
	// RetrySendTimeout retry after 20 ms if there is an error
	RetrySendTimeout = 20 * time.Millisecond
	// MaxSendRetry define max retries if there are socket issues
	MaxSendRetry = 5
	// ReadTimeout Instead of reading in a for loop, try to avoid full CPU load taking some pause
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
)

// Raw form of websocket message
type message struct {
	// Message type
	mType int
	// Message data
	data []byte
	// Message send retries when error
	retries int
}

// EventPayload Event Payload is the object that
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
	SocketAttributes map[string]any
	// Optional error when are fired events like
	// - Disconnect
	// - Error
	Error error
	// Data is used on Message and on Error event
	Data []byte
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
// The data argument must be valid JSON (object, array, string, number, etc.).
// namespace may be nil for the root namespace.
func buildSIOEvent(namespace []byte, event string, data []byte) []byte {
	name, _ := json.Marshal(event)
	var buf []byte
	buf = append(buf, eioMessage, sioEvent)
	if len(namespace) > 0 {
		buf = append(buf, namespace...)
		buf = append(buf, ',')
	}
	buf = append(buf, '[')
	buf = append(buf, name...)
	if len(data) > 0 {
		buf = append(buf, ',')
		buf = append(buf, data...)
	}
	buf = append(buf, ']')
	return buf
}

// parseSIOEvent parses the JSON-array payload of a Socket.IO EVENT packet.
//
// payload is the bytes after the "42" prefix, e.g. `["message",{"key":"val"}]`.
// It returns the event name and the raw JSON of the first data argument.
// When the client sends multiple arguments they are returned as a JSON array.
func parseSIOEvent(payload []byte) (string, []byte, error) {
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
	if len(arr) == 1 {
		return eventName, nil, nil
	}
	if len(arr) == 2 {
		return eventName, []byte(arr[1]), nil
	}
	// Multiple args: return remaining args as a JSON array
	rest, err := json.Marshal(arr[1:])
	if err != nil {
		return "", nil, fmt.Errorf("socketio: failed to serialize event args: %w", err)
	}
	return eventName, rest, nil
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

type Websocket struct {
	once sync.Once
	mu   sync.RWMutex
	// The Fiber.Websocket connection
	Conn *websocket.Conn
	// Define if the connection is alive or not
	isAlive bool
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
	// Attributes map collection for the connection
	attributes map[string]interface{}
	// Unique id of the connection
	UUID string
	// Wrap Fiber Locals function
	Locals func(key string) interface{}
	// Wrap Fiber Params function
	Params func(key string, defaultValue ...string) string
	// Wrap Fiber Query function
	Query func(key string, defaultValue ...string) string
	// Wrap Fiber Cookies function
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

type safeListeners struct {
	sync.RWMutex
	list map[string][]eventCallback
}

func (l *safeListeners) set(event string, callback eventCallback) {
	l.Lock()
	listeners.list[event] = append(listeners.list[event], callback)
	l.Unlock()
}

func (l *safeListeners) get(event string) []eventCallback {
	l.RLock()
	defer l.RUnlock()
	if _, ok := l.list[event]; !ok {
		return make([]eventCallback, 0)
	}

	ret := make([]eventCallback, 0)
	ret = append(ret, l.list[event]...)
	return ret
}

//nolint:unused
func (l *safeListeners) reset() {
	l.Lock()
	l.list = make(map[string][]eventCallback)
	l.Unlock()
}

// List of the listeners for the events
var listeners = safeListeners{
	list: make(map[string][]eventCallback),
}

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
			queue:      make(chan message, 100),
			done:       make(chan struct{}, 1),
			attributes: make(map[string]interface{}),
			isAlive:    true,
		}

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
		go kws.send(ctx)

		// 3. Execute the user callback on a fully established socket.
		callback(kws)

		// 4. Notify listeners that the socket is ready.
		kws.fireEvent(EventConnect, nil, nil)

		// 5. Read / heartbeat goroutines and block until the connection closes.
		kws.run()
	}, config...)
}

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
	// 1. Send EIO OPEN
	open := eioOpenPacket{
		SID:          kws.UUID,
		Upgrades:     []string{},
		PingInterval: int(PingInterval.Milliseconds()),
		PingTimeout:  int(PingTimeout.Milliseconds()),
		MaxPayload:   1_000_000,
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

	// Extract optional namespace from the CONNECT packet so the ack echoes it
	// and so outbound events can mirror it.
	// Wire format: "40" [ "/namespace," ] [ <json_auth> ]
	namespace := extractSIONamespace(msg[2:])
	kws.mu.Lock()
	kws.namespace = append(kws.namespace[:0], namespace...)
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
	if len(data) == 0 || data[0] != '/' {
		return nil
	}
	if idx := bytes.IndexByte(data, ','); idx >= 0 {
		return data[:idx]
	}
	// Namespace without trailing comma (no auth payload).
	return data
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

// Broadcast to all the active connections
// except avoid broadcasting the message to itself
func (kws *Websocket) Broadcast(message []byte, except bool, mType ...int) {
	for wsUUID := range pool.all() {
		if except && kws.UUID == wsUUID {
			continue
		}
		err := kws.EmitTo(wsUUID, message, mType...)
		if err != nil {
			kws.fireEvent(EventError, message, err)
		}
	}
}

// Broadcast to all the active connections
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
// For named events use EmitEvent instead.
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

// EmitEvent sends a named socket.io event to the client.
// The data parameter should be valid JSON.
func (kws *Websocket) EmitEvent(event string, data []byte) {
	kws.write(TextMessage, buildSIOEvent(kws.getNamespace(), event, data))
}

// Close Actively close the connection from the server
func (kws *Websocket) Close() {
	// Notify the client with a Socket.IO DISCONNECT packet (mirroring the
	// namespace) before closing.
	disconnect := []byte{eioMessage, sioDisconnect}
	if ns := kws.getNamespace(); len(ns) > 0 {
		disconnect = append(disconnect, ns...)
	}
	kws.write(TextMessage, disconnect)
	kws.write(CloseMessage, []byte("Connection closed"))
	kws.fireEvent(EventClose, nil, nil)
}

// getNamespace returns the Socket.IO namespace this connection is bound to,
// or nil for the root namespace.
func (kws *Websocket) getNamespace() []byte {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	if len(kws.namespace) == 0 {
		return nil
	}
	out := make([]byte, len(kws.namespace))
	copy(out, kws.namespace)
	return out
}

func (kws *Websocket) IsAlive() bool {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return kws.isAlive
}

func (kws *Websocket) hasConn() bool {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return kws.Conn.Conn != nil
}

func (kws *Websocket) setAlive(alive bool) {
	kws.mu.Lock()
	defer kws.mu.Unlock()
	kws.isAlive = alive
}

//nolint:all
func (kws *Websocket) queueLength() int {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return len(kws.queue)
}

// pong sends Engine.IO PING packets to the client at regular PingInterval intervals.
// The client is expected to respond with an EIO PONG ("3") within PongTimeout.
func (kws *Websocket) pong(ctx context.Context) {
	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			kws.write(TextMessage, []byte{eioPing})
		case <-ctx.Done():
			return
		}
	}
}

// Add in message queue
func (kws *Websocket) write(messageType int, messageBytes []byte) {
	kws.queue <- message{
		mType:   messageType,
		data:    messageBytes,
		retries: 0,
	}
}

// Send out message queue
func (kws *Websocket) send(ctx context.Context) {
	for {
		select {
		case message := <-kws.queue:
			if !kws.hasConn() {
				if message.retries <= MaxSendRetry {
					// retry without blocking the sending thread
					go func() {
						time.Sleep(RetrySendTimeout)
						message.retries = message.retries + 1
						kws.queue <- message
					}()
				}
				continue
			}

			kws.mu.RLock()
			err := kws.Conn.WriteMessage(message.mType, message.data)
			kws.mu.RUnlock()

			if err != nil {
				kws.disconnected(err)
			}
		case <-ctx.Done():
			return
		}
	}
}

// run starts the heartbeat and read goroutines and blocks until the connection
// is torn down. The handshake, send goroutine and EventConnect notification
// are intentionally performed in New() before run() is called, so that any
// Emit/EmitEvent calls inside the user callback are flushed onto an already
// established connection and are not interleaved with handshake frames.
//
// Needs to be blocking, otherwise the connection would close.
func (kws *Websocket) run() {
	ctx := kws.ctx
	if ctx == nil {
		// Defensive: should always be set by New() but allow standalone use.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		kws.ctx = ctx
		kws.cancelCtx = cancel
		go kws.send(ctx)
	}

	go kws.pong(ctx)
	go kws.read(ctx)

	<-kws.done // block until disconnected closes the channel

	if kws.cancelCtx != nil {
		kws.cancelCtx()
	}
}

// Listen for incoming messages
// and filter by message type
func (kws *Websocket) read(ctx context.Context) {
	timeoutTicker := time.NewTicker(ReadTimeout)
	defer timeoutTicker.Stop()
	for {
		select {
		case <-timeoutTicker.C:
			if !kws.hasConn() {
				continue
			}

			kws.mu.RLock()
			mType, msg, err := kws.Conn.ReadMessage()
			kws.mu.RUnlock()

			// WebSocket-level control frames
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

			// Binary messages (socket.io binary events / raw binary data)
			if mType == BinaryMessage {
				kws.fireEvent(EventMessage, msg, nil)
				continue
			}

			// Text messages: parse Engine.IO packet
			if mType != TextMessage || len(msg) == 0 {
				continue
			}

			switch msg[0] {
			case eioPong:
				// EIO PONG — client's response to our PING
				kws.fireEvent(EventPong, nil, nil)

			case eioPing:
				// EIO PING from client (unusual in EIO v4 but handle gracefully)
				kws.write(TextMessage, []byte{eioPong})
				kws.fireEvent(EventPing, nil, nil)

			case eioClose:
				kws.disconnected(nil)
				return

			case eioNoop:
				// No-op, ignore

			case eioMessage:
				// Socket.IO packet wrapped in an EIO MESSAGE
				kws.handleSIOPacket(msg[1:])

			default:
				// Unknown EIO packet type — surface as a raw message
				kws.fireEvent(EventMessage, msg, nil)
			}

		case <-ctx.Done():
			return
		}
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
		eventName, eventData, err := parseSIOEvent(data)
		if err != nil {
			kws.fireEvent(EventError, payload, err)
			return
		}
		kws.fireEvent(eventName, eventData, nil)

	case sioDisconnect:
		kws.disconnected(nil)

	case sioConnect:
		// Late namespace CONNECT (after the initial handshake): confirm via
		// the send queue, mirroring the namespace, so we do not race the read
		// loop.
		ackPayload, err := json.Marshal(struct {
			SID string `json:"sid"`
		}{SID: kws.UUID})
		if err == nil {
			ns := extractSIONamespace(payload[1:])
			kws.write(TextMessage, buildSIOConnectAck(ns, ackPayload))
		}

	case sioAck:
		// Acknowledge packets are intentionally ignored for now

	default:
		// Unknown SIO packet type — surface payload as a raw message
		kws.fireEvent(EventMessage, payload, nil)
	}
}

// When the connection closes, disconnected method
func (kws *Websocket) disconnected(err error) {
	kws.fireEvent(EventDisconnect, nil, err)

	// may be called multiple times from different go routines
	if kws.IsAlive() {
		kws.once.Do(func() {
			kws.setAlive(false)
			close(kws.done)
		})
	}

	// Fire error event if the connection is
	// disconnected by an error
	if err != nil {
		kws.fireEvent(EventError, nil, err)
	}

	// Remove the socket from the pool
	pool.delete(kws.UUID)
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
	callbacks := listeners.get(event)

	for _, callback := range callbacks {
		callback(&EventPayload{
			Kws:              kws,
			Name:             event,
			SocketUUID:       kws.UUID,
			SocketAttributes: kws.attributes,
			Data:             data,
			Error:            error,
		})
	}
}

type eventCallback func(payload *EventPayload)

// On Add listener callback for an event into the listeners list
func On(event string, callback eventCallback) {
	listeners.set(event, callback)
}
