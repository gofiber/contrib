package socketio

import (
	"bytes"
	"context"
	"encoding/base64"
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

// PollQueueMaxFrames caps the number of frames buffered in a polling
// session's outbound queue. When the cap is hit, behaviour mirrors the
// WebSocket SendQueueSize backpressure: with DropFramesOnOverflow=true
// the new frame is dropped and EventError fires with
// ErrSendQueueOverflow; otherwise the session is torn down with
// ErrSendQueueClosed. Without this cap, a slow or absent poller paired
// with a steady server-side emit stream would grow the queue without
// bound. Set to zero to disable the cap (not recommended).
var PollQueueMaxFrames = 1024

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

// enqueueResult is returned from enqueue to let the caller act on the
// queue overflow policy without exposing pollQueue internals.
type enqueueResult int

const (
	enqueueOK enqueueResult = iota
	enqueueDropped
	enqueueOverflow
)

// enqueue appends a frame to the buffer. Caller transfers ownership of
// frame; callers must not mutate it after the call.
//
// Returns:
//   - enqueueOK when the frame was buffered (or the queue was closed,
//     in which case the frame is silently dropped to match the
//     post-disconnect "best effort" semantics);
//   - enqueueDropped when the frame would exceed PollQueueMaxFrames AND
//     DropFramesOnOverflow is true (caller fires EventError);
//   - enqueueOverflow when the frame would exceed PollQueueMaxFrames
//     and DropFramesOnOverflow is false (caller tears the session
//     down).
func (q *pollQueue) enqueue(frame []byte) enqueueResult {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return enqueueOK
	}
	if PollQueueMaxFrames > 0 && len(q.frames) >= PollQueueMaxFrames {
		if DropFramesOnOverflow {
			return enqueueDropped
		}
		return enqueueOverflow
	}
	q.frames = append(q.frames, frame)
	q.signalLocked()
	return enqueueOK
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
			taken := make([][]byte, 0, len(q.frames))
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
			// Truncate in place to retain the backing array across
			// drain cycles. nil out the slots we are dropping so the
			// frame []byte references can be GC'd.
			if i >= len(q.frames) {
				for j := range q.frames {
					q.frames[j] = nil
				}
				q.frames = q.frames[:0]
			} else {
				n := copy(q.frames, q.frames[i:])
				for j := n; j < len(q.frames); j++ {
					q.frames[j] = nil
				}
				q.frames = q.frames[:n]
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
	queries := c.Queries()
	queriesSnap := make(map[string]string, len(queries))
	for k, v := range queries {
		queriesSnap[k] = v
	}
	var paramsSnap map[string]string
	if route := c.Route(); route != nil && len(route.Params) > 0 {
		paramsSnap = make(map[string]string, len(route.Params))
		for _, k := range route.Params {
			paramsSnap[k] = c.Params(k)
		}
	}
	// Locals and Cookies are usually empty for socket.io routes; lazy-
	// init keeps the OPEN allocation budget low for the common case.
	var cookiesSnap map[string]string
	c.RequestCtx().Request.Header.VisitAllCookie(func(k, v []byte) {
		if cookiesSnap == nil {
			cookiesSnap = make(map[string]string)
		}
		cookiesSnap[string(k)] = string(v)
	})
	var localsSnap map[string]interface{}
	c.RequestCtx().VisitUserValues(func(k []byte, v interface{}) {
		if localsSnap == nil {
			localsSnap = make(map[string]interface{})
		}
		localsSnap[string(k)] = v
	})

	mkLookup := func(m map[string]string) func(string, ...string) string {
		return func(key string, def ...string) string {
			if v, ok := m[key]; ok {
				return v
			}
			if len(def) > 0 {
				return def[0]
			}
			return ""
		}
	}
	kws.Locals = func(key string) interface{} { return localsSnap[key] }
	kws.Params = mkLookup(paramsSnap)
	kws.Query = mkLookup(queriesSnap)
	kws.Cookies = mkLookup(cookiesSnap)

	pool.set(kws)

	frame, err := buildEIOOpenFrame(kws.UUID)
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
	go func() { <-kws.done; kws.finishRun() }()

	// Handshake budget: enforce parity with the WebSocket path
	// (handshake() uses HandshakeTimeout via SetReadDeadline). Without
	// this, a polling client could open a session, never POST "40", and
	// only the heartbeat enforcer (PingInterval+PingTimeout, default
	// 45s) would tear it down. The timer is a no-op once the SIO
	// CONNECT path flips connectFired. We retain the *time.Timer on
	// the session and Stop it in disconnected() so a graceful close
	// does not pin the *Websocket for the full HandshakeTimeout
	// budget on the runtime timer heap.
	if HandshakeTimeout > 0 {
		kws.handshakeTimer.Store(time.AfterFunc(HandshakeTimeout, func() {
			if !kws.connectFired.Load() {
				kws.disconnected(ErrHandshakeClosed)
			}
		}))
	}

	// Recover panics from the user callback. Without this, a panicking
	// callback unwinds out of openPollingSession with the lifecycle
	// goroutine still parked on <-kws.done; disconnected() never runs,
	// so the session leaks (pool entry, pong goroutine, *Websocket
	// graph). On panic we log, surface via EventError, run
	// disconnected to clean up the session, and respond with HTTP 500
	// without re-raising so Fiber's worker pool stays healthy.
	var callbackPanic interface{}
	func() {
		defer func() {
			if r := recover(); r != nil {
				callbackPanic = r
			}
		}()
		callback(kws)
	}()
	if callbackPanic != nil {
		logf("error", "polling_callback_panic", "uuid", kws.UUID, "panic", fmt.Sprintf("%v", callbackPanic))
		kws.disconnected(fmt.Errorf("socketio: polling callback panic: %v", callbackPanic))
		c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
		return c.Status(http.StatusInternalServerError).
			SendString(`{"code":3,"message":"Bad request"}`)
	}

	if !kws.IsAlive() {
		// Callback rejected the session (typically by calling
		// kws.Close() inside the callback). Close() already invokes
		// disconnected() which closes the queue, removes the session
		// from the pool, and wakes the lifecycle goroutine so
		// finishRun runs. disconnected() is idempotent via kws.once,
		// so calling it here defensively is safe and ensures cleanup
		// also happens when callbacks reach the !IsAlive state
		// through some future non-Close() path.
		kws.disconnected(nil)
		return writePollingError(c, 4)
	}

	c.Set(fiber.HeaderContentType, "text/plain; charset=UTF-8")
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.Send(frame)
}

// drainPolling is the GET long-poll handler: blocks until queued frames
// arrive, the session is closed, or the wait deadline elapses. Concurrent
// GETs on the same sid are rejected with engine.io error code 3 (Bad
// request) to match the reference server's single-poller invariant.
func drainPolling(c fiber.Ctx, kws *Websocket) error {
	if !kws.pollGate.CompareAndSwap(false, true) {
		return writePollingError(c, 3)
	}

	drainCtx := c.Context()
	if MaxPollWait > 0 {
		var cancel context.CancelFunc
		drainCtx, cancel = context.WithTimeout(drainCtx, MaxPollWait)
		defer cancel()
	}

	frames, closed := kws.pollQ.drain(drainCtx, PollingMaxBufferSize)

	// Release the gate before writing the response body. Holding it
	// across c.Send would strand a slow-client session for the duration
	// of TCP backpressure on the response write, blocking subsequent
	// legitimate polls with concurrent-GET errors. The drain has already
	// taken its share of the queue, so a competing GET racing in here
	// either drains new frames or blocks on its own context.
	kws.pollGate.Store(false)

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

// encodePollingBinary returns the Engine.IO v4 polling text encoding of
// a binary payload: a single 'b' byte followed by the standard-base64
// representation of data. Used by the polling write path to wrap raw
// bytes that user code intends to send as a BinaryMessage; the
// WebSocket transport uses a binary frame instead.
func encodePollingBinary(data []byte) []byte {
	enc := base64.StdEncoding
	out := make([]byte, 1+enc.EncodedLen(len(data)))
	out[0] = 'b'
	enc.Encode(out[1:], data)
	return out
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
		// Tear the session down: matching the WebSocket SetReadLimit
		// behaviour, an oversized inbound payload is a protocol-level
		// error, not just a single-request rejection. Without this an
		// adversary could repeatedly POST oversized bodies to keep
		// sessions alive in the pool until heartbeat reaps them.
		kws.fireEvent(EventError, nil, ErrPollingBodyTooLarge)
		kws.disconnected(ErrPollingBodyTooLarge)
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
		// matching the WebSocket BinaryMessage path in read(). The
		// decoded slice owns its memory; safe to surface to listeners
		// even after fasthttp recycles the request body.
		if packet[0] == 'b' {
			decoded := make([]byte, base64.StdEncoding.DecodedLen(len(packet)-1))
			n, decErr := base64.StdEncoding.Decode(decoded, packet[1:])
			if decErr != nil {
				kws.fireEvent(EventError, append([]byte(nil), packet...), decErr)
				continue
			}
			kws.fireEvent(EventMessage, decoded[:n], nil)
			count++
			continue
		}
		// Copy the packet off c.Body() before dispatch: fasthttp
		// reuses the request body buffer once the handler returns,
		// and listener callbacks may stash EventPayload.Data /
		// .Args into a goroutine outliving the request. The
		// WebSocket read path already copies binary frames for the
		// same reason; do the same here for parity.
		owned := make([]byte, len(packet))
		copy(owned, packet)
		kws.dispatchEIOPacket(owned)
		count++
	}

	c.Set(fiber.HeaderContentType, "text/html")
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.Status(http.StatusOK).SendString("ok")
}
