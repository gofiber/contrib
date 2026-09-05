// Package event provides a plain WebSocket event helper built on top of the
// websocket middleware.
package event

import (
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

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
	// EventDisconnect is fired when the connection is closed. When the close is
	// caused by an error, EventError is fired as well (with the same error).
	EventDisconnect = "disconnect"
	// EventConnect is fired when the connection is initialized, immediately
	// after the New / NewWithConfig callback has run and before the read loop
	// starts.
	EventConnect = "connect"
	// EventClose is fired when the server actively closes the connection (for
	// example via Close or CloseAll).
	EventClose = "close"
	// EventError is fired when an error occurs, including a failed EmitTo, a
	// dropped outbound message, and error-driven disconnects.
	EventError = "error"
)

var (
	// ErrorInvalidConnection indicates that the addressed connection is no
	// longer available.
	ErrorInvalidConnection = errors.New("message cannot be delivered invalid/gone connection")
	// ErrorUUIDDuplication indicates that the UUID already exists in the pool.
	ErrorUUIDDuplication = errors.New("UUID already exists in the available connections pool")
	// ErrorCallbackPanic is carried by the EventError fired when a connection
	// callback panics, and by the EventDisconnect fired with it unless the
	// callback had already closed the connection.
	ErrorCallbackPanic = errors.New("connection callback panicked")
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
	disconnected(err error) bool
	createUUID() string
	randomUUID() string
	fireEvent(event string, data []byte, err error)
	fireOwnedEvent(event string, data []byte, err error)
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
	// localListeners holds listeners registered for this connection only via
	// the On method. They fire in addition to the process-global listeners and
	// are discarded when the connection disconnects.
	localListeners safeListeners
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
	// snap caches the last snapshot until conn changes.
	snap []ws
}

var pool = safePool{
	conn: make(map[string]ws),
}

func (p *safePool) set(ws ws) {
	p.Lock()
	p.conn[ws.GetUUID()] = ws
	p.snap = nil
	p.Unlock()
}

// snapshot returns the live connections. The slice is shared until membership
// changes and must not be modified.
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

type safeListeners struct {
	mu sync.Mutex
	// list is replaced wholesale on every change, so readers load a pointer
	// instead of taking a lock on every frame.
	list atomic.Pointer[map[string][]EventCallback]
}

// set registers callback for event. Published maps and slices are never
// modified, so get hands them out without copying.
func (l *safeListeners) set(event string, callback EventCallback) {
	l.mu.Lock()
	defer l.mu.Unlock()
	next := make(map[string][]EventCallback)
	if cur := l.list.Load(); cur != nil {
		maps.Copy(next, *cur)
	}
	next[event] = append(slices.Clip(next[event]), callback)
	l.list.Store(&next)
}

// get returns the callbacks registered for event, or nil when there are none.
// The result is a snapshot that must only be read, never appended to.
func (l *safeListeners) get(event string) []EventCallback {
	cur := l.list.Load()
	if cur == nil {
		return nil
	}
	return (*cur)[event]
}

func (l *safeListeners) remove(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cur := l.list.Load()
	if cur == nil {
		return
	}
	if _, ok := (*cur)[event]; !ok {
		return
	}
	next := make(map[string][]EventCallback, len(*cur))
	maps.Copy(next, *cur)
	delete(next, event)
	l.list.Store(&next)
}

var listeners safeListeners

// New returns a Fiber handler that upgrades the request to WebSocket and wraps
// it with the event helper using default tuning. For per-instance tuning use
// NewWithConfig.
func New(callback func(kws *Websocket), config ...websocket.Config) fiber.Handler {
	return NewWithConfig(callback, Config{}, config...)
}

