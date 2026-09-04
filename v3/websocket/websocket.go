// 🚀 Fiber is an Express inspired web framework written in Go with 💖
// 📌 API Documentation: https://fiber.wiki
// 📝 Github Repository: https://github.com/gofiber/fiber

package websocket

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
	"github.com/valyala/fasthttp"
)

// Config ...
type Config struct {
	// Next defines a function to skip this middleware when it returns true.
	// Optional. Default: nil
	Next func(fiber.Ctx) bool

	// HandshakeTimeout specifies the duration for the handshake to complete.
	HandshakeTimeout time.Duration

	// Subprotocols lists the subprotocols this server supports, in order of
	// preference. The handshake negotiates the first entry that also appears in
	// the client's Sec-WebSocket-Protocol header; when nothing matches, no
	// subprotocol is negotiated and the response header is omitted, as required
	// by RFC 6455 section 4.2.2.
	Subprotocols []string

	// Allowed Origin's based on the Origin header, this validate the request origin to
	// prevent cross-site request forgery. Everything is allowed if left empty.
	// Comparison is case-insensitive, matching the case-insensitive scheme and
	// host of a serialized origin (RFC 6454, section 4).
	Origins []string

	// AllowEmptyOrigin allows WebSocket connections when the Origin header is absent.
	// When false (default), connections without an Origin header are rejected unless Origins includes "*".
	// Set to true to allow connections from non-browser clients that don't send Origin headers.
	// Optional. Default: false
	AllowEmptyOrigin bool

	// ReadBufferSize and WriteBufferSize specify I/O buffer sizes in bytes. If a buffer
	// size is zero, then a useful default size is used. The I/O buffer sizes
	// do not limit the size of the messages that can be sent or received.
	ReadBufferSize, WriteBufferSize int

	// WriteBufferPool is a pool of buffers for write operations. If the value
	// is not set, then write buffers are allocated to the connection for the
	// lifetime of the connection.
	//
	// A pool is most useful when the application has a modest volume of writes
	// across a large number of connections.
	//
	// Applications should use a single pool for each unique value of
	// WriteBufferSize.
	WriteBufferPool websocket.BufferPool

	// EnableCompression specifies if the client should attempt to negotiate
	// per message compression (RFC 7692). Setting this value to true does not
	// guarantee that compression will be supported. Currently only "no context
	// takeover" modes are supported.
	EnableCompression bool

	// RecoverHandler is a panic handler function that recovers from panics
	// Default recover function is used when nil and writes error message in a response field `error`
	// It prints stack trace to the stderr by default
	// Optional. Default: defaultRecover
	RecoverHandler func(*Conn)
}

// supportedVersion is the only WebSocket protocol version defined by RFC 6455.
const supportedVersion = "13"

// defaultRecoverWriteTimeout bounds the courtesy error frame defaultRecover
// sends after a panic. The handler is already unwinding, so a peer that stopped
// reading must not be able to park the hijacked goroutine and its file
// descriptor forever.
const defaultRecoverWriteTimeout = 5 * time.Second

func defaultRecover(c *Conn) {
	if err := recover(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "panic: %v\n%s\n", err, debug.Stack()) //nolint:errcheck // This will never fail
		if c.Conn == nil {
			return
		}
		_ = c.SetWriteDeadline(time.Now().Add(defaultRecoverWriteTimeout))
		// The panic value is rendered before encoding: panic(err) is by far the
		// most common shape and an error marshals to "{}", so the client would
		// otherwise be told an error occurred without being told which.
		if err := c.WriteJSON(fiber.Map{"error": fmt.Sprint(err)}); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "could not write error response: %v\n", err)
		}
	}
}

// keepHijackedConnsServers records the fasthttp servers whose KeepHijackedConns
// flag this package has already set. Every upgrade goes through the lookup, so
// it uses a sync.Map to stay lock free once a server has been seen; the entry is
// only published after the flag is written, which is what lets a later reader
// take the fast path and still observe the flag.
var (
	keepHijackedConnsMu      sync.Mutex
	keepHijackedConnsServers sync.Map // map[*fasthttp.Server]struct{}
)

