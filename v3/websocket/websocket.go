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
	"slices"
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

	// Subprotocols lists the subprotocols the server supports in order of
	// preference. The first one the client also offers is negotiated; with no
	// match none is (RFC 6455 section 4.2.2).
	Subprotocols []string

	// Allowed Origin's based on the Origin header, this validate the request origin to
	// prevent cross-site request forgery. Everything is allowed if left empty.
	// Matching is case-insensitive (RFC 6454 section 4).
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

// defaultRecoverWriteTimeout bounds the error frame defaultRecover sends after
// a panic, so a peer that stopped reading cannot park the goroutine.
const defaultRecoverWriteTimeout = 5 * time.Second

// defaultRecoverMessage is what defaultRecover tells the peer. It is
// deliberately constant and carries nothing derived from the panic.
const defaultRecoverMessage = "internal error"

// defaultRecover is the default RecoverHandler: it logs the panic and stack to
// stderr and sends the peer a fixed error.
func defaultRecover(c *Conn) {
	if err := recover(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "panic: %v\n%s\n", err, debug.Stack()) //nolint:errcheck // This will never fail
		_ = c.SetWriteDeadline(time.Now().Add(defaultRecoverWriteTimeout))
		// Fixed payload: the panic value may carry internal detail and is already on
		// stderr.
		if err := c.WriteJSON(fiber.Map{"error": defaultRecoverMessage}); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "could not write error response: %v\n", err)
		}
	}
}

// keepHijackedConnsServers holds one sync.Once per fasthttp server, so the
// steady state per upgrade is a lock-free Load.
var keepHijackedConnsServers sync.Map // map[*fasthttp.Server]*sync.Once

