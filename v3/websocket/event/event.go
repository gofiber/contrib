// Package event provides a plain WebSocket event helper built on top of the
// websocket middleware.
package event

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
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
	// PingMessage denotes a ping control frame.
	PingMessage = 9
	// PongMessage denotes a pong control frame.
	PongMessage = 10
)

// Supported event list.
const (
	// EventMessage is fired when a text or binary message is received.
	EventMessage = "message"
	// EventPing is fired when a WebSocket ping control frame is received.
	EventPing = "ping"
	// EventPong is fired when a WebSocket pong control frame is received.
	EventPong = "pong"
	// EventDisconnect is fired when the connection is closed.
	EventDisconnect = "disconnect"
	// EventConnect is fired when the connection is initialized.
	EventConnect = "connect"
	// EventClose is fired when the server actively closes the connection.
	EventClose = "close"
	// EventError is fired when an error occurs.
	EventError = "error"
)

var (
	// ErrorInvalidConnection indicates that the addressed connection is no
	// longer available.
	ErrorInvalidConnection = errors.New("message cannot be delivered invalid/gone connection")
	// ErrorUUIDDuplication indicates that the UUID already exists in the pool.
	ErrorUUIDDuplication = errors.New("UUID already exists in the available connections pool")
)

var (
	// PongTimeout controls how often this helper sends a WebSocket pong frame.
	PongTimeout = time.Second
	// RetrySendTimeout controls how long a queued message waits before retrying.
	RetrySendTimeout = 20 * time.Millisecond
	// MaxSendRetry defines the max retries for transient socket write issues.
	MaxSendRetry = 5
	// ReadTimeout controls the pause between read attempts.
	ReadTimeout = 10 * time.Millisecond
)

type message struct {
	mType   int
	data    []byte
	retries int
}

// EventPayload stores information about an event and its connection.
type EventPayload struct {
	// Kws is the connection object.
	Kws *Websocket
	// Name is the event name.
	Name string
	// SocketUUID is the unique connection UUID.
	SocketUUID string
	// SocketAttributes is a snapshot of connection attributes.
	SocketAttributes map[string]any
	// Error is populated for disconnect and error events.
	Error error
	// Data is used on message, custom, and error events.
	Data []byte
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
	Close()
	pong(ctx context.Context)
	write(messageType int, messageBytes []byte)
	run()
	read(ctx context.Context)
	disconnected(err error)
	createUUID() string
	randomUUID() string
	fireEvent(event string, data []byte, err error)
}

// Websocket wraps a websocket.Conn with event-bus helpers.
type Websocket struct {
	once sync.Once
	mu   sync.RWMutex
	// Conn is the underlying Fiber websocket connection.
	Conn *websocket.Conn
	// isAlive defines if the connection is alive or not.
	isAlive bool
	// queue stores outbound messages.
	queue chan message
	// done signals goroutines to stop gracefully.
	done chan struct{}
	// doneOnce closes done exactly once.
	doneOnce sync.Once
	// attributes stores optional connection-scoped values.
	attributes map[string]interface{}
	// UUID is the unique connection identifier.
	UUID string
	// Locals wraps Fiber Locals.
	Locals func(key string) interface{}
	// Params wraps Fiber Params.
	Params func(key string, defaultValue ...string) string
	// Query wraps Fiber Query.
	Query func(key string, defaultValue ...string) string
	// Cookies wraps Fiber Cookies.
	Cookies func(key string, defaultValue ...string) string
}

type safePool struct {
	sync.RWMutex
	conn map[string]ws
}

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
	ret := make(map[string]ws, len(p.conn))
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
	l.list[event] = append(l.list[event], callback)
	l.Unlock()
}

func (l *safeListeners) get(event string) []eventCallback {
	l.RLock()
	defer l.RUnlock()
	if _, ok := l.list[event]; !ok {
		return make([]eventCallback, 0)
	}

	ret := make([]eventCallback, 0, len(l.list[event]))
	ret = append(ret, l.list[event]...)
	return ret
}

