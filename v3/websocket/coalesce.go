package websocket

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
	"github.com/valyala/fasthttp"
)

// Write coalescing.
//
// fasthttp/websocket puts every frame on the wire with a write syscall of its
// own. A peer that pipelines frames gets its replies one packet at a time, and
// the server pays a syscall per frame however many arrived in a single read.
// The library never batches, but it only ever talks to a net.Conn, so a
// net.Conn the middleware owns can: hold replies while the reader is still
// working through input it already has, and push them out in one write the
// moment it comes back for more.
//
// Owning that net.Conn rules out FastHTTPUpgrader, which hijacks with
// fasthttp's own connection. With Config.CoalesceWrites the upgrade goes
// through the library's net/http Upgrader instead, handed a request built from
// the fasthttp one and an http.Hijacker backed by fasthttp's Hijack, so the
// handshake stays the library's while the connection underneath is ours.

const (
	// coalesceLimit caps what is held back. A frame that would not fit is
	// written directly, after whatever was pending, so order holds.
	coalesceLimit = 64 << 10
	// coalesceMaxDelay bounds how long a reply can wait for the reader to come
	// back. It only matters for a handler that writes and then does not read
	// again promptly. The timer is armed at most once per delay and is left to
	// expire, so a reply usually costs no timer operation at all.
	coalesceMaxDelay = time.Millisecond
	// coalesceRetain is the largest pending buffer kept between batches, so a
	// single big burst does not pin its memory to the connection for good.
	coalesceRetain = 16 << 10
	// hijackBufferSize sizes the bufio pair the library is given on hijack. It
	// is below the sizes at which the library would reuse them, so it allocates
	// its own read and write buffers from Config exactly as the fasthttp
	// upgrader does.
	hijackBufferSize = 16
)

// errNotAttached is returned by a read before fasthttp handed the socket over,
// which the library never attempts: during Upgrade it only writes.
var errNotAttached = errors.New("websocket: connection not attached yet")

// handshakeHeaders are the request headers the library's Upgrader looks at,
// in the canonical form its http.Header lookups index by.
var handshakeHeaders = [...]string{
	http.CanonicalHeaderKey(fiber.HeaderConnection),
	http.CanonicalHeaderKey(fiber.HeaderUpgrade),
	http.CanonicalHeaderKey(fiber.HeaderSecWebSocketVersion),
	http.CanonicalHeaderKey(fiber.HeaderSecWebSocketKey),
	http.CanonicalHeaderKey(fiber.HeaderSecWebSocketExtensions),
	http.CanonicalHeaderKey(fiber.HeaderSecWebSocketProtocol),
	http.CanonicalHeaderKey(fiber.HeaderOrigin),
}

// coalescingConn is the net.Conn handed to fasthttp/websocket when
// Config.CoalesceWrites is set.
//
// A write is deferred only while batch && !parked: the reader was handed
// input by its last fill and has not come back for more. Everything else goes
// straight to the socket, after anything pending, so nothing is ever
// reordered and a handler that only pushes never waits. Deferred bytes leave
// when the reader is about to block, when they pass coalesceLimit, on Close,
// or after coalesceMaxDelay, whichever comes first.
type coalescingConn struct {
	// src is fasthttp's hijacked connection. It owns whatever fasthttp buffered
	// past the upgrade request, so every read goes through it.
	src net.Conn
	// raw is the socket underneath src. Writes, deadlines and addresses go to
	// it directly; src adds only the read buffer and a Close that also returns
	// fasthttp's pooled wrapper. Nil until attach.
	raw net.Conn

	// batch: the last fill handed the reader input it may still be handling.
	// parked: the reader is blocked waiting for the peer.
	batch  atomic.Bool
	parked atomic.Bool

	mu       sync.Mutex
	pending  []byte
	timer    *time.Timer
	maxDelay time.Duration
	armed    bool
	closed   bool
	err      error // first write failure, sticky
}

func newCoalescingConn() *coalescingConn {
	return &coalescingConn{maxDelay: coalesceMaxDelay}
}

