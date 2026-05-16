// Package event provides a plain WebSocket event helper built on top of the
// websocket middleware.
package event

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
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
	// PongTimeout is the interval between server-originated Ping frames.
	// Despite its name, this helper uses Ping for liveness; the historical
	// name is preserved for backwards compatibility. The value must be less
	// than any upstream proxy or load balancer idle timeout.
	//
	// Deprecated: prefer Config.PingInterval passed to NewWithConfig. The
	// package-level value is read once per connection at upgrade time;
	// mutating it after that has no effect on running connections.
	PongTimeout = time.Second
	// RetrySendTimeout controls how long a queued message waits before retrying.
	RetrySendTimeout = 20 * time.Millisecond
	// MaxSendRetry defines the max retries for transient socket write issues.
	MaxSendRetry = 5
	// SendQueueSize controls the per-connection outbound message queue size.
	SendQueueSize = 100
	// ReadTimeout is deprecated and no longer used; reads block until a
	// message arrives or the connection is closed.
	//
	// Deprecated: ReadTimeout is a no-op. Configure Config.ReadIdleTimeout
	// on NewWithConfig for the actual read deadline behaviour.
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

// Config tunes a single event helper instance. Zero values fall back to the
// matching deprecated package-level var, which itself falls back to the
// historical default. Pass via NewWithConfig.
type Config struct {
	// PingInterval is the interval between server-originated Ping frames.
	// Must be less than any upstream proxy or load balancer idle timeout.
	// Zero falls back to PongTimeout, then 1s.
	PingInterval time.Duration
	// ReadIdleTimeout bounds how long a connection may stay silent before
	// it is considered dead. Zero falls back to 3 * PingInterval.
	ReadIdleTimeout time.Duration
	// WriteTimeout bounds a single WriteMessage or WriteControl call. Zero
	// falls back to 10s.
	WriteTimeout time.Duration
	// MaxMessageSize bounds the largest inbound frame in bytes. Zero falls
	// back to 1 MiB. To opt out, set math.MaxInt64.
	MaxMessageSize int64
	// SendQueueSize is the per-connection outbound buffer. Zero falls back
	// to SendQueueSize package var, then 100.
	SendQueueSize int
	// MaxSendRetry caps retries for transient socket write issues. Zero
	// falls back to MaxSendRetry package var, then 5.
	MaxSendRetry int
	// RetrySendTimeout is the wait between retries. Zero falls back to
	// RetrySendTimeout package var, then 20ms.
	RetrySendTimeout time.Duration
	// RecoverHandler is called on a panic inside a user On callback. If
	// nil, panics are recovered silently.
	RecoverHandler func(event string, r any)
}

// settings is the per-connection immutable snapshot.
type settings struct {
	pingInterval     time.Duration
	readIdleTimeout  time.Duration
	writeTimeout     time.Duration
	maxMessageSize   int64
	sendQueueSize    int
	maxSendRetry     int
	retrySendTimeout time.Duration
	recover          func(event string, r any)
}