var listeners = safeListeners{
	list: make(map[string][]eventCallback),
}

// New returns a Fiber handler that upgrades the request to WebSocket and wraps
// it with the event helper.
func New(callback func(kws *Websocket), config ...websocket.Config) fiber.Handler {
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

		kws.UUID = kws.createUUID()
		pool.set(kws)

		callback(kws)
		kws.fireEvent(EventConnect, nil, nil)
		kws.run()
	}, config...)
}

// GetUUID returns the connection UUID.
func (kws *Websocket) GetUUID() string {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return kws.UUID
}

// SetUUID updates the connection UUID and its pool entry.
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

// SetAttribute sets an attribute for the connection.
func (kws *Websocket) SetAttribute(key string, attribute interface{}) {
	kws.mu.Lock()
	defer kws.mu.Unlock()
	kws.attributes[key] = attribute
}

// GetAttribute returns an attribute from the connection.
func (kws *Websocket) GetAttribute(key string) interface{} {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	value, ok := kws.attributes[key]
	if ok {
		return value
	}
	return nil
}

// GetIntAttribute retrieves an attribute as an int.
func (kws *Websocket) GetIntAttribute(key string) int {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	value, ok := kws.attributes[key]
	if ok {
		if v, ok := value.(int); ok {
			return v
		}
	}
	return 0
}

// GetStringAttribute retrieves an attribute as a string.
func (kws *Websocket) GetStringAttribute(key string) string {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	value, ok := kws.attributes[key]
	if ok {
		if v, ok := value.(string); ok {
			return v
		}
	}
	return ""
}

// EmitToList emits a message to a list of connection UUIDs.
func (kws *Websocket) EmitToList(uuids []string, message []byte, mType ...int) {
	for _, wsUUID := range uuids {
		err := kws.EmitTo(wsUUID, message, mType...)
		if err != nil {
			kws.fireEvent(EventError, message, err)
		}
	}
}

// EmitToList emits a message to a list of connection UUIDs and ignores errors.
func EmitToList(uuids []string, message []byte, mType ...int) {
	for _, wsUUID := range uuids {
		_ = EmitTo(wsUUID, message, mType...)
	}
}

// EmitTo emits a message to a connection UUID.
func (kws *Websocket) EmitTo(uuid string, message []byte, mType ...int) error {
	conn, err := pool.get(uuid)
	if err != nil {
		kws.fireEvent(EventError, []byte(uuid), ErrorInvalidConnection)
		return ErrorInvalidConnection
	}
	if !conn.IsAlive() {
		kws.fireEvent(EventError, []byte(uuid), ErrorInvalidConnection)
		return ErrorInvalidConnection
	}

	conn.Emit(message, mType...)
	return nil
}

// EmitTo emits a message to a connection UUID.
func EmitTo(uuid string, message []byte, mType ...int) error {
	conn, err := pool.get(uuid)
	if err != nil {
		return ErrorInvalidConnection
	}
	if !conn.IsAlive() {
		return ErrorInvalidConnection
	}

	conn.Emit(message, mType...)
	return nil
}

// Broadcast emits to all active connections except itself when except is true.
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

// Broadcast emits to all active connections.
func Broadcast(message []byte, mType ...int) {
	for _, kws := range pool.all() {
		kws.Emit(message, mType...)
	}
}

// Fire fires a custom event on the current connection.
func (kws *Websocket) Fire(event string, data []byte) {
	kws.fireEvent(event, data, nil)
}

// Fire fires a custom event on all active connections.
func Fire(event string, data []byte) {
	fireGlobalEvent(event, data, nil)
}

// Emit writes a message to the current connection.
func (kws *Websocket) Emit(message []byte, mType ...int) {
	t := TextMessage
	if len(mType) > 0 {
		t = mType[0]
	}
	kws.write(t, message)
}

// Close actively closes the current connection from the server.
func (kws *Websocket) Close() {
	kws.write(CloseMessage, []byte("Connection closed"))
	kws.fireEvent(EventClose, nil, nil)
}

