package socketio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
)

// EnablePolling toggles HTTP long-polling fallback support. When true, the
// handler returned from New() also accepts Engine.IO v4 polling requests
// (transport=polling) on the same route, in addition to the existing
// WebSocket upgrade. Default false preserves prior behavior.
//
// Read once per request; mutate before serving connections.
//
// To enable polling on the route, the user must mount the handler for both
// GET and POST methods, e.g.
//
//	h := socketio.New(cb)
//	app.Get("/socket.io/", h)
//	app.Post("/socket.io/", h)
//
// or app.All("/socket.io/", h).
//
// Polling sessions advertise an empty "upgrades" array in the Engine.IO
// OPEN packet: clients connected via polling stay on polling for the
// session lifetime. (Polling-to-WebSocket transport upgrade is not yet
// implemented; see issue tracker.)
var EnablePolling = false

// PollingMaxBufferSize bounds the size of a single polling HTTP body
// (POST request body and GET long-poll response body) in bytes. Matches
// engine.io's maxHttpBufferSize default of 1 MB.
var PollingMaxBufferSize = 1_000_000

// MaxPollWait caps how long a long-poll GET blocks waiting for outbound
// frames. The pong heartbeat (PingInterval, default 25s) typically
// enqueues a "2" PING packet within MaxPollWait, so most polls return
// well before this deadline. Set to zero to disable (not recommended).
var MaxPollWait = 30 * time.Second

// pollQueue is the per-session outbound buffer for HTTP long-polling.
// Frames enqueued by any write-path goroutine (Emit, pong heartbeat, late
// SIO CONNECT ack, Close) are drained by the long-poll GET handler in
// batches separated by ASCII RS (0x1E).
//
// Lock ordering: pollQueue.mu is a leaf - never held across user code or
// other locks. enqueue and close are O(1) tail operations; drain copies
// the buffered frame slice and resets the notify channel.
type pollQueue struct {
	mu     sync.Mutex
	frames [][]byte
	closed bool
	// notify is closed by the next signalLocked call after a successful
	// enqueue or close, waking every blocked drain. It is recreated by
	// drain after consuming frames so the next enqueue starts a fresh
	// signal cycle.
	notify chan struct{}
}

func newPollQueue() *pollQueue {
	return &pollQueue{notify: make(chan struct{})}
}

// signalLocked closes the current notify channel exactly once per cycle.
// Caller must hold q.mu.
func (q *pollQueue) signalLocked() {
	select {
	case <-q.notify:
		// already signalled this cycle
	default:
		close(q.notify)
	}
}

// enqueue appends a frame to the buffer. Caller transfers ownership of
// frame; callers must not mutate it after the call. A no-op when the
// queue is already closed (drops silently rather than panicking).
func (q *pollQueue) enqueue(frame []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.frames = append(q.frames, frame)
	q.signalLocked()
}

// close releases any blocked drain and marks the queue terminal: future
// enqueue calls become no-ops, and a drain that finds an empty buffer
// returns the closed flag. Idempotent.
func (q *pollQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	q.signalLocked()
}

// drain blocks until at least one frame is available, ctx is done, or
// the queue is closed. Returns frames in FIFO order capped at maxBytes
// total (counting one separator byte between consecutive frames); a
// frame larger than maxBytes is emitted alone rather than blocking
// forever. The closed flag indicates the session has terminated.
//
// On ctx expiry returns (nil, false). On close with empty buffer returns
// (nil, true).
func (q *pollQueue) drain(ctx context.Context, maxBytes int) ([][]byte, bool) {
	for {
		q.mu.Lock()
		if len(q.frames) > 0 {
			var taken [][]byte
			var size int
			i := 0
			for ; i < len(q.frames); i++ {
				f := q.frames[i]
				add := len(f)
				if i > 0 {
					add++
				}
				if maxBytes > 0 && size+add > maxBytes && len(taken) > 0 {
					break
				}
				taken = append(taken, f)
				size += add
			}
			if i >= len(q.frames) {
				q.frames = nil
			} else {
				rem := q.frames[i:]
				q.frames = make([][]byte, len(rem))
				copy(q.frames, rem)
			}
			// If the buffer is empty, recycle the notify channel so a
			// future enqueue starts a fresh signal cycle. If frames
			// remain, leave notify closed so the next drain wakes
			// immediately.
			if len(q.frames) == 0 {
				q.notify = make(chan struct{})
			}
			closed := q.closed
			q.mu.Unlock()
			return taken, closed
		}
		if q.closed {
			q.mu.Unlock()
			return nil, true
		}
		notify := q.notify
		q.mu.Unlock()

		select {
		case <-notify:
			// fall through to re-check buffer or closed flag
		case <-ctx.Done():
			return nil, false
		}
	}
}