// attach binds the connection fasthttp handed over and sends what the library
// wrote during the handshake, the 101 response, under the handshake deadline
// when there is one.
func (c *coalescingConn) attach(src net.Conn, handshakeTimeout time.Duration) error {
	raw := src
	if u, ok := src.(interface{ UnsafeConn() net.Conn }); ok {
		// Reads must stay on src, which may hold bytes fasthttp buffered past the
		// request; writes can go to the socket itself.
		raw = u.UnsafeConn()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.src, c.raw = src, raw
	if handshakeTimeout > 0 {
		if err := raw.SetWriteDeadline(time.Now().Add(handshakeTimeout)); err != nil {
			return err
		}
	}
	if err := c.flushLocked(); err != nil {
		return err
	}
	if handshakeTimeout > 0 {
		return raw.SetWriteDeadline(time.Time{})
	}
	return nil
}

// Read refills the library's buffer. The library asks only once it has handled
// everything it already read, so every reply owed so far leaves first, in one
// write, and then the reader waits for the peer.
func (c *coalescingConn) Read(p []byte) (int, error) {
	if c.src == nil {
		return 0, errNotAttached
	}
	c.batch.Store(false)
	c.mu.Lock()
	err := c.flushLocked()
	c.mu.Unlock()
	if err != nil {
		return 0, err
	}
	c.parked.Store(true)
	n, err := c.src.Read(p)
	c.parked.Store(false)
	if n > 0 {
		c.batch.Store(true)
	}
	return n, err
}

// Write holds a reply back while the reader still has input to handle and
// writes it through otherwise. Before attach only the handshake response comes
// through, and attach sends it.
func (c *coalescingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return 0, c.err
	}
	if c.closed {
		return 0, net.ErrClosed
	}
	if c.raw == nil || (c.batch.Load() && !c.parked.Load() && len(c.pending)+len(p) <= coalesceLimit) {
		c.pending = append(c.pending, p...)
		if c.raw != nil && !c.armed {
			c.arm()
		}
		return len(p), nil
	}
	if err := c.flushLocked(); err != nil {
		return 0, err
	}
	n, err := c.raw.Write(p)
	if err != nil {
		c.err = err
	}
	return n, err
}

// arm starts the safety timer for the bytes just deferred. It is never
// stopped early: expiring with nothing pending is a no-op, and not touching it
// on every flush keeps the per-reply cost to a copy. Caller holds mu.
func (c *coalescingConn) arm() {
	if c.timer == nil {
		c.timer = time.AfterFunc(c.maxDelay, c.onTimer)
	} else {
		c.timer.Reset(c.maxDelay)
	}
	c.armed = true
}

// onTimer sends whatever a reader that did not come back left pending, and
// stops deferring until the next fill proves the reader is still consuming.
func (c *coalescingConn) onTimer() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = false
	if len(c.pending) == 0 {
		return
	}
	c.batch.Store(false)
	_ = c.flushLocked()
}

// flushLocked writes everything pending in one syscall. Caller holds mu.
func (c *coalescingConn) flushLocked() error {
	if len(c.pending) == 0 {
		return nil
	}
	_, err := c.raw.Write(c.pending)
	if cap(c.pending) > coalesceRetain {
		c.pending = nil
	} else {
		c.pending = c.pending[:0]
	}
	if err != nil {
		c.err = err
	}
	return err
}

// Close sends what is pending and closes the hijacked connection. A second
// Close is a no-op: fasthttp recycles its hijacked connection on the first.
func (c *coalescingConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	if c.raw != nil {
		_ = c.flushLocked()
	}
	if c.armed {
		c.timer.Stop()
		c.armed = false
	}
	c.pending = nil
	src := c.src
	c.mu.Unlock()
	if src == nil {
		return nil
	}
	return src.Close()
}

func (c *coalescingConn) LocalAddr() net.Addr {
	if c.raw == nil {
		return nil
	}
	return c.raw.LocalAddr()
}

func (c *coalescingConn) RemoteAddr() net.Addr {
	if c.raw == nil {
		return nil
	}
	return c.raw.RemoteAddr()
}

// The deadline setters are no-ops before attach: fasthttp clears every
// deadline before it hands the socket over, and attach applies the handshake
// timeout itself.

func (c *coalescingConn) SetDeadline(t time.Time) error {
	if c.raw == nil {
		return nil
	}
	return c.raw.SetDeadline(t)
}

func (c *coalescingConn) SetReadDeadline(t time.Time) error {
	if c.raw == nil {
		return nil
	}
	return c.raw.SetReadDeadline(t)
}

func (c *coalescingConn) SetWriteDeadline(t time.Time) error {
	if c.raw == nil {
		return nil
	}
	return c.raw.SetWriteDeadline(t)
}

// upgradeResponseWriter is the http.ResponseWriter handed to the library's
// net/http Upgrader. It never writes a response: a rejected handshake records
// its status for Fiber to answer, an accepted one is hijacked into conn.
type upgradeResponseWriter struct {
	conn   *coalescingConn
	header http.Header
	status int
}

func (w *upgradeResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *upgradeResponseWriter) Write([]byte) (int, error) {
	return 0, http.ErrHijacked
}

func (w *upgradeResponseWriter) WriteHeader(int) {}

// Hijack hands the library the coalescing connection. The bufio pair is sized
// below what the library would reuse, so it allocates its own buffers from
// Config, exactly as the fasthttp upgrader does.
func (w *upgradeResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	rw := bufio.NewReadWriter(bufio.NewReaderSize(w.conn, hijackBufferSize), bufio.NewWriterSize(w.conn, hijackBufferSize))
	return w.conn, rw, nil
}