func ensureKeepHijackedConns(server *fasthttp.Server) {
	if server == nil {
		return
	}
	if _, ok := keepHijackedConnsServers.Load(server); ok {
		return
	}
	keepHijackedConnsMu.Lock()
	defer keepHijackedConnsMu.Unlock()
	if _, ok := keepHijackedConnsServers.Load(server); ok {
		return
	}
	server.KeepHijackedConns = true
	keepHijackedConnsServers.Store(server, struct{}{})
}

// New returns a new `handler func(*Conn)` that upgrades a client to the
// websocket protocol, you can pass an optional config.
func New(handler func(*Conn), config ...Config) fiber.Handler {
	if handler == nil {
		panic("websocket: handler must not be nil")
	}

	// Init config
	var cfg Config
	if len(config) > 0 {
		cfg = config[0]
	}
	if cfg.ReadBufferSize == 0 {
		cfg.ReadBufferSize = 1024
	}
	if cfg.WriteBufferSize == 0 {
		cfg.WriteBufferSize = 1024
	}
	if cfg.RecoverHandler == nil {
		cfg.RecoverHandler = defaultRecover
	}

	// Resolve the origin policy once so the upgrade path only walks the
	// explicit entries instead of searching for the wildcard per request. An
	// empty list keeps the historical "allow everything" default. The list is
	// copied so mutating the caller's slice afterwards cannot change the policy
	// of an already mounted handler.
	allowAllOrigins := len(cfg.Origins) == 0
	allowedOrigins := make([]string, 0, len(cfg.Origins))
	for _, origin := range cfg.Origins {
		if origin == "*" {
			allowAllOrigins = true
			continue
		}
		allowedOrigins = append(allowedOrigins, origin)
	}

	var upgrader = websocket.FastHTTPUpgrader{
		HandshakeTimeout:  cfg.HandshakeTimeout,
		Subprotocols:      cfg.Subprotocols,
		ReadBufferSize:    cfg.ReadBufferSize,
		WriteBufferSize:   cfg.WriteBufferSize,
		EnableCompression: cfg.EnableCompression,
		WriteBufferPool:   cfg.WriteBufferPool,
		CheckOrigin: func(fctx *fasthttp.RequestCtx) bool {
			if allowAllOrigins {
				return true
			}
			origin := utils.UnsafeString(fctx.Request.Header.Peek(fiber.HeaderOrigin))
			if origin == "" {
				return cfg.AllowEmptyOrigin
			}
			// RFC 6454 section 4 serializes an origin as scheme "://" host
			// [":" port] and both the scheme and the host are case-insensitive,
			// so a client may legitimately send an origin whose case differs
			// from the configured one.
			for i := range allowedOrigins {
				if utils.EqualFold(allowedOrigins[i], origin) {
					return true
				}
			}
			return false
		},
	}
	return func(c fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}
		// A request that never asked to switch protocols cannot become a
		// WebSocket connection. Answering here keeps the whole request copy
		// below off the path taken by plain GETs and crawlers.
		if !IsWebSocketUpgrade(c) {
			return fiber.ErrUpgradeRequired
		}
		ensureKeepHijackedConns(c.App().Server())

		fctx := c.RequestCtx()
		conn := acquireConn()

		// locals
		fctx.VisitUserValues(func(key []byte, value interface{}) {
			if conn.locals == nil {
				conn.locals = make(map[string]interface{})
			}
			conn.locals[string(key)] = value
		})

		// params: the names belong to the parsed route and stay valid for the
		// lifetime of the app, so only the values are copied out of the request
		// buffer that fasthttp recycles.
		if params := c.Route().Params; len(params) > 0 {
			if conn.params == nil {
				conn.params = make(map[string]string, len(params))
			}
			for i := range params {
				conn.params[params[i]] = utils.CopyString(c.Params(params[i]))
			}
		}

		// queries
		if args := fctx.QueryArgs(); args.Len() > 0 {
			if conn.queries == nil {
				conn.queries = make(map[string]string, args.Len())
			}
			for key, value := range args.All() {
				conn.queries[string(key)] = string(value)
			}
		}

		// cookies
		for key, value := range fctx.Request.Header.Cookies() {
			if conn.cookies == nil {
				conn.cookies = make(map[string]string)
			}
			conn.cookies[string(key)] = string(value)
		}

		// headers
		if n := fctx.Request.Header.Len(); n > 0 {
			if conn.headers == nil {
				conn.headers = make(map[string]string, n)
			}
			for key, value := range fctx.Request.Header.All() {
				conn.headers[string(key)] = string(value)
			}
		}

		// ip address
		conn.ip = utils.CopyString(c.IP())

		if err := upgrader.Upgrade(fctx, func(fconn *websocket.Conn) {
			conn.Conn = fconn
			defer releaseConn(conn)
			defer cfg.RecoverHandler(conn)
			handler(conn)
		}); err != nil { // Handshake rejected
			releaseConn(conn)
			// The upgrader already wrote the status RFC 6455 asks for: 403 for
			// an Origin the server does not accept (section 4.2.2), 400 for a
			// malformed or unsupported handshake, 405 for a non-GET request.
			// Keeping it beats flattening every rejection to 426, which only
			// describes a client that has not tried to upgrade at all.
			//
			// fasthttp's ctx.Error resets the response on its way out and drops
			// the Sec-WebSocket-Version header the upgrader had just set, so it
			// is restored here: section 4.4 requires a server that turns a
			// handshake down to advertise the versions it does understand.
			c.Set(fiber.HeaderSecWebSocketVersion, supportedVersion)
			return nil
		}

		return nil
	}
}