// pollingErrorBody returns the engine.io error body for the given numeric
// code per https://github.com/socketio/engine.io-protocol. Codes 0/1/2/3/5
// map to HTTP 400; code 4 (Forbidden) maps to HTTP 403.
func pollingErrorBody(code int) (status int, body string) {
	var msg string
	switch code {
	case 0:
		msg = "Transport unknown"
	case 1:
		msg = "Session ID unknown"
	case 2:
		msg = "Bad handshake method"
	case 3:
		msg = "Bad request"
	case 4:
		return http.StatusForbidden, fmt.Sprintf(`{"code":%d,"message":%q}`, code, "Forbidden")
	case 5:
		msg = "Unsupported protocol version"
	default:
		msg = "Unknown error"
	}
	return http.StatusBadRequest, fmt.Sprintf(`{"code":%d,"message":%q}`, code, msg)
}

func writePollingError(c fiber.Ctx, code int) error {
	status, body := pollingErrorBody(code)
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.Status(status).SendString(body)
}

// handlePolling routes an inbound polling request. Returns true when the
// request was handled (response sent), false when the dispatcher should
// fall through to the WebSocket upgrade handler.
//
// Engine.IO error codes:
//
//	0 Transport unknown      - reserved (transport-validated upstream)
//	1 Session ID unknown     - sid query has no matching session
//	2 Bad handshake method   - new session opened with non-GET
//	3 Bad request            - JSONP, transport mismatch, malformed
//	4 Forbidden              - reserved (authorisation denied)
//	5 Unsupported protocol   - EIO != 4 (validated upstream)
func handlePolling(c fiber.Ctx, callback func(kws *Websocket)) (handled bool, err error) {
	transport := c.Query("transport")
	if transport != "polling" {
		return false, nil
	}
	if c.Query("j") != "" {
		// JSONP polling fallback is not implemented. Reject cleanly so
		// modern clients fall back to XHR2 (or fail noisily).
		return true, writePollingError(c, 3)
	}

	sid := c.Query("sid")
	method := c.Method()

	if sid == "" {
		// New polling session.
		if method != fiber.MethodGet {
			return true, writePollingError(c, 2)
		}
		return true, openPollingSession(c, callback)
	}

	// Existing session lookup.
	kwsAny, lookupErr := pool.get(sid)
	if lookupErr != nil {
		return true, writePollingError(c, 1)
	}
	kws, ok := kwsAny.(*Websocket)
	if !ok || kws.pollQ == nil {
		// sid resolves to a non-polling session: transport mismatch.
		return true, writePollingError(c, 3)
	}

	switch method {
	case fiber.MethodGet:
		return true, drainPolling(c, kws)
	case fiber.MethodPost:
		return true, ingestPolling(c, kws)
	case fiber.MethodOptions:
		// CORS preflight: respond 204 with no body. CORS allow headers
		// are intentionally NOT set here; mount the user's preferred
		// CORS middleware (e.g. github.com/gofiber/fiber/v3/middleware/cors)
		// upstream of the polling route to control the policy.
		c.Set(fiber.HeaderCacheControl, "no-store")
		return true, c.SendStatus(http.StatusNoContent)
	default:
		return true, writePollingError(c, 2)
	}
}

