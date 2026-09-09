package websocket

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
)

// The handshake runs through the library's net/http Upgrader on a
// fasthttp-backed Hijacker, so the middleware owns the net.Conn the library
// writes to and can answer a burst of pipelined frames with one write.
// fasthttp still sends the 101: the headers the library composed are mirrored
// onto the response before the hijack, where later middleware sees them.

const (
	coalesceLimit    = 64 << 10               // a write that would push pending past this goes out
	coalesceMaxDelay = time.Millisecond       // flush if the reader has not returned by then
	closeFlushGrace  = 100 * time.Millisecond // how long Close waits to send what is pending
	// minCoalesceFill is the smallest fill that can hold two client frames, each
	// at least 6 bytes of header and mask; a smaller one has nothing to
	// coalesce a reply with, so the reply goes straight out.
	minCoalesceFill = 12
)

var handshakeHeaders = [...]string{
	http.CanonicalHeaderKey(fiber.HeaderConnection),
	http.CanonicalHeaderKey(fiber.HeaderUpgrade),
	http.CanonicalHeaderKey(fiber.HeaderSecWebSocketVersion),
	http.CanonicalHeaderKey(fiber.HeaderSecWebSocketKey),
	http.CanonicalHeaderKey(fiber.HeaderSecWebSocketExtensions),
	http.CanonicalHeaderKey(fiber.HeaderSecWebSocketProtocol),
	http.CanonicalHeaderKey(fiber.HeaderOrigin),
}

// hijackReadWriter is handed to the library on hijack. It only inspects the
// pair and, at these sizes, allocates its own buffers from Config.
var hijackReadWriter = bufio.NewReadWriter(bufio.NewReaderSize(bytes.NewReader(nil), 16), bufio.NewWriterSize(io.Discard, 16))

var scratchPool = sync.Pool{New: func() any { b := make([]byte, 4096); return &b }}

// coalescingConn defers a write only while batch is set: the reader's last
// fill could hold more than one frame and it has not come back for more. Pending bytes
// leave before the reader blocks, when a write would not fit, on Close, or
// after coalesceMaxDelay.
//
// mu guards pending and the flags; wmu orders socket writes and is never
// taken under mu, so Close and the reader never wait behind a stalled writer.
type coalescingConn struct {
	src   net.Conn  // fasthttp's hijacked conn; its Close is the one fasthttp expects
	raw   net.Conn  // the socket underneath, nil until attach
	rd    io.Reader // raw once fasthttp's buffer is in stash, else src
	stash []byte    // bytes fasthttp had read past the handshake

	batch atomic.Bool

	mu       sync.Mutex
	pending  []byte
	timer    *time.Timer
	maxDelay time.Duration
	armed    bool
	closed   bool
	err      error

	wmu sync.Mutex
}

func newCoalescingConn() *coalescingConn {
	return &coalescingConn{maxDelay: coalesceMaxDelay}
}

// takeHandshake returns what the library wrote during Upgrade, the 101 that
// fasthttp sends instead.
func (c *coalescingConn) takeHandshake() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pending
	c.pending = nil
	return p
}

// attach binds the hijacked conn. Whatever fasthttp buffered past the
// handshake is stashed so reads can go straight to the socket; with that
// buffer empty the expired deadline fails before any syscall. A conn that
// refuses the deadline would block the drain instead, so it keeps reading
// through fasthttp's buffer.
func (c *coalescingConn) attach(src net.Conn) {
	raw := src
	var rd io.Reader = src
	var stash []byte
	if u, ok := src.(interface{ UnsafeConn() net.Conn }); ok {
		raw = u.UnsafeConn()
		if src.SetReadDeadline(time.Unix(1, 0)) == nil {
			scratch := scratchPool.Get().(*[]byte)
			for {
				n, err := src.Read(*scratch)
				stash = append(stash, (*scratch)[:n]...)
				if err != nil {
					break
				}
			}
			scratchPool.Put(scratch)
			_ = raw.SetReadDeadline(time.Time{})
			rd = raw
		}
	}
	c.mu.Lock()
	c.src, c.raw, c.rd, c.stash = src, raw, rd, stash
	c.mu.Unlock()
}

func (c *coalescingConn) Read(p []byte) (int, error) {
	c.batch.Store(false)
	if err := c.flush(); err != nil {
		return 0, err
	}
	if len(c.stash) > 0 {
		n := copy(p, c.stash)
		c.stash = c.stash[n:]
		c.batch.Store(n >= minCoalesceFill)
		return n, nil
	}
	n, err := c.rd.Read(p)
	c.batch.Store(n >= minCoalesceFill)
	return n, err
}