func resolveSettings(cfg Config) settings {
	s := settings{
		pingInterval:     cfg.PingInterval,
		readIdleTimeout:  cfg.ReadIdleTimeout,
		writeTimeout:     cfg.WriteTimeout,
		maxMessageSize:   cfg.MaxMessageSize,
		sendQueueSize:    cfg.SendQueueSize,
		maxSendRetry:     cfg.MaxSendRetry,
		retrySendTimeout: cfg.RetrySendTimeout,
		recover:          cfg.RecoverHandler,
	}
	if s.pingInterval <= 0 {
		s.pingInterval = PongTimeout
		if s.pingInterval <= 0 {
			s.pingInterval = time.Second
		}
	}
	if s.readIdleTimeout <= 0 {
		s.readIdleTimeout = 3 * s.pingInterval
	}
	if s.writeTimeout <= 0 {
		s.writeTimeout = 10 * time.Second
	}
	if s.maxMessageSize <= 0 {
		s.maxMessageSize = 1 << 20
	}
	if s.sendQueueSize <= 0 {
		s.sendQueueSize = SendQueueSize
		if s.sendQueueSize <= 0 {
			s.sendQueueSize = 1
		}
	}
	if s.maxSendRetry <= 0 {
		s.maxSendRetry = MaxSendRetry
		if s.maxSendRetry <= 0 {
			s.maxSendRetry = 5
		}
	}
	if s.retrySendTimeout <= 0 {
		s.retrySendTimeout = RetrySendTimeout
		if s.retrySendTimeout <= 0 {
			s.retrySendTimeout = 20 * time.Millisecond
		}
	}
	return s
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
	once      sync.Once
	closeOnce sync.Once
	mu        sync.RWMutex
	// Conn is the underlying Fiber websocket connection.
	Conn *websocket.Conn
	// settings holds the per-connection immutable tuning snapshot.
	settings settings
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

func (p *safePool) delete(key string) {
	p.Lock()
	delete(p.conn, key)
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

	return append([]eventCallback(nil), l.list[event]...)
}

var listeners = safeListeners{
	list: make(map[string][]eventCallback),
}

// New returns a Fiber handler that upgrades the request to WebSocket and wraps
// it with the event helper using default tuning. For per-instance tuning use
// NewWithConfig.
func New(callback func(kws *Websocket), config ...websocket.Config) fiber.Handler {
	return NewWithConfig(callback, Config{}, config...)
}

// NewWithConfig returns a Fiber handler that upgrades the request to WebSocket
// and wraps it with the event helper, using the supplied per-instance tuning.
func NewWithConfig(callback func(kws *Websocket), eventCfg Config, wsConfig ...websocket.Config) fiber.Handler {
	s := resolveSettings(eventCfg)
	return websocket.New(func(c *websocket.Conn) {
		kws := &Websocket{
			Conn:     c,
			settings: s,
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
			queue:      make(chan message, s.sendQueueSize),
			done:       make(chan struct{}),
			attributes: make(map[string]interface{}),
			isAlive:    true,
		}

		kws.UUID = kws.createUUID()
		pool.set(kws)

		callback(kws)
		kws.fireEvent(EventConnect, nil, nil)
		kws.run()
	}, wsConfig...)
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

// closeFrameMaxReason caps the reason payload in a close frame so the
// combined control frame stays within RFC 6455 §5.5's 125-byte limit
// (2 bytes status code + up to 123 bytes reason).
const closeFrameMaxReason = 123

// Close actively closes the current connection from the server.
func (kws *Websocket) Close() {
	if !kws.IsAlive() {
		return
	}

	kws.closeOnce.Do(func() {
		kws.writeClose(websocket.CloseNormalClosure, "Connection closed")
		kws.fireEvent(EventClose, nil, nil)
		kws.disconnected(nil)
	})
}

// writeClose sends an RFC 6455 compliant close control frame using
// WriteControl with a write deadline so shutdown cannot block on a slow
// peer.
func (kws *Websocket) writeClose(code int, reason string) {
	if len(reason) > closeFrameMaxReason {
		reason = reason[:closeFrameMaxReason]
	}
	kws.mu.RLock()
	conn := kws.Conn
	kws.mu.RUnlock()
	if conn == nil {
		return
	}
	payload := websocket.FormatCloseMessage(code, reason)
	deadline := time.Now().Add(kws.settings.writeTimeout)
	_ = conn.WriteControl(CloseMessage, payload, deadline)
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

// pong sends server-originated Ping control frames at PingInterval. The
// method is named "pong" purely to preserve the unexported ws interface; the
// frame type was corrected from Pong to Ping for RFC 6455 compliant
// liveness.
func (kws *Websocket) pong(ctx context.Context) {
	ticker := time.NewTicker(kws.settings.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			kws.mu.RLock()
			conn := kws.Conn
			kws.mu.RUnlock()
			if conn == nil {
				return
			}
			deadline := time.Now().Add(kws.settings.writeTimeout)
			if err := conn.WriteControl(PingMessage, nil, deadline); err != nil {
				kws.disconnected(err)
				return
			}
		case <-ctx.Done():
			return
		case <-kws.done:
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
				if msg.retries <= kws.settings.maxSendRetry {
					retryTimer := time.NewTimer(kws.settings.retrySendTimeout)
					select {
					case <-retryTimer.C:
					case <-ctx.Done():
						stopTimer(retryTimer)
						return
					case <-kws.done:
						stopTimer(retryTimer)
						return
					}

					msg.retries++
					select {
					case kws.queue <- msg:
					case <-ctx.Done():
						return
					case <-kws.done:
						return
					}
				}
				continue
			}

			kws.mu.RLock()
			conn := kws.Conn
			kws.mu.RUnlock()
			if conn == nil {
				continue
			}
			_ = conn.SetWriteDeadline(time.Now().Add(kws.settings.writeTimeout))
			err := conn.WriteMessage(msg.mType, msg.data)
			if err != nil {
				kws.drainQueue()
				kws.disconnected(err)
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// drainQueue discards all remaining queued messages so a closed connection
// does not pin payload memory until the channel is garbage collected.
func (kws *Websocket) drainQueue() {
	for {
		select {
		case <-kws.queue:
		default:
			return
		}
	}
}

func (kws *Websocket) run() {
	if kws.Conn != nil && kws.settings.maxMessageSize > 0 {
		kws.Conn.SetReadLimit(kws.settings.maxMessageSize)
	}

	if kws.Conn != nil {
		_ = kws.Conn.SetReadDeadline(time.Now().Add(kws.settings.readIdleTimeout))
		kws.Conn.SetPongHandler(func(string) error {
			_ = kws.Conn.SetReadDeadline(time.Now().Add(kws.settings.readIdleTimeout))
			kws.fireEvent(EventPong, nil, nil)
			return nil
		})
		kws.Conn.SetPingHandler(func(data string) error {
			kws.fireEvent(EventPing, []byte(data), nil)
			deadline := time.Now().Add(kws.settings.writeTimeout)
			err := kws.Conn.WriteControl(PongMessage, []byte(data), deadline)
			if errors.Is(err, websocket.ErrCloseSent) {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return nil
			}
			return err
		})
	}

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
	wg.Wait()
	kws.closeConn()
}

func (kws *Websocket) read(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-kws.done:
			return
		default:
		}

		kws.mu.RLock()
		conn := kws.Conn
		kws.mu.RUnlock()
		if conn == nil {
			return
		}

		mType, msg, err := conn.ReadMessage()
		if err != nil {
			// Control frames (Ping, Pong, Close) are handled by the
			// library's Set*Handler hooks above. An orderly client close
			// surfaces as a *CloseError here.
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				kws.disconnected(nil)
				return
			}
			kws.disconnected(err)
			return
		}

		switch mType {
		case TextMessage, BinaryMessage:
			kws.fireEvent(EventMessage, msg, nil)
		default:
			// Defensive: ReadMessage should not deliver control frames.
		}
	}
}

func (kws *Websocket) disconnected(err error) {
	disconnected := false
	kws.once.Do(func() {
		disconnected = true
		kws.setAlive(false)
		kws.doneOnce.Do(func() {
			close(kws.done)
		})
		pool.delete(kws.UUID)
	})

	if !disconnected {
		return
	}

	kws.fireEvent(EventDisconnect, nil, err)
	if err != nil {
		kws.fireEvent(EventError, nil, err)
	}
}

func (kws *Websocket) closeConn() {
	kws.mu.Lock()
	conn := kws.Conn
	kws.Conn = nil
	kws.mu.Unlock()
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

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func fireGlobalEvent(event string, data []byte, err error) {
	for _, kws := range pool.all() {
		kws.fireEvent(event, data, err)
	}
}

func (kws *Websocket) fireEvent(event string, data []byte, err error) {
	callbacks := listeners.get(event)
	if len(callbacks) == 0 {
		return
	}

	kws.mu.RLock()
	attrs := make(map[string]any, len(kws.attributes))
	for key, value := range kws.attributes {
		attrs[key] = value
	}
	kws.mu.RUnlock()

	// Clone payload bytes once before fan-out so listeners that retain the
	// slice are not exposed to the read buffer being reused by the next
	// frame.
	var payloadData []byte
	if data != nil {
		payloadData = make([]byte, len(data))
		copy(payloadData, data)
	}

	uuid := kws.GetUUID()
	for _, callback := range callbacks {
		kws.invokeCallback(event, callback, &EventPayload{
			Kws:              kws,
			Name:             event,
			SocketUUID:       uuid,
			SocketAttributes: attrs,
			Data:             payloadData,
			Error:            err,
		})
	}
}

// invokeCallback runs a single listener callback with panic recovery so a
// faulty user listener cannot tear down the read or send goroutine.
func (kws *Websocket) invokeCallback(event string, cb eventCallback, p *EventPayload) {
	defer func() {
		if r := recover(); r != nil {
			if kws.settings.recover != nil {
				kws.settings.recover(event, r)
			}
		}
	}()
	cb(p)
}

type eventCallback func(payload *EventPayload)

// On adds a listener callback for an event.
func On(event string, callback eventCallback) {
	listeners.set(event, callback)
}

var draining atomic.Bool

// IsDraining reports whether the package is in draining mode. Upgrade
// handlers can poll this to refuse new connections during a graceful
// shutdown.
func IsDraining() bool {
	return draining.Load()
}

// Drain marks the package as draining. New connections are not refused
// automatically; the upgrade gate is the caller's responsibility (a
// middleware that checks IsDraining and returns 503).
func Drain() {
	draining.Store(true)
}

// CloseAll iterates every active connection in the in-process pool and
// sends a close control frame with the supplied code and reason, then
// waits for each helper's run() loop to exit. If ctx expires first,
// remaining connections are force closed via Conn.Close.
//
// The typical usage is from a Fiber shutdown hook:
//
//	app.Hooks().OnShutdown(func() error {
//	    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
//	    defer cancel()
//	    event.Drain()
//	    return event.CloseAll(ctx, websocket.CloseGoingAway, "server shutting down")
//	})
//
// Reason is capped at 123 bytes per RFC 6455.
func CloseAll(ctx context.Context, code int, reason string) error {
	snapshot := pool.all()
	if len(snapshot) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	for _, c := range snapshot {
		kws, ok := c.(*Websocket)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(kws *Websocket) {
			defer wg.Done()
			kws.closeOnce.Do(func() {
				kws.writeClose(code, reason)
				kws.fireEvent(EventClose, nil, nil)
				kws.disconnected(nil)
			})
		}(kws)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		for _, c := range pool.all() {
			kws, ok := c.(*Websocket)
			if !ok {
				continue
			}
			kws.closeConn()
		}
		return ctx.Err()
	}
}