// NewWithConfig returns a Fiber handler that upgrades the request to WebSocket
// and wraps it with the event helper, using the supplied per-instance tuning.
func NewWithConfig(callback func(kws *Websocket), eventCfg Config, wsConfig ...websocket.Config) fiber.Handler {
	if callback == nil {
		panic("websocket/event: callback must not be nil")
	}
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

		// If callback panics before run starts, the pool entry would keep its done
		// channel open forever and every later Broadcast would block on its queue.
		// Detach and disconnect; the middleware closes the socket after
		// RecoverHandler.
		completed := false
		defer func() {
			if completed {
				return
			}
			kws.mu.Lock()
			kws.Conn = nil
			kws.mu.Unlock()
			// A callback that closed the connection before panicking already spent the
			// one disconnect; still report the crash.
			if !kws.disconnected(ErrorCallbackPanic) {
				kws.fireEvent(EventError, nil, ErrorCallbackPanic)
			}
		}()

		callback(kws)
		kws.fireEvent(EventConnect, nil, nil)
		kws.run()
		completed = true
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

	// Validate before mutating so a duplicate leaves kws untouched and no
	// rollback is needed. On conflict kws keeps prevUUID and stays in the
	// pool exactly as before.
	if existing, ok := pool.conn[uuid]; ok && existing != kws {
		return ErrorUUIDDuplication
	}

	kws.UUID = uuid
	if prevUUID != "" {
		delete(pool.conn, prevUUID)
	}
	pool.conn[uuid] = kws
	// Every write to conn drops the cached snapshot.
	pool.snap = nil
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

// EmitToList emits a message to a list of connection UUIDs. Each failed UUID
// fires EventError on the originating connection (kws); unlike the package-level
// EmitToList, errors are not silently ignored.
func (kws *Websocket) EmitToList(uuids []string, message []byte, mType ...int) {
	for _, wsUUID := range uuids {
		// EmitTo already fires EventError on failure, so it is not fired again
		// here to avoid a duplicate event per failed UUID.
		_ = kws.EmitTo(wsUUID, message, mType...)
	}
}

// EmitToList emits a message to a list of connection UUIDs. Per-UUID errors are
// silently ignored and, unlike the (*Websocket).EmitToList method form, no
// EventError is fired.
func EmitToList(uuids []string, message []byte, mType ...int) {
	for _, wsUUID := range uuids {
		_ = EmitTo(wsUUID, message, mType...)
	}
}

// EmitTo emits a message to a connection UUID. On an invalid or dead target it
// fires EventError on the originating connection (kws) and returns the error;
// the package-level EmitTo does not fire EventError.
func (kws *Websocket) EmitTo(uuid string, message []byte, mType ...int) error {
	conn, err := pool.get(uuid)
	if err != nil {
		kws.fireOwnedEvent(EventError, []byte(uuid), ErrorInvalidConnection)
		return ErrorInvalidConnection
	}
	if !conn.IsAlive() {
		kws.fireOwnedEvent(EventError, []byte(uuid), ErrorInvalidConnection)
		return ErrorInvalidConnection
	}

	conn.Emit(message, mType...)
	return nil
}

// EmitTo emits a message to a connection UUID. It returns the error to the
// caller and, unlike the (*Websocket).EmitTo method form, does not fire
// EventError.
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

// Broadcast emits to all active connections, skipping the originating
// connection (kws) when except is true. Each failed target fires EventError on
// kws; the package-level Broadcast does not.
func (kws *Websocket) Broadcast(message []byte, except bool, mType ...int) {
	for _, conn := range pool.snapshot() {
		// Identity rather than UUID: no lock, and right under a concurrent SetUUID.
		if except && conn == ws(kws) {
			continue
		}
		// The snapshot already resolved every target; one that died meanwhile still
		// reports EventError on kws.
		if !conn.IsAlive() {
			kws.fireOwnedEvent(EventError, []byte(conn.GetUUID()), ErrorInvalidConnection)
			continue
		}
		conn.Emit(message, mType...)
	}
}

// Broadcast emits to all active connections.
func Broadcast(message []byte, mType ...int) {
	for _, kws := range pool.snapshot() {
		kws.Emit(message, mType...)
	}
}

// On registers a listener for the event on this connection only. Unlike the
// package-level On, which is process-global, these listeners fire only for
// events on this connection and are discarded when it disconnects. Both
// per-connection and global listeners fire for a given event.
func (kws *Websocket) On(event string, callback EventCallback) {
	kws.localListeners.set(event, callback)
}

// Off removes all listeners registered for the event on this connection via the
// On method. It does not affect process-global listeners registered with the
// package-level On.
func (kws *Websocket) Off(event string) {
	kws.localListeners.remove(event)
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
	conn := kws.conn()
	if conn == nil {
		return
	}
	payload := websocket.FormatCloseMessage(sanitizeCloseCode(code), closeReason(reason))
	deadline := time.Now().Add(kws.settings.writeTimeout)
	_ = conn.WriteControl(CloseMessage, payload, deadline)
}

// sanitizeCloseCode replaces a status code the peer would reject with a normal
// closure. RFC 6455 section 7.4.1 forbids sending 1005, 1006 and 1015 and
// leaves 1004 reserved, and this library accepts only 1000-1003, 1007-1013 and
// 3000-4999 on receive, so anything else fails the connection with a protocol
// error. 1005 passes through: FormatCloseMessage renders it as the empty
// payload, which is how "no status" is expressed.
func sanitizeCloseCode(code int) int {
	switch code {
	case websocket.CloseNoStatusReceived:
		return code
	case websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseProtocolError,
		websocket.CloseUnsupportedData,
		websocket.CloseInvalidFramePayloadData,
		websocket.ClosePolicyViolation,
		websocket.CloseMessageTooBig,
		websocket.CloseMandatoryExtension,
		websocket.CloseInternalServerErr,
		websocket.CloseServiceRestart,
		websocket.CloseTryAgainLater:
		return code
	}
	// 3000-3999 is registered for libraries and frameworks, 4000-4999 is
	// private use; both are accepted by a conformant peer.
	if code >= 3000 && code <= 4999 {
		return code
	}
	return websocket.CloseNormalClosure
}

// closeReason makes reason valid UTF-8 (RFC 6455 section 5.5.1) and cuts it
// back to closeFrameMaxReason bytes on a rune boundary.
func closeReason(reason string) string {
	reason = strings.ToValidUTF8(reason, "")
	if len(reason) <= closeFrameMaxReason {
		return reason
	}
	cut := closeFrameMaxReason
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut]
}