// ensureKeepHijackedConns makes fasthttp leave upgraded connections open after
// the handler returns. It runs once per server.
func ensureKeepHijackedConns(server *fasthttp.Server) {
	once, ok := keepHijackedConnsServers.Load(server)
	if !ok {
		once, _ = keepHijackedConnsServers.LoadOrStore(server, new(sync.Once))
	}
	once.(*sync.Once).Do(func() {
		server.KeepHijackedConns = true
	})
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

	// Resolved once: no wildcard search per request, and the clone keeps a later
	// mutation of cfg.Origins from changing a mounted handler.
	allowAllOrigins := len(cfg.Origins) == 0 || slices.Contains(cfg.Origins, "*")
	allowedOrigins := slices.Clone(cfg.Origins)

	var upgrader = websocket.FastHTTPUpgrader{
		HandshakeTimeout:  cfg.HandshakeTimeout,
		Subprotocols:      cfg.Subprotocols,
		ReadBufferSize:    cfg.ReadBufferSize,
		WriteBufferSize:   cfg.WriteBufferSize,
		EnableCompression: cfg.EnableCompression,
		WriteBufferPool:   cfg.WriteBufferPool,
		// Record the status only. ctx.Error would Response.Reset, wiping headers
		// earlier middleware set and the Sec-WebSocket-Version header; the handler
		// returns a *fiber.Error instead.
		Error: func(fctx *fasthttp.RequestCtx, status int, _ error) {
			fctx.SetStatusCode(status)
		},
		CheckOrigin: func(fctx *fasthttp.RequestCtx) bool {
			if allowAllOrigins {
				return true
			}
			origin := utils.UnsafeString(fctx.Request.Header.Peek(fiber.HeaderOrigin))
			if origin == "" {
				return cfg.AllowEmptyOrigin
			}
			// Scheme and host of an origin are case-insensitive (RFC 6454 section 4).
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
		fctx := c.RequestCtx()
		// No upgrade signal at all: answer 426 before copying the request. A partial
		// handshake goes on to the upgrader for its 400.
		if !asksToUpgrade(fctx) {
			return fiber.ErrUpgradeRequired
		}
		ensureKeepHijackedConns(c.App().Server())

		conn := &Conn{}

		// Route params and the IP come from the Fiber context, which is recycled
		// before the hijack callback runs. Param names outlive the request; only the
		// values are copied.
		for _, name := range c.Route().Params {
			setEntry(&conn.params, name, utils.CopyString(c.Params(name)))
		}
		conn.ip = utils.CopyString(c.IP())
		// Copied while the middleware chain is still on the stack: the hijack
		// callback runs only after it has unwound.
		conn.capture(fctx)

		if err := upgrader.Upgrade(fctx, func(fconn *websocket.Conn) {
			conn.Conn = fconn

			returned := false
			// Runs after RecoverHandler. A handler that panicked cannot be trusted with
			// the socket and nothing else closes a hijacked connection; a normal return
			// leaves it open.
			defer func() {
				if !returned {
					_ = fconn.Close()
				}
			}()
			defer cfg.RecoverHandler(conn)
			handler(conn)
			returned = true
		}); err != nil { // Handshake rejected
			// The upgrader chose the RFC 6455 status: 403 for a bad Origin, 400 for a
			// malformed handshake, 405 for a non-GET; section 4.4 asks for the supported
			// version on rejection. A *fiber.Error keeps it on the ErrorHandler path.
			status := fctx.Response.StatusCode()
			c.Set(fiber.HeaderSecWebSocketVersion, supportedVersion)
			return fiber.NewError(status, utils.StatusMessage(status))
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

// A Conn is allocated per upgrade and never reused: the event helper reads
// these maps from listeners on other goroutines after the handler returns, so
// pooling them is unsafe. Each map is allocated only when the request carries
// that kind of data.

// capture copies the request's locals, query arguments, cookies and headers
// into the Conn before fasthttp recycles the RequestCtx.
func (conn *Conn) capture(fctx *fasthttp.RequestCtx) {
	fctx.VisitUserValues(func(key []byte, value interface{}) {
		setEntry(&conn.locals, string(key), value)
	})
	// No size hint: Args.Len counts repeated keys that collapse into one entry.
	for key, value := range fctx.QueryArgs().All() {
		setEntry(&conn.queries, string(key), string(value))
	}
	for key, value := range fctx.Request.Header.Cookies() {
		setEntry(&conn.cookies, string(key), string(value))
	}
	// Names are stored canonical so Headers is one probe. fasthttp delivers them
	// that way unless normalizing was disabled, server-wide or per request, which
	// is not observable here, so every name is checked; a canonical name comes
	// back without allocating.
	for key, value := range fctx.Request.Header.All() {
		setEntry(&conn.headers, string(utils.CanonicalHeaderKey(key)), string(value))
	}
}

// setEntry stores value under key in *m, allocating the map on first use so a
// connection that carries no entries of a kind never allocates one.
func setEntry[V any](m *map[string]V, key string, value V) {
	if *m == nil {
		*m = make(map[string]V)
	}
	(*m)[key] = value
}

// Locals makes it possible to pass interface{} values under string keys scoped to the request
// and therefore available to all following routes that match the request.
func (conn *Conn) Locals(key string, value ...interface{}) interface{} {
	if len(value) == 0 {
		return conn.locals[key]
	}
	setEntry(&conn.locals, key, value[0])
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
	// Names are canonical in the map; CanonicalHeaderKey returns key unchanged
	// when it already is.
	if v, ok := conn.headers[utils.CanonicalHeaderKey(key)]; ok {
		return v
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

// Close codes 1000-1011 and 1015 are defined in RFC 6455 section 11.7; 1012
// and 1013 come from the IANA registry. 1014 is not exported because the
// underlying library rejects it in both directions. Not every code may be
// sent: section 7.4.1 forbids 1005, 1006 and 1015, and the library accepts
// only 1000-1003, 1007-1013 and 3000-4999 on receive; the event package's
// sanitizeCloseCode applies that rule.
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

// asksToUpgrade reports whether the request carries any upgrade signal: an
// Upgrade header or an upgrade token in Connection. Unlike IsWebSocketUpgrade
// it accepts a partial handshake, so the upgrader can answer it with 400.
func asksToUpgrade(fctx *fasthttp.RequestCtx) bool {
	h := &fctx.Request.Header
	return h.ConnectionUpgrade() || len(h.Peek(fiber.HeaderUpgrade)) > 0
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