// IsAlive reports whether the connection is active.
func (kws *Websocket) IsAlive() bool {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return kws.isAlive
}

func (kws *Websocket) hasConn() bool {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return kws.Conn != nil && kws.Conn.Conn != nil
}

func (kws *Websocket) setAlive(alive bool) {
	kws.mu.Lock()
	defer kws.mu.Unlock()
	kws.isAlive = alive
}

func (kws *Websocket) pong(ctx context.Context) {
	timeoutTicker := time.NewTicker(PongTimeout)
	defer timeoutTicker.Stop()
	for {
		select {
		case <-timeoutTicker.C:
			kws.write(PongMessage, []byte{})
		case <-ctx.Done():
			return
		}
	}
}

func (kws *Websocket) write(messageType int, messageBytes []byte) {
	msg := message{
		mType:   messageType,
		data:    messageBytes,
		retries: 0,
	}

	select {
	case kws.queue <- msg:
	case <-kws.done:
	}
}

func (kws *Websocket) send(ctx context.Context) {
	for {
		select {
		case msg := <-kws.queue:
			if !kws.hasConn() {
				if msg.retries <= MaxSendRetry {
					go func(msg message) {
						time.Sleep(RetrySendTimeout)
						msg.retries++
						select {
						case kws.queue <- msg:
						case <-ctx.Done():
						case <-kws.done:
						}
					}(msg)
				}
				continue
			}

			err := kws.Conn.WriteMessage(msg.mType, msg.data)
			if err != nil {
				kws.disconnected(err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (kws *Websocket) run() {
	ctx, cancelFunc := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	wg.Add(3)
	go func() {
		defer wg.Done()
		kws.pong(ctx)
	}()
	go func() {
		defer wg.Done()
		kws.read(ctx)
	}()
	go func() {
		defer wg.Done()
		kws.send(ctx)
	}()

	<-kws.done
	cancelFunc()
	kws.closeConn()
	wg.Wait()
}

func (kws *Websocket) read(ctx context.Context) {
	timeoutTicker := time.NewTicker(ReadTimeout)
	defer timeoutTicker.Stop()
	for {
		select {
		case <-timeoutTicker.C:
			if !kws.hasConn() {
				continue
			}

			mType, msg, err := kws.Conn.ReadMessage()

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

			kws.fireEvent(EventMessage, msg, nil)
		case <-ctx.Done():
			return
		}
	}
}

func (kws *Websocket) disconnected(err error) {
	kws.fireEvent(EventDisconnect, nil, err)

	if kws.IsAlive() {
		kws.once.Do(func() {
			kws.setAlive(false)
			kws.doneOnce.Do(func() {
				close(kws.done)
			})
		})
	}

	if err != nil {
		kws.fireEvent(EventError, nil, err)
	}

	pool.delete(kws.UUID)
}

func (kws *Websocket) closeConn() {
	kws.mu.RLock()
	conn := kws.Conn
	kws.mu.RUnlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (kws *Websocket) createUUID() string {
	return kws.randomUUID()
}

func (kws *Websocket) randomUUID() string {
	return uuid.New().String()
}

func fireGlobalEvent(event string, data []byte, err error) {
	for _, kws := range pool.all() {
		kws.fireEvent(event, data, err)
	}
}

func (kws *Websocket) fireEvent(event string, data []byte, err error) {
	callbacks := listeners.get(event)

	kws.mu.RLock()
	attrs := make(map[string]any, len(kws.attributes))
	for key, value := range kws.attributes {
		attrs[key] = value
	}
	kws.mu.RUnlock()

	for _, callback := range callbacks {
		callback(&EventPayload{
			Kws:              kws,
			Name:             event,
			SocketUUID:       kws.GetUUID(),
			SocketAttributes: attrs,
			Data:             data,
			Error:            err,
		})
	}
}

type eventCallback func(payload *EventPayload)

// On adds a listener callback for an event.
func On(event string, callback eventCallback) {
	listeners.set(event, callback)
}