func (c *coalescingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return 0, err
	}
	if c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	if c.raw == nil || (c.batch.Load() && len(c.pending)+len(p) <= coalesceLimit) {
		c.pending = append(c.pending, p...)
		if c.raw != nil && !c.armed {
			c.arm()
		}
		c.mu.Unlock()
		return len(p), nil
	}
	c.mu.Unlock()

	c.wmu.Lock()
	var err error
	if pending := c.take(); len(pending) > 0 {
		bufs := net.Buffers{pending, p} // one writev on a TCP socket
		_, err = bufs.WriteTo(c.raw)
		c.recycle(pending)
	} else {
		_, err = c.raw.Write(p)
	}
	if err != nil {
		c.fail(err)
		c.wmu.Unlock()
		return 0, err
	}
	_ = c.drainAndRelease()
	return len(p), nil
}

// flush sends what is pending unless a writer holds the socket, which then
// drains it before releasing.
func (c *coalescingConn) flush() error {
	c.mu.Lock()
	empty, err := len(c.pending) == 0, c.err
	c.mu.Unlock()
	if err != nil || empty {
		return err
	}
	if !c.wmu.TryLock() {
		return nil
	}
	return c.drainAndRelease()
}

// drainAndRelease writes pending until it is empty, then releases wmu. Caller
// holds wmu. The release happens under mu, after the last look at pending, so
// a write that lands later arms a timer that finds the socket free: a holder
// never leaves bytes behind that nothing would send.
func (c *coalescingConn) drainAndRelease() error {
	for {
		c.mu.Lock()
		if c.armed {
			c.timer.Stop()
			c.armed = false
		}
		if len(c.pending) == 0 || c.err != nil {
			err := c.err
			c.wmu.Unlock()
			c.mu.Unlock()
			return err
		}
		pending := c.pending
		c.pending = nil
		c.mu.Unlock()
		_, err := c.raw.Write(pending)
		c.recycle(pending)
		if err != nil {
			c.fail(err)
			c.wmu.Unlock()
			return err
		}
	}
}

// take removes the pending bytes, or returns nil when there are none or the
// connection has already failed.
func (c *coalescingConn) take() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.armed {
		c.timer.Stop()
		c.armed = false
	}
	if len(c.pending) == 0 || c.err != nil {
		return nil
	}
	p := c.pending
	c.pending = nil
	return p
}

// recycle keeps a written buffer for the next batch.
func (c *coalescingConn) recycle(p []byte) {
	c.mu.Lock()
	if c.pending == nil && cap(p) <= coalesceLimit {
		c.pending = p[:0]
	}
	c.mu.Unlock()
}

func (c *coalescingConn) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
}

// arm starts the safety timer. Caller holds mu.
func (c *coalescingConn) arm() {
	if c.timer == nil {
		c.timer = time.AfterFunc(c.maxDelay, c.onTimer)
	} else {
		c.timer.Reset(c.maxDelay)
	}
	c.armed = true
}

// onTimer sends what a reader that did not come back left pending, and stops
// deferring until the next fill proves the reader is still consuming. A
// writer that holds the socket sends it instead, before letting go.
func (c *coalescingConn) onTimer() {
	c.mu.Lock()
	c.armed = false
	empty := len(c.pending) == 0
	if !empty {
		c.batch.Store(false)
	}
	c.mu.Unlock()
	if empty || !c.wmu.TryLock() {
		return
	}
	_ = c.drainAndRelease()
}

// Close sends what is pending, within closeFlushGrace, and closes the socket.
// A writer stalled on a full window is expired first so it cannot hold the
// close up. A conn that refuses deadlines is closed under such a writer
// instead, and pending bytes are dropped rather than sent without a bound on
// the wait. A second Close is a no-op.
func (c *coalescingConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	src, raw := c.src, c.raw
	c.mu.Unlock()
	if raw == nil {
		return nil
	}
	if !c.wmu.TryLock() {
		if raw.SetWriteDeadline(time.Unix(1, 0)) != nil {
			_ = raw.Close()
		}
		c.wmu.Lock()
	}
	if raw.SetWriteDeadline(time.Now().Add(closeFlushGrace)) == nil {
		_ = c.drainAndRelease()
	} else {
		c.wmu.Unlock()
	}
	return src.Close()
}

// UnsafeConn returns the socket underneath, as fasthttp's hijacked conn does.
func (c *coalescingConn) UnsafeConn() net.Conn { return c.raw }