// upgradeRequest builds the http.Request the library validates: the method and
// the handshake headers. Values are copied; the subprotocol the library selects
// lives on the connection for as long as it does.
func upgradeRequest(fctx *fasthttp.RequestCtx) *http.Request {
	h := &fctx.Request.Header
	header := make(http.Header, len(handshakeHeaders))
	for _, name := range handshakeHeaders {
		if v := h.Peek(name); len(v) > 0 {
			header[name] = []string{string(v)}
		}
	}
	method := http.MethodGet
	if !fctx.IsGet() {
		method = string(fctx.Method())
	}
	return &http.Request{Method: method, Header: header}
}

// upgradeResponseHeader carries what earlier middleware set on the response, a
// cookie or a CORS header, into the 101 the library writes. fasthttp's own
// defaults and the handshake headers the library sets itself are left out.
//
// The subprotocol is negotiated here rather than by the library: its net/http
// upgrader prefers the client's order, the fasthttp one and this middleware's
// documentation the server's. The library takes the choice from the header,
// which is how it treats a subprotocol chosen by the application.
func upgradeResponseHeader(fctx *fasthttp.RequestCtx, subprotocols []string) http.Header {
	var header http.Header
	for key, value := range fctx.Response.Header.All() {
		switch utils.UnsafeString(key) {
		case fiber.HeaderContentType, fiber.HeaderContentLength, fiber.HeaderServer, fiber.HeaderDate,
			fiber.HeaderConnection, fiber.HeaderUpgrade, fiber.HeaderSecWebSocketAccept:
			continue
		}
		if header == nil {
			header = http.Header{}
		}
		name := http.CanonicalHeaderKey(string(key))
		header[name] = append(header[name], string(value))
	}
	if subprotocols != nil {
		protocol := http.CanonicalHeaderKey(fiber.HeaderSecWebSocketProtocol)
		delete(header, protocol)
		if chosen := selectSubprotocol(fctx.Request.Header.Peek(fiber.HeaderSecWebSocketProtocol), subprotocols); chosen != "" {
			if header == nil {
				header = http.Header{}
			}
			header[protocol] = []string{chosen}
		}
	}
	return header
}

// selectSubprotocol returns the first server subprotocol the client offered,
// or "" when none matches (RFC 6455 section 4.2.2).
func selectSubprotocol(offered []byte, subprotocols []string) string {
	for _, serverProtocol := range subprotocols {
		for clientProtocol := range strings.SplitSeq(utils.UnsafeString(offered), ",") {
			if strings.TrimSpace(clientProtocol) == serverProtocol {
				return serverProtocol
			}
		}
	}
	return ""
}

// newCoalescingUpgrader configures the library's net/http Upgrader from cfg.
// HandshakeTimeout is not passed on: the 101 is written once fasthttp hands
// the socket over, and attach applies the timeout to that write.
func newCoalescingUpgrader(cfg *Config, originAllowed func(origin string) bool) websocket.Upgrader {
	return websocket.Upgrader{
		// Subprotocols stay nil: upgradeResponseHeader negotiates them.
		ReadBufferSize:    cfg.ReadBufferSize,
		WriteBufferSize:   cfg.WriteBufferSize,
		WriteBufferPool:   cfg.WriteBufferPool,
		EnableCompression: cfg.EnableCompression,
		Error: func(w http.ResponseWriter, _ *http.Request, status int, _ error) {
			// The net/http upgrader answers a Connection header without its
			// Upgrade counterpart with 426; keep the 400 documented for a partial
			// handshake, which is also what the fasthttp upgrader sends.
			if status == fiber.StatusUpgradeRequired {
				status = fiber.StatusBadRequest
			}
			if uw, ok := w.(*upgradeResponseWriter); ok {
				uw.status = status
			}
		},
		CheckOrigin: func(r *http.Request) bool {
			return originAllowed(r.Header.Get(fiber.HeaderOrigin)) // Get canonicalizes the key
		},
	}
}

// upgradeCoalescing performs the handshake through the library's net/http
// Upgrader and hijacks the fasthttp connection into a coalescingConn.
func upgradeCoalescing(c fiber.Ctx, upgrader *websocket.Upgrader, conn *Conn, cfg *Config, handler func(*Conn)) error {
	fctx := c.RequestCtx()
	cc := newCoalescingConn()
	w := &upgradeResponseWriter{conn: cc}
	fconn, err := upgrader.Upgrade(w, upgradeRequest(fctx), upgradeResponseHeader(fctx, cfg.Subprotocols))
	if err != nil {
		if w.status == 0 {
			w.status = fiber.StatusInternalServerError
		}
		fctx.SetStatusCode(w.status)
		return rejectHandshake(c, w.status)
	}
	// The library has already written the 101 into cc, where attach sends it;
	// fasthttp must not add a response of its own.
	fctx.HijackSetNoResponse(true)
	fctx.Hijack(func(netConn net.Conn) {
		if err := cc.attach(netConn, cfg.HandshakeTimeout); err != nil {
			_ = cc.Close()
			return
		}
		runHandler(conn, fconn, cfg.RecoverHandler, handler)
	})
	return nil
}