// IsAlive reports whether the connection is active.
func (kws *Websocket) IsAlive() bool {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return kws.isAlive
}

// conn returns the underlying connection, or nil once closeConn has cleared it.
func (kws *Websocket) conn() *websocket.Conn {
	kws.mu.RLock()
	defer kws.mu.RUnlock()
	return kws.Conn
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
			conn := kws.conn()
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
			// One observation of the connection per message, so a message cannot be
			// dropped between two disagreeing reads.
			conn := kws.conn()
			if conn == nil {
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
					continue
				}
				// Retries exhausted while the connection is not ready: drop the
				// message and fire EventError so the caller is not left
				// believing it was delivered. The dequeue above freed a slot,
				// so a single Emit from the handler will not block.
				kws.fireEvent(EventError, msg.data, ErrorInvalidConnection)
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
	// The handlers close over the connection value: they run on the read
	// goroutine while closeConn may clear kws.Conn.
	if conn := kws.conn(); conn != nil {
		if kws.settings.maxMessageSize > 0 {
			conn.SetReadLimit(kws.settings.maxMessageSize)
		}
		_ = conn.SetReadDeadline(time.Now().Add(kws.settings.readIdleTimeout))
		conn.SetPongHandler(func(string) error {
			// Best-effort short circuit so a peer that keeps ponging does not
			// keep firing events once shutdown started. Shutdown correctness
			// does not depend on winning this race: run closes the underlying
			// connection, which no deadline refresh can undo.
			select {
			case <-kws.done:
				return nil
			default:
			}
			_ = conn.SetReadDeadline(time.Now().Add(kws.settings.readIdleTimeout))
			kws.fireEvent(EventPong, nil, nil)
			return nil
		})
		conn.SetPingHandler(func(data string) error {
			// Same short circuit as the pong handler; RFC 6455 section 5.5.2 lets an
			// endpoint skip the pong once a close is under way.
			select {
			case <-kws.done:
				return nil
			default:
			}
			// A ping proves the peer is alive just as a pong does, so the idle
			// deadline restarts here too.
			_ = conn.SetReadDeadline(time.Now().Add(kws.settings.readIdleTimeout))
			kws.fireOwnedEvent(EventPing, []byte(data), nil)
			deadline := time.Now().Add(kws.settings.writeTimeout)
			err := conn.WriteControl(PongMessage, []byte(data), deadline)
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
	// Unblock a read goroutine parked in ReadMessage before waiting on it,
	// otherwise a peer that never sends anything keeps run from reaching
	// closeConn.
	kws.unblockRead()
	wg.Wait()
	kws.closeConn()
}

func (kws *Websocket) read(ctx context.Context) {
	// run installs the connection before this goroutine starts and closeConn
	// clears it only after it exits, so read it once rather than under the mutex
	// on every frame.
	conn := kws.conn()
	if conn == nil {
		return
	}

	// A frame proves the peer is alive, so the idle deadline moves on data as
	// well as on pongs, but only once per quarter of the timeout: moving it is a
	// runtime timer update.
	refreshEvery := kws.settings.readIdleTimeout / 4
	var nextRefresh time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-kws.done:
			return
		default:
		}

		mType, msg, err := readFrame(conn)
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

		if now := time.Now(); now.After(nextRefresh) {
			_ = conn.SetReadDeadline(now.Add(kws.settings.readIdleTimeout))
			nextRefresh = now.Add(refreshEvery)
		}

		switch mType {
		case TextMessage, BinaryMessage:
			// readFrame returns a copy made for this frame, so the fan-out owns it.
			kws.fireOwnedEvent(EventMessage, msg, nil)
		default:
			// Defensive: NextReader never delivers control frames.
		}
	}
}

// frameReader reads one message into a growable buffer and returns an
// exact-size copy, where ReadMessage's io.ReadAll starts at 512 bytes and
// doubles. Readers are pooled: an idle connection holds no buffer and the GC
// trims the pool.
type frameReader struct {
	buf   []byte
	probe [1]byte
}

const (
	// frameBufferInitial is what a fresh buffer starts at, io.ReadAll's own
	// figure.
	frameBufferInitial = 512
	// frameBufferRetained caps what goes back into the pool, so one big frame does
	// not leave its size behind.
	frameBufferRetained = 64 << 10
)

var framePool = sync.Pool{New: func() interface{} { return new(frameReader) }}

// readFrame reads the next data message from conn and returns a copy the
// caller owns.
func readFrame(conn *websocket.Conn) (int, []byte, error) {
	mType, r, err := conn.NextReader()
	if err != nil {
		return mType, nil, err
	}
	fr := framePool.Get().(*frameReader)
	msg, err := fr.readAll(r)
	fr.release()
	return mType, msg, err
}

func (fr *frameReader) readAll(r io.Reader) ([]byte, error) {
	buf := fr.buf[:0]
	if cap(buf) == 0 {
		buf = make([]byte, 0, frameBufferInitial)
	}
	for {
		if len(buf) == cap(buf) {
			// Full: probe one byte so a message that exactly fills the buffer does not
			// double it.
			n, err := r.Read(fr.probe[:])
			if n > 0 {
				grown := make([]byte, len(buf), 2*cap(buf))
				copy(grown, buf)
				buf = append(grown, fr.probe[0])
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				fr.buf = buf
				return nil, err
			}
			continue
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fr.buf = buf
			return nil, err
		}
	}
	msg := make([]byte, len(buf))
	copy(msg, buf)
	fr.buf = buf
	return msg, nil
}

// release puts the reader back into the pool, minus a buffer that grew past
// frameBufferRetained.
func (fr *frameReader) release() {
	fr.trim()
	framePool.Put(fr)
}

func (fr *frameReader) trim() {
	if cap(fr.buf) > frameBufferRetained {
		fr.buf = nil
		return
	}
	fr.buf = fr.buf[:0]
}

// disconnected tears the connection down once and reports whether this call
// was the one that did it; later calls are no-ops that return false.
func (kws *Websocket) disconnected(err error) bool {
	disconnected := false
	kws.once.Do(func() {
		disconnected = true
		kws.setAlive(false)
		kws.doneOnce.Do(func() {
			close(kws.done)
		})
		pool.delete(kws.GetUUID())
	})

	if !disconnected {
		return false
	}

	kws.fireEvent(EventDisconnect, nil, err)
	if err != nil {
		kws.fireEvent(EventError, nil, err)
	}
	return true
}

// unblockRead closes the underlying network connection so a read goroutine
// blocked in ReadMessage returns immediately.
//
// Close is the only read-unblocking operation the websocket package documents
// as safe to call concurrently with the read methods, so it is used here
// instead of SetReadDeadline, which the package classifies as a read method
// and which a pong handler could immediately push back into the future. The
// Conn field is deliberately left in place: closeConn performs the final
// cleanup once the goroutines have exited, and closing twice is harmless.
func (kws *Websocket) unblockRead() {
	if conn := kws.conn(); conn != nil {
		_ = conn.Close()
	}
}

func (kws *Websocket) closeConn() {
	kws.mu.Lock()
	conn := kws.Conn
	kws.Conn = nil
	kws.mu.Unlock()
	// Close is nil-receiver safe and closing twice is harmless.
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
	// Every connection gets its own copy: Data is mutable, and one listener must
	// not change what another connection's listeners see.
	for _, kws := range pool.snapshot() {
		kws.fireEvent(event, data, err)
	}
}

// fireEvent delivers event to every listener with a copy of data, so a
// listener that retains the slice is not exposed to the caller reusing it.
func (kws *Websocket) fireEvent(event string, data []byte, err error) {
	kws.fire(event, data, err, true)
}

// fireOwnedEvent is fireEvent for data the fan-out already owns and need not
// copy.
func (kws *Websocket) fireOwnedEvent(event string, data []byte, err error) {
	kws.fire(event, data, err, false)
}

func (kws *Websocket) fire(event string, data []byte, err error, clone bool) {
	// Both registries hand back immutable snapshots: no locks, no copies, and an
	// event nobody listens for costs two map lookups.
	globalCallbacks := listeners.get(event)
	localCallbacks := kws.localListeners.get(event)
	if len(globalCallbacks) == 0 && len(localCallbacks) == 0 {
		return
	}
	if clone {
		data = cloneBytes(data)
	}

	kws.mu.RLock()
	attrs := maps.Clone(kws.attributes)
	socketUUID := kws.UUID
	kws.mu.RUnlock()

	// Each listener gets its own EventPayload value; the attribute map and the
	// Data bytes are shared, as they always were.
	payload := EventPayload{
		Kws:              kws,
		Name:             event,
		SocketUUID:       socketUUID,
		SocketAttributes: attrs,
		Data:             data,
		Error:            err,
	}
	for _, callback := range globalCallbacks {
		p := payload
		kws.invokeCallback(event, callback, &p)
	}
	for _, callback := range localCallbacks {
		p := payload
		kws.invokeCallback(event, callback, &p)
	}
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// invokeCallback runs a single listener callback with panic recovery so a
// faulty user listener cannot tear down the read or send goroutine.
func (kws *Websocket) invokeCallback(event string, cb EventCallback, p *EventPayload) {
	defer func() {
		if r := recover(); r != nil {
			if kws.settings.recover != nil {
				kws.settings.recover(event, r)
			}
		}
	}()
	cb(p)
}

// EventCallback is the listener signature invoked when an event fires.
type EventCallback func(payload *EventPayload)

// On registers a process-global listener for an event. The callback fires for
// that event on every connection created by New / NewWithConfig, regardless of
// route or Config, and stays registered until removed with Off. For listeners
// scoped to a single connection use the (*Websocket).On method instead.
func On(event string, callback EventCallback) {
	listeners.set(event, callback)
}

// Off removes all process-global listeners registered for the event via On. It
// is the counterpart to On and does not affect per-connection listeners
// registered through (*Websocket).On.
func Off(event string) {
	listeners.remove(event)
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

// CloseAll sends every connection in the pool a close frame with code and
// reason, fires EventClose and marks it disconnected, and returns once that is
// done for all of them; each connection's goroutines then wind down on their
// own. If ctx expires first, the remaining connections are force closed.
//
// Typical usage from a Fiber shutdown hook:
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
	conns := pool.snapshot()
	if len(conns) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	for _, c := range conns {
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
		for _, c := range pool.snapshot() {
			kws, ok := c.(*Websocket)
			if !ok {
				continue
			}
			// Mark disconnected (clears isAlive, removes from pool) before the
			// force close so later emits cannot target a closed connection.
			kws.disconnected(ctx.Err())
			kws.closeConn()
		}
		return ctx.Err()
	}
}