// openPollingSession creates a fresh polling session, runs the user
// callback, and writes the Engine.IO OPEN packet as the response body.
func openPollingSession(c fiber.Ctx, callback func(kws *Websocket)) error {
	kws := &Websocket{
		queue: make(chan message, SendQueueSize),
		done:  make(chan struct{}, 1),
		pollQ: newPollQueue(),
	}
	kws.UUID = kws.createUUID()
	kws.isAlive.Store(true)
	kws.lastPongNanos.Store(time.Now().UnixNano())

	// Snapshot per-request state into immutable lookup maps. fasthttp
	// recycles its RequestCtx after the handler returns, so capturing
	// live closures over c would race the pool's reuse on subsequent
	// reads from POST/drain handler goroutines. We therefore copy
	// Locals, Params, Query and Cookies from the OPEN request once;
	// listener callbacks on later transports see those frozen values.
	// Users needing per-connection mutable state should use
	// SetAttribute, which is transport-agnostic.
	queriesSnap := make(map[string]string)
	for k, v := range c.Queries() {
		queriesSnap[k] = v
	}
	paramsSnap := make(map[string]string)
	if route := c.Route(); route != nil {
		for _, k := range route.Params {
			paramsSnap[k] = c.Params(k)
		}
	}
	cookiesSnap := make(map[string]string)
	c.RequestCtx().Request.Header.VisitAllCookie(func(k, v []byte) {
		cookiesSnap[string(k)] = string(v)
	})
	localsSnap := make(map[string]interface{})
	c.RequestCtx().VisitUserValues(func(k []byte, v interface{}) {
		localsSnap[string(k)] = v
	})

	kws.Locals = func(key string) interface{} { return localsSnap[key] }
	kws.Params = func(key string, def ...string) string {
		if v, ok := paramsSnap[key]; ok {
			return v
		}
		if len(def) > 0 {
			return def[0]
		}
		return ""
	}
	kws.Query = func(key string, def ...string) string {
		if v, ok := queriesSnap[key]; ok {
			return v
		}
		if len(def) > 0 {
			return def[0]
		}
		return ""
	}
	kws.Cookies = func(key string, def ...string) string {
		if v, ok := cookiesSnap[key]; ok {
			return v
		}
		if len(def) > 0 {
			return def[0]
		}
		return ""
	}

	pool.set(kws)

	maxPayload := int(MaxPayload)
	if maxPayload <= 0 {
		maxPayload = 1_000_000
	}
	open := eioOpenPacket{
		SID:          kws.UUID,
		Upgrades:     []string{},
		PingInterval: int(PingInterval.Milliseconds()),
		PingTimeout:  int(PingTimeout.Milliseconds()),
		MaxPayload:   maxPayload,
	}
	data, err := json.Marshal(open)
	if err != nil {
		kws.disconnected(err)
		return writePollingError(c, 3)
	}

	// Heartbeat goroutine. Polling has no send goroutine (the long-poll
	// GET handler is the sender) and no read goroutine (POST handlers
	// are the readers). pong() emits PINGs via kws.write, which routes
	// to pollQ.enqueue for polling sessions.
	ctx, cancel := context.WithCancel(context.Background())
	kws.ctx = ctx
	kws.cancelCtx = cancel
	kws.workersWg.Add(1)
	go func() { defer kws.workersWg.Done(); kws.pong(ctx) }()
	// Lifecycle goroutine: blocks until disconnected fires, then runs
	// finishRun to cancel the ctx and join the heartbeat goroutine. This
	// substitutes for the run() loop used by the WebSocket path.
	go kws.pollLifecycle()

	callback(kws)

	if !kws.IsAlive() {
		// Callback rejected the session (typically by calling
		// kws.Close() inside the callback). Close() already invokes
		// disconnected() which closes the queue, removes the session
		// from the pool, and unblocks pollLifecycle so finishRun runs.
		// disconnected() is idempotent via kws.once, so calling it
		// here defensively is safe and ensures cleanup also happens
		// when callbacks reach the !IsAlive state through some future
		// non-Close() path.
		kws.disconnected(nil)
		return writePollingError(c, 4)
	}

	body := append([]byte{eioOpen}, data...)
	c.Set(fiber.HeaderContentType, "text/plain; charset=UTF-8")
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.Send(body)
}

// pollLifecycle blocks until the session is torn down, then runs the
// shared finishRun barrier so workersWg.Wait observers see the goroutines
// drained.
func (kws *Websocket) pollLifecycle() {
	<-kws.done
	kws.finishRun()
}