func (c *coalescingConn) LocalAddr() net.Addr  { return c.raw.LocalAddr() }
func (c *coalescingConn) RemoteAddr() net.Addr { return c.raw.RemoteAddr() }

// The deadline setters tolerate a nil raw: the library clears the deadlines
// during Upgrade, before fasthttp has handed the socket over.

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

// upgradeResponseWriter never writes a response: a rejection records its
// status for Fiber to answer, an accepted handshake is hijacked into conn.
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

func (w *upgradeResponseWriter) Write([]byte) (int, error) { return 0, http.ErrHijacked }
func (w *upgradeResponseWriter) WriteHeader(code int)      { w.status = code }

func (w *upgradeResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, hijackReadWriter, nil
}

func upgradeRequest(fctx *fasthttp.RequestCtx) *http.Request {
	h := &fctx.Request.Header
	header := make(http.Header, len(handshakeHeaders))
	for _, name := range handshakeHeaders {
		for _, v := range h.PeekAll(name) {
			header[name] = append(header[name], string(v))
		}
	}
	method := http.MethodGet
	if !fctx.IsGet() {
		method = string(fctx.Method())
	}
	return &http.Request{Method: method, Header: header}
}

// subprotocolHeader hands the library the negotiated subprotocol, chosen in
// the server's order of preference, which the library's own selection would
// not keep. With no Subprotocols configured, one set on the response by
// earlier middleware stands.
func subprotocolHeader(fctx *fasthttp.RequestCtx, req *http.Request, subprotocols []string) http.Header {
	var chosen string
	if subprotocols == nil {
		chosen = string(fctx.Response.Header.Peek(fiber.HeaderSecWebSocketProtocol))
	} else {
		offered := websocket.Subprotocols(req)
		for _, s := range subprotocols {
			if slices.Contains(offered, s) {
				chosen = s
				break
			}
		}
	}
	if chosen == "" {
		return nil
	}
	return http.Header{http.CanonicalHeaderKey(fiber.HeaderSecWebSocketProtocol): {chosen}}
}

func newUpgrader(cfg *Config, originAllowed func(origin string) bool) websocket.Upgrader {
	return websocket.Upgrader{
		ReadBufferSize:    cfg.ReadBufferSize,
		WriteBufferSize:   cfg.WriteBufferSize,
		WriteBufferPool:   cfg.WriteBufferPool,
		EnableCompression: cfg.EnableCompression,
		CheckOrigin: func(r *http.Request) bool {
			return originAllowed(r.Header.Get(fiber.HeaderOrigin))
		},
	}
}

func upgrade(c fiber.Ctx, upgrader *websocket.Upgrader, conn *Conn, cfg *Config, handler func(*Conn)) error {
	fctx := c.RequestCtx()
	if len(fctx.Response.Header.Peek(fiber.HeaderSecWebSocketExtensions)) > 0 {
		// Application extensions are unsupported, as on the fasthttp upgrader.
		fctx.SetStatusCode(fiber.StatusInternalServerError)
		return rejectHandshake(c, fiber.StatusInternalServerError)
	}
	cc := newCoalescingConn()
	w := &upgradeResponseWriter{conn: cc}
	req := upgradeRequest(fctx)
	fconn, err := upgrader.Upgrade(w, req, subprotocolHeader(fctx, req, cfg.Subprotocols))
	if err != nil {
		status := w.status
		switch status {
		case 0:
			status = fiber.StatusInternalServerError
		case fiber.StatusUpgradeRequired: // a Connection header without its Upgrade counterpart: 400, as documented
			status = fiber.StatusBadRequest
		}
		fctx.SetStatusCode(status)
		return rejectHandshake(c, status)
	}
	// The library wrote the 101 into cc. fasthttp sends the response once the
	// chain has unwound, so mirror those headers onto it.
	fctx.SetStatusCode(fiber.StatusSwitchingProtocols)
	for line := range bytes.SplitSeq(cc.takeHandshake(), []byte("\r\n")) {
		if key, value, ok := bytes.Cut(line, []byte(": ")); ok {
			fctx.Response.Header.SetBytesKV(key, value)
		}
	}
	if netConn := fctx.Conn(); netConn != nil && cfg.HandshakeTimeout > 0 {
		// Bounds fasthttp's write of the 101; fasthttp clears it before the
		// hijack, and replaces it with its own when the server has a WriteTimeout.
		_ = netConn.SetWriteDeadline(time.Now().Add(cfg.HandshakeTimeout))
	}
	fctx.Hijack(func(netConn net.Conn) {
		cc.attach(netConn)
		runHandler(conn, fconn, cfg.RecoverHandler, handler)
	})
	return nil
}
