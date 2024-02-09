package ikisocket

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
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
	PongTimeout = 1 * time.Second
	// RetrySendTimeout retry after 20 ms if there is an error
	RetrySendTimeout = 20 * time.Millisecond
	//MaxSendRetry define max retries if there are socket issues
	MaxSendRetry = 5
	// ReadTimeout Instead of reading in a for loop, try to avoid full CPU load taking some pause
	ReadTimeout = 10 * time.Millisecond
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
	SocketAttributes map[string]interface{}
	// Optional error when are fired events like
	// - Disconnect
	// - Error
	Error error
	// Data is used on Message and on Error event
	Data []byte
}

type ws interface {
	IsAlive() bool
	GetUUID() string
	SetUUID(uuid string)
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
	fireEvent(event string, data []byte, error error)
}

type Websocket struct {
	mu sync.RWMutex
	// The Fiber.Websocket connection
	Conn *websocket.Conn
	// Define if the connection is alive or not
	isAlive bool
	// Queue of messages sent from the socket
	queue chan message
	// Channel to signal when this websocket is closed
	// so go routines will stop gracefully
	done chan struct{}
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

func (p *safePool) get(key string) ws {
	p.RLock()
	ret, ok := p.conn[key]
	p.RUnlock()
	if !ok {
		panic("not found")
	}
	return ret
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
	for _, v := range l.list[event] {
		ret = append(ret, v)
	}
	return ret
}

// List of the listeners for the events
var listeners = safeListeners{
	list: make(map[string][]eventCallback),
}

func New(callback func(kws *Websocket)) func(*fiber.Ctx) error {
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

		// execute the callback of the socket initialization
		callback(kws)

		kws.fireEvent(EventConnect, nil, nil)

		// Run the loop for the given connection
		kws.run()
	})
}

func (kws *Websocket) GetUUID() string {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return kws.UUID
}

func (kws *Websocket) SetUUID(uuid string) {
	kws.mu.Lock()
	defer kws.mu.Unlock()

	if pool.contains(uuid) {
		panic(ErrorUUIDDuplication)
	}
	kws.UUID = uuid
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
// Will panic if attribute is not an int.
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

	if !pool.contains(uuid) || !pool.get(uuid).IsAlive() {
		kws.fireEvent(EventError, []byte(uuid), ErrorInvalidConnection)
		return ErrorInvalidConnection
	}

	pool.get(uuid).Emit(message, mType...)
	return nil
}

// EmitTo Emit to a specific socket connection
func EmitTo(uuid string, message []byte, mType ...int) error {
	if !pool.contains(uuid) || !pool.get(uuid).IsAlive() {
		return ErrorInvalidConnection
	}

	pool.get(uuid).Emit(message, mType...)
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

// Emit /Write the message into the given connection
func (kws *Websocket) Emit(message []byte, mType ...int) {
	t := TextMessage
	if len(mType) > 0 {
		t = mType[0]
	}
	kws.write(t, message)
}

// Close Actively close the connection from the server
func (kws *Websocket) Close() {
	kws.write(CloseMessage, []byte("Connection closed"))
	kws.fireEvent(EventClose, nil, nil)
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

func (kws *Websocket) queueLength() int {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return len(kws.queue)
}

// pong writes a control message to the client
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

// Start Pong/Read/Write functions
//
// Needs to be blocking, otherwise the connection would close.
func (kws *Websocket) run() {
	ctx, cancelFunc := context.WithCancel(context.Background())

	go kws.pong(ctx)
	go kws.read(ctx)
	go kws.send(ctx)

	<-kws.done // block until one event is sent to the done channel

	cancelFunc()
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

			// We have a message and we fire the message event
			kws.fireEvent(EventMessage, msg, nil)
		case <-ctx.Done():
			return
		}
	}
}

// When the connection closes, disconnected method
func (kws *Websocket) disconnected(err error) {
	kws.fireEvent(EventDisconnect, nil, err)

	// may be called multiple times from different go routines
	if kws.IsAlive() {
		close(kws.done)
	}
	kws.setAlive(false)

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