// drainPolling is the GET long-poll handler: blocks until queued frames
// arrive, the session is closed, or the wait deadline elapses. Concurrent
// GETs on the same sid are rejected with engine.io error code 3 (Bad
// request) to match the reference server's single-poller invariant.
func drainPolling(c fiber.Ctx, kws *Websocket) error {
	if !kws.pollGate.CompareAndSwap(false, true) {
		return writePollingError(c, 3)
	}
	defer kws.pollGate.Store(false)

	drainCtx := c.Context()
	if MaxPollWait > 0 {
		var cancel context.CancelFunc
		drainCtx, cancel = context.WithTimeout(drainCtx, MaxPollWait)
		defer cancel()
	}

	frames, closed := kws.pollQ.drain(drainCtx, PollingMaxBufferSize)
	body := encodePollingFrames(frames)

	c.Set(fiber.HeaderContentType, "text/plain; charset=UTF-8")
	c.Set(fiber.HeaderCacheControl, "no-store")

	if len(body) == 0 {
		if closed {
			// Session terminated and nothing left to deliver. Match the
			// engine.io reference: report unknown sid so the client
			// gives up cleanly instead of polling a dead session.
			return writePollingError(c, 1)
		}
		// Long-poll deadline elapsed with no data. The engine.io
		// reference holds the request open until either a frame is
		// queued or the underlying TCP connection drops; it does NOT
		// inject a synthetic packet on idle expiry. With our default
		// MaxPollWait (30s) > PingInterval (25s), the heartbeat almost
		// always enqueues a "2" PING before this branch fires, so this
		// path is the rare-fallback. Return an empty 200 to align with
		// the reference.
		return c.Send(nil)
	}
	return c.Send(body)
}

// encodePollingFrames serialises frames into a single HTTP body using
// ASCII RS (0x1E) as separator, per Engine.IO v4 polling. Single-frame
// payloads bypass the separator. Frames are appended verbatim; binary
// payloads must already be encoded as "b<base64>" by the caller.
func encodePollingFrames(frames [][]byte) []byte {
	if len(frames) == 0 {
		return nil
	}
	if len(frames) == 1 {
		return frames[0]
	}
	size := 0
	for _, f := range frames {
		size += len(f) + 1
	}
	buf := make([]byte, 0, size)
	for i, f := range frames {
		if i > 0 {
			buf = append(buf, 0x1E)
		}
		buf = append(buf, f...)
	}
	return buf
}

// ingestPolling is the POST handler: parses the request body as a stream
// of RS-separated EIO packets, dispatches each through the same packet
// pipeline used by the WebSocket read loop, and responds with the
// engine.io literal "ok" body.
func ingestPolling(c fiber.Ctx, kws *Websocket) error {
	body := c.Body()
	if PollingMaxBufferSize > 0 && len(body) > PollingMaxBufferSize {
		return writePollingError(c, 3)
	}

	// Serialise POSTs per session so dispatched packets across
	// overlapping POSTs preserve FIFO order. Held only for the parse
	// loop; never across user-callback locks.
	kws.postGate.Lock()
	defer kws.postGate.Unlock()

	// Any inbound HTTP body counts as proof of life for the heartbeat
	// enforcer, mirroring the WebSocket read-loop behaviour at
	// socketio.go:1960.
	kws.lastPongNanos.Store(time.Now().UnixNano())

	rest := body
	count := 0
	for len(rest) > 0 {
		if count > MaxBatchPackets {
			kws.fireEvent(EventError, nil, ErrBatchPacketsExceeded)
			break
		}
		idx := bytes.IndexByte(rest, 0x1E)
		var packet []byte
		if idx < 0 {
			packet, rest = rest, nil
		} else {
			packet, rest = rest[:idx], rest[idx+1:]
		}
		if len(packet) == 0 {
			continue
		}
		// Binary packet encoding: "b" prefix + base64 payload. Decode
		// inline and surface as an EventMessage with the raw bytes,
		// matching the WebSocket BinaryMessage path in read().
		if packet[0] == 'b' {
			decoded := make([]byte, base64.StdEncoding.DecodedLen(len(packet)-1))
			n, decErr := base64.StdEncoding.Decode(decoded, packet[1:])
			if decErr != nil {
				kws.fireEvent(EventError, packet, decErr)
				continue
			}
			kws.fireEvent(EventMessage, decoded[:n], nil)
			count++
			continue
		}
		kws.dispatchEIOPacket(packet)
		count++
	}

	c.Set(fiber.HeaderContentType, "text/html")
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.Status(http.StatusOK).SendString("ok")
}

// connectFiredCAS atomically marks the session as having delivered its
// EventConnect notification. Returns true on the first successful flip,
// false on subsequent calls. Used to gate the polling first-CONNECT
// branch in handleSIOPacket so the EventConnect listener fires exactly
// once per session regardless of which transport delivered the SIO
// CONNECT packet.
func (kws *Websocket) connectFiredCAS() bool {
	return kws.connectFired.CompareAndSwap(false, true)
}