// Conn https://godoc.org/github.com/gorilla/websocket#pkg-index
type Conn struct {
	*websocket.Conn
	locals  map[string]interface{}
	params  map[string]string
	cookies map[string]string
	headers map[string]string
	queries map[string]string
	ip      string
}

// Conn pool
var poolConn = sync.Pool{
	New: func() interface{} {
		return new(Conn)
	},
}

// maxPooledMapEntries bounds the maps carried by a pooled Conn. Reusing the
// buckets of a modest map is the point of the pool, but a single request
// carrying thousands of query arguments or headers would otherwise make every
// later connection pay for that footprint, so oversized maps are dropped.
const maxPooledMapEntries = 64

// Acquire Conn from pool
func acquireConn() *Conn {
	return poolConn.Get().(*Conn)
}

// Return Conn to pool
func releaseConn(conn *Conn) {
	conn.Conn = nil
	conn.ip = ""
	// Emptying the maps rather than replacing them keeps their buckets for the
	// next connection, and it stops a pooled Conn from pinning the finished
	// connection's locals, headers and cookies until it is picked up again.
	conn.locals = resetMap(conn.locals)
	conn.params = resetMap(conn.params)
	conn.queries = resetMap(conn.queries)
	conn.cookies = resetMap(conn.cookies)
	conn.headers = resetMap(conn.headers)
	poolConn.Put(conn)
}

// resetMap empties m so the next connection can reuse it, or discards it once
// it has grown past maxPooledMapEntries.
func resetMap[V any](m map[string]V) map[string]V {
	if len(m) > maxPooledMapEntries {
		return nil
	}
	clear(m)
	return m
}

// Locals makes it possible to pass interface{} values under string keys scoped to the request
// and therefore available to all following routes that match the request.
func (conn *Conn) Locals(key string, value ...interface{}) interface{} {
	if len(value) == 0 {
		return conn.locals[key]
	}
	if conn.locals == nil {
		conn.locals = make(map[string]interface{}, 1)
	}
	conn.locals[key] = value[0]
	return value[0]
}

// Params is used to get the route parameters.
// Defaults to empty string "" if the param doesn't exist.
// If a default value is given, it will return that value if the param doesn't exist.
func (conn *Conn) Params(key string, defaultValue ...string) string {
	v, ok := conn.params[key]
	if !ok && len(defaultValue) > 0 {
		return defaultValue[0]
	}
	return v
}

// Query returns the query string parameter in the url.
// Defaults to empty string "" if the query doesn't exist.
// If a default value is given, it will return that value if the query doesn't exist.
func (conn *Conn) Query(key string, defaultValue ...string) string {
	v, ok := conn.queries[key]
	if !ok && len(defaultValue) > 0 {
		return defaultValue[0]
	}
	return v
}

