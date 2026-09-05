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

// defaultRecoverMessage is what defaultRecover tells the peer. It is
// deliberately constant and carries nothing derived from the panic.
const defaultRecoverMessage = "internal error"

// defaultRecover is the RecoverHandler used when Config leaves one unset. It
// recovers the panic, writes the value and stack to stderr, and tells the peer
// only that something failed.
func defaultRecover(c *Conn) {
	if err := recover(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "panic: %v\n%s\n", err, debug.Stack()) //nolint:errcheck // This will never fail
		_ = c.SetWriteDeadline(time.Now().Add(defaultRecoverWriteTimeout))
		// A fixed payload: the stderr line above already gives the operator the
		// full value and stack, so anything derived from the panic here would
		// only widen what a remote peer can read out of it. panic("...") sent
		// its string verbatim and a custom error type could expose exported
		// fields or its own MarshalJSON. Applications that want to tell the peer
		// more should supply their own RecoverHandler.
		if err := c.WriteJSON(fiber.Map{"error": defaultRecoverMessage}); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "could not write error response: %v\n", err)
		}
	}
}

// keepHijackedConnsServers holds one sync.Once per fasthttp server this package
// has upgraded on. Every upgrade goes through the lookup, so the steady state is
// a lock free Load and a Once that has already run; Once.Do is also what makes
// the flag write visible to every request that takes that fast path.
var keepHijackedConnsServers sync.Map // map[*fasthttp.Server]*sync.Once

// ensureKeepHijackedConns sets KeepHijackedConns on the server the first time it
// is seen, so fasthttp leaves the upgraded connection open for this package to
// own once the request handler returns.
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

	// Resolve the origin policy once so the upgrade path never searches for the
	// wildcard per request. An empty list keeps the historical "allow everything"
	// default; the clone keeps a later mutation of the caller's slice from
	// changing the policy of an already mounted handler.
	allowAllOrigins := len(cfg.Origins) == 0 || slices.Contains(cfg.Origins, "*")
	allowedOrigins := slices.Clone(cfg.Origins)

	var upgrader = websocket.FastHTTPUpgrader{
		HandshakeTimeout:  cfg.HandshakeTimeout,
		Subprotocols:      cfg.Subprotocols,
		ReadBufferSize:    cfg.ReadBufferSize,
		WriteBufferSize:   cfg.WriteBufferSize,
		EnableCompression: cfg.EnableCompression,
		WriteBufferPool:   cfg.WriteBufferPool,
		// Only the status is recorded here. Left to itself the upgrader would
		// call ctx.Error, whose Response.Reset wipes every header earlier
		// middleware set (request IDs, CORS) together with the
		// Sec-WebSocket-Version header the upgrader adds just before. The
		// handler below turns the status into a *fiber.Error so the rejection
		// reaches the app's ErrorHandler like any other error.
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
		server := c.App().Server()
		ensureKeepHijackedConns(server)

		fctx := c.RequestCtx()
		conn := &Conn{}

		// Route params and the IP come from the Fiber context, which is
		// recycled before the hijack callback runs, so they are captured now.
		// The param names belong to the parsed route and stay valid for the
		// lifetime of the app; only the values are copied out of the request
		// buffer.
		for _, name := range c.Route().Params {
			setEntry(&conn.params, name, utils.CopyString(c.Params(name)))
		}
		conn.ip = utils.CopyString(c.IP())

		if err := upgrader.Upgrade(fctx, func(fconn *websocket.Conn) {
			conn.Conn = fconn
			// Everything else is copied only once the handshake has been
			// accepted, so a rejected one pays for none of it. fasthttp keeps
			// the RequestCtx alive until this callback returns.
			conn.capture(fctx, !server.DisableHeaderNamesNormalizing)

			returned := false
			// Registered before the recover handler so it runs after it. A
			// handler that panicked cannot be trusted to still own the socket,
			// and KeepHijackedConns means nothing else will ever close it, so
			// close it here -- but only once RecoverHandler has had its chance
			// to write the error frame. A handler that returns normally leaves
			// the socket open, so it may keep using the connection from another
			// goroutine after returning.
			defer func() {
				if !returned {
					_ = fconn.Close()
				}
			}()
			defer cfg.RecoverHandler(conn)
			handler(conn)
			returned = true
		}); err != nil { // Handshake rejected
			// The upgrader picked the status RFC 6455 asks for: 403 for an
			// Origin the server does not accept (section 4.2.2), 400 for a
			// malformed or unsupported handshake, 405 for a non-GET request.
			// Section 4.4 also has a server that turns a handshake down
			// advertise the versions it understands. Returning a *fiber.Error
			// with that status keeps the rejection on the same path as every
			// other error: a custom ErrorHandler formats it, error logging and
			// metrics see it.
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

// A Conn is allocated per upgrade and never reused. An earlier revision pooled
// it and emptied the maps in place for the next connection, which is unsafe:
// the event helper's Locals, Params, Query and Cookies closures read those maps
// from listeners that run on other goroutines during Close and CloseAll, so a
// new upgrade taking the wrapper could clear and refill them under a listener
// still reading the previous client's data. The maps are only allocated when
// the request carries that kind of data, so a typical handshake still allocates
// less than unconditionally creating all five.

// capture copies the locals, query arguments, cookies and headers of the
// request into the Conn. It runs inside the hijack callback, where the
// RequestCtx is still valid, and after the handshake has been accepted.
// canonical says whether the server already normalizes header names.
func (conn *Conn) capture(fctx *fasthttp.RequestCtx, canonical bool) {
	fctx.VisitUserValues(func(key []byte, value interface{}) {
		setEntry(&conn.locals, string(key), value)
	})
	// No size hint for the query map: Args.Len counts repeated keys that the
	// assignments collapse into one entry, so it would over-allocate.
	for key, value := range fctx.QueryArgs().All() {
		setEntry(&conn.queries, string(key), string(value))
	}
	for key, value := range fctx.Request.Header.Cookies() {
		setEntry(&conn.cookies, string(key), string(value))
	}
	// Header names are stored in canonical form so Headers is a single probe.
	// fasthttp delivers them that way already unless the server disabled
	// normalizing, and only then is each name rewritten here.
	for key, value := range fctx.Request.Header.All() {
		if !canonical {
			key = utils.CanonicalHeaderKey(key)
		}
		setEntry(&conn.headers, string(key), string(value))
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
	// capture stores names in canonical form, so the canonical form of key is
	// one map probe. CanonicalHeaderKey returns key itself when it is already
	// canonical, which is the usual spelling, and allocates a small copy
	// otherwise.
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

// Close codes 1000-1011 and 1015 are defined in RFC 6455, section 11.7; 1012
// and 1013 were added to the IANA WebSocket Close Code Number Registry
// afterwards. 1014 is registered too but is deliberately not exported: the
// underlying library rejects it in both directions, so it can neither be sent
// nor received through this stack.
//
// Not every code may travel in a close frame: RFC 6455, section 7.4.1 forbids
// sending 1005, 1006 and 1015, and the underlying library rejects anything but
// 1000-1003, 1007-1013 and 3000-4999 on receive. The event package's
// sanitizeCloseCode is the one place that rule is applied.
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
