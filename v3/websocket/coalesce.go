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

// The upgrade runs through the library's net/http Upgrader on a fasthttp-backed
// Hijacker, so the middleware owns the net.Conn the library writes to and can
// answer a burst of pipelined frames with one write.

const (
	coalesceLimit    = 64 << 10         // flush once this much is pending
	coalesceMaxDelay = time.Millisecond // flush if the reader has not returned by then
	coalesceRetain   = 16 << 10         // largest pending buffer kept between bursts
	hijackBufferSize = 16               // below the sizes the library would reuse
)

var errNotAttached = errors.New("websocket: connection not attached yet")

var handshakeHeaders = [...]string{
	http.CanonicalHeaderKey(fiber.HeaderConnection),
	http.CanonicalHeaderKey(fiber.HeaderUpgrade),
	http.CanonicalHeaderKey(fiber.HeaderSecWebSocketVersion),
	http.CanonicalHeaderKey(fiber.HeaderSecWebSocketKey),
	http.CanonicalHeaderKey(fiber.HeaderSecWebSocketExtensions),
	http.CanonicalHeaderKey(fiber.HeaderSecWebSocketProtocol),
	http.CanonicalHeaderKey(fiber.HeaderOrigin),
}

// coalescingConn defers a write only while batch && !parked: the reader was
// handed input by its last fill and has not come back for more. Pending bytes
// leave before a blocking read, at coalesceLimit, on Close, or after
// coalesceMaxDelay.
type coalescingConn struct {
	src net.Conn // fasthttp's hijacked conn: reads, and the Close fasthttp expects
	raw net.Conn // the socket: writes, deadlines, addresses; nil until attach

	batch  atomic.Bool
	parked atomic.Bool

	mu       sync.Mutex
	pending  []byte
	timer    *time.Timer
	maxDelay time.Duration
	armed    bool
	closed   bool
	err      error
}

func newCoalescingConn() *coalescingConn {
	return &coalescingConn{maxDelay: coalesceMaxDelay}
}

// attach binds the hijacked conn and sends the 101 the library wrote meanwhile.
func (c *coalescingConn) attach(src net.Conn, handshakeTimeout time.Duration) error {
	raw := src
	if u, ok := src.(interface{ UnsafeConn() net.Conn }); ok {
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

// arm starts the safety timer; it is left to expire rather than stopped per flush.
func (c *coalescingConn) arm() {
	if c.timer == nil {
		c.timer = time.AfterFunc(c.maxDelay, c.onTimer)
	} else {
		c.timer.Reset(c.maxDelay)
	}
	c.armed = true
}

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

// upgradeResponseWriter never writes: a rejection records its status for
// Fiber to answer, an accepted handshake is hijacked into conn.
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

func (w *upgradeResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	rw := bufio.NewReadWriter(bufio.NewReaderSize(w.conn, hijackBufferSize), bufio.NewWriterSize(w.conn, hijackBufferSize))
	return w.conn, rw, nil
}

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

// upgradeResponseHeader forwards what earlier middleware set and negotiates
// the subprotocol in server-preference order, which the net/http upgrader
// would not.
func upgradeResponseHeader(fctx *fasthttp.RequestCtx, subprotocols []string) http.Header {
	var header http.Header
	for key, value := range fctx.Response.Header.All() {
		switch utils.UnsafeString(key) {
		case fiber.HeaderContentType, fiber.HeaderContentLength, fiber.HeaderDate,
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

func newUpgrader(cfg *Config, originAllowed func(origin string) bool) websocket.Upgrader {
	return websocket.Upgrader{
		ReadBufferSize:    cfg.ReadBufferSize,
		WriteBufferSize:   cfg.WriteBufferSize,
		WriteBufferPool:   cfg.WriteBufferPool,
		EnableCompression: cfg.EnableCompression,
		Error: func(w http.ResponseWriter, _ *http.Request, status int, _ error) {
			if status == fiber.StatusUpgradeRequired { // partial handshake: 400, as documented
				status = fiber.StatusBadRequest
			}
			if uw, ok := w.(*upgradeResponseWriter); ok {
				uw.status = status
			}
		},
		CheckOrigin: func(r *http.Request) bool {
			return originAllowed(r.Header.Get(fiber.HeaderOrigin))
		},
	}
}

func upgrade(c fiber.Ctx, upgrader *websocket.Upgrader, conn *Conn, cfg *Config, handler func(*Conn)) error {
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
	fctx.HijackSetNoResponse(true) // the 101 is already in cc; attach sends it
	fctx.Hijack(func(netConn net.Conn) {
		if err := cc.attach(netConn, cfg.HandshakeTimeout); err != nil {
			_ = cc.Close()
			return
		}
		runHandler(conn, fconn, cfg.RecoverHandler, handler)
	})
	return nil
}