// Cookies is used for getting a cookie value by key
// Defaults to empty string "" if the cookie doesn't exist.
// If a default value is given, it will return that value if the cookie doesn't exist.
func (conn *Conn) Cookies(key string, defaultValue ...string) string {
	v, ok := conn.cookies[key]
	if !ok && len(defaultValue) > 0 {
		return defaultValue[0]
	}
	return v
}

// Headers is used for getting a header value by key
// Defaults to empty string "" if the header doesn't exist.
// If a default value is given, it will return that value if the header doesn't exist.
// Header lookups are case-insensitive.
func (conn *Conn) Headers(key string, defaultValue ...string) string {
	// fasthttp stores inbound header names in canonical form, so canonicalizing
	// the requested key turns the lookup into a single map probe instead of a
	// case-insensitive walk over every header of the request.
	if v, ok := conn.headers[utils.CanonicalHeaderKey(key)]; ok {
		return v
	}
	// Fall back to the scan for servers that disabled header name normalizing,
	// and for keys outside the RFC 9110 token alphabet, which
	// CanonicalHeaderKey deliberately leaves untouched.
	for k, v := range conn.headers {
		if utils.EqualFold(k, key) {
			return v
		}
	}
	if len(defaultValue) > 0 {
		return defaultValue[0]
	}
	return ""
}

// IP returns the client's network address
func (conn *Conn) IP() string {
	return conn.ip
}

// Constants are taken from https://github.com/fasthttp/websocket/blob/master/conn.go#L43

// Close codes 1000-1011 and 1015 are defined in RFC 6455, section 11.7; 1012,
// 1013 and 1014 were added to the IANA WebSocket Close Code Number Registry
// afterwards. Note that 1005, 1006 and 1015 are reserved for use by
// applications and must never be sent in a close frame (RFC 6455, section
// 7.4.1).
const (
	CloseNormalClosure           = 1000
	CloseGoingAway               = 1001
	CloseProtocolError           = 1002
	CloseUnsupportedData         = 1003
	CloseNoStatusReceived        = 1005
	CloseAbnormalClosure         = 1006
	CloseInvalidFramePayloadData = 1007
	ClosePolicyViolation         = 1008
	CloseMessageTooBig           = 1009
	CloseMandatoryExtension      = 1010
	CloseInternalServerErr       = 1011
	CloseServiceRestart          = 1012
	CloseTryAgainLater           = 1013
	CloseBadGateway              = 1014
	CloseTLSHandshake            = 1015
)

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

var (
	// ErrBadHandshake is returned when the server response to opening handshake is
	// invalid.
	ErrBadHandshake = errors.New("websocket: bad handshake")
	// ErrCloseSent is returned when the application writes a message to the
	// connection after sending a close message.
	ErrCloseSent = errors.New("websocket: close sent")
	// ErrReadLimit is returned when reading a message that is larger than the
	// read limit set for the connection.
	ErrReadLimit = errors.New("websocket: read limit exceeded")
)

// FormatCloseMessage formats closeCode and text as a WebSocket close message.
// An empty message is returned for code CloseNoStatusReceived.
func FormatCloseMessage(closeCode int, text string) []byte {
	return websocket.FormatCloseMessage(closeCode, text)
}

// IsCloseError returns boolean indicating whether the error is a *CloseError
// with one of the specified codes.
func IsCloseError(err error, codes ...int) bool {
	return websocket.IsCloseError(err, codes...)
}

// IsUnexpectedCloseError returns boolean indicating whether the error is a
// *CloseError with a code not in the list of expected codes.
func IsUnexpectedCloseError(err error, expectedCodes ...int) bool {
	return websocket.IsUnexpectedCloseError(err, expectedCodes...)
}

// IsWebSocketUpgrade returns true if the client requested upgrade to the
// WebSocket protocol.
func IsWebSocketUpgrade(c fiber.Ctx) bool {
	return websocket.FastHTTPIsWebSocketUpgrade(c.RequestCtx())
}

// JoinMessages concatenates received messages to create a single io.Reader.
// The string term is appended to each message. The returned reader does not
// support concurrent calls to the Read method.
func JoinMessages(c *websocket.Conn, term string) io.Reader {
	return websocket.JoinMessages(c, term)
}
