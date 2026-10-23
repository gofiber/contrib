package pydanticai

import (
	"bufio"
	"cmp"
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/contrib/v3/pydanticai/internal/agentrun"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/sse"
	"github.com/gofiber/utils/v2"
	"github.com/valyala/fasthttp"
)

// streamEndGrace is how long a write to the client may block once Timeout
// elapsed or the server started shutting down. A client that takes nothing for
// that long stopped reading, and its connection is closed.
const streamEndGrace = 5 * time.Second

// serveStream sends what serve writes as a server-sent events response.
//
// The context passed to serve is canceled when a write to the client fails, when
// a heartbeat detects that the client went away, when fasthttp gives up on the
// response, when Timeout elapses, when the server starts shutting down with
// CancelOnShutdown, and when the stream ends. fasthttp does not cancel the
// request context for a disconnected client, so these are the signals that stop
// a run nobody is listening to anymore. The cancellation of the context of the
// request is not among them, see requestHold.
//
// OnRunEnd and then OnClose are called after the response is over and the run
// has ended.
func serveStream[Deps, A any](
	c fiber.Ctx,
	opts *StreamConfig[Deps, A],
	deps Deps,
	run *agentrun.Run[Deps],
	serve func(ctx context.Context, send func(data []byte) error) error,
) error {
	hold := newRequestHold(c, c.RequestCtx().Conn(), run, cmp.Or(opts.grace, streamEndGrace), func(err error) {
		if opts.OnClose != nil {
			agentrun.Safely("OnClose", func() { opts.OnClose(deps, err) })
		}
	})
	config := sse.Config{
		HeartbeatInterval: heartbeatInterval(opts.HeartbeatInterval),
		DisableHeartbeat:  opts.DisableHeartbeat,
		Handler: func(_ fiber.Ctx, stream *sse.Stream) error {
			return serve(stream.Context(), func(data []byte) error {
				// sse copies a []byte into a string before it writes it, and
				// writes a string as it is. The event is written when Event
				// returns, and nothing changes data until then.
				return stream.Event(sse.Event{Data: utils.UnsafeString(data)})
			})
		},
		OnClose: func(_ fiber.Ctx, err error) { hold.streamEnded(err) },
	}
	return sse.New(config)(hold)
}

// supervised is the run that a hold serves, as agentrun.Run supervises it.
type supervised interface {
	// Context is the context of the run.
	Context() context.Context
	// Expired reports whether the server ended the run, because its Timeout
	// elapsed or it started shutting down.
	Expired() bool
	// Cancel cancels the run.
	Cancel()
	// End reports the end of the run, once.
	End(err error)
}

// requestHold is the fiber.Ctx that sse writes its response to, and the
// lifetime of that response. It ties the lifetime of the request to the end of
// the run, serves the stream with the context of the run, and closes the
// connection of a client that stopped reading once the server ended the run.
//
// fasthttp starts the goroutine that writes the response as soon as the handler
// sets it, and reuses the request when the connection is done with it, which
// for a client that went away is while the run is still winding down. A string
// that a tool took from the request would then read another request. Fiber
// keeps its own ctx, not the request of fasthttp.
//
// The body stream of the response that requestHold installs is closed by
// fasthttp before it reuses the request, after the response was written or has
// failed to be. Its Close waits for the run, and then reports its end to
// OnRunEnd and OnClose.
type requestHold struct {
	fiber.Ctx

	// run is the run that the response is for.
	run supervised
	// onClose is told how the stream ended, once the run has ended and OnRunEnd
	// was told.
	onClose func(err error)
	// ended receives how the stream ended, once the run has returned.
	ended chan error
	// released makes the end of the stream be reported once, however often it is
	// released.
	released sync.Once

	// conn is the connection of the request, taken while the handler runs.
	conn net.Conn
	// writing is when fasthttp started the write to the client that it is in,
	// in Unix nanoseconds, or 0 while it waits for the run.
	writing atomic.Int64

	// grace is how long a write may block once the run was told to end, see
	// streamEndGrace.
	grace time.Duration

	mu sync.Mutex
	// done is set once fasthttp is done with the body: from then on the
	// connection can serve another request, and is left alone.
	done bool
	// check looks for a write that blocks, once the run was told to end.
	check *time.Timer
}

// newRequestHold returns the hold for the response of run on conn, which is nil
// for a RequestCtx that no server serves. grace is how long a write may block
// once the server ended the run, and onClose is told how the stream ended.
func newRequestHold(c fiber.Ctx, conn net.Conn, run supervised, grace time.Duration, onClose func(err error)) *requestHold {
	h := &requestHold{
		Ctx:     c,
		run:     run,
		onClose: onClose,
		ended:   make(chan error, 1),
		conn:    conn,
		grace:   grace,
	}
	// The run ends when the server ends it, unless it is stuck in a write to a
	// client that stopped reading.
	context.AfterFunc(run.Context(), func() {
		if run.Expired() {
			h.expire()
		}
	})
	return h
}

// streamEnded is called by sse with how the stream ended, once the run has
// returned.
func (h *requestHold) streamEnded(err error) { h.ended <- err }

// release cancels the run, waits for it to return, and reports the end of the
// stream to OnRunEnd and OnClose. It does that once, however often it is called.
func (h *requestHold) release() {
	h.released.Do(func() {
		h.run.Cancel()
		err := <-h.ended
		h.run.End(err)
		h.onClose(err)
	})
}

// expire starts to look for a client that stopped reading, once Timeout elapsed
// or the server started shutting down. The run, OnRunEnd, OnClose and the
// shutdown would otherwise wait for a write to the client that only fails when
// TCP gives up on the connection, which a client that keeps its receive window
// closed can delay forever.
func (h *requestHold) expire() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done || h.conn == nil || h.check != nil {
		return
	}
	h.check = time.AfterFunc(h.grace, h.cut)
}

// cut closes the connection when a write to it has been blocked for the grace.
// The write then fails, which ends the response and the run.
func (h *requestHold) cut() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done {
		return
	}
	next := h.grace
	if since := h.writing.Load(); since != 0 {
		blocked := time.Since(time.Unix(0, since))
		if blocked >= h.grace {
			_ = h.conn.Close()
			return
		}
		next -= blocked
	}
	h.check.Reset(next)
}

// finish records that fasthttp is done with the body.
func (h *requestHold) finish() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.done = true
	if h.check != nil {
		h.check.Stop()
	}
}

// Context is what sse derives the context of the stream from. It carries the
// values of the request, and is canceled by release rather than by a
// middleware that scoped the context of the request to the handler.
func (h *requestHold) Context() context.Context { return h.run.Context() }

// SendStreamWriter sets the body of the response like fiber.Ctx does, with a
// body stream that holds the request.
func (h *requestHold) SendStreamWriter(write func(*bufio.Writer)) error {
	return h.SendStream(&holdingBody{ReadCloser: fasthttp.NewStreamReader(write), hold: h})
}

// holdingBody is the body stream of a response that holds its request.
type holdingBody struct {
	io.ReadCloser
	hold *requestHold
}

// Read is called by fasthttp between its writes to the client.
func (b *holdingBody) Read(p []byte) (int, error) {
	b.hold.writing.Store(0)
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.hold.writing.Store(time.Now().UnixNano())
	}
	return n, err
}

// Close ends the response. It does not return before the run has ended.
func (b *holdingBody) Close() error {
	b.hold.finish()
	err := b.ReadCloser.Close()
	// fasthttp calls Close once the response is complete, and when it cannot
	// write the response any more, which means that the client went away.
	b.hold.release()
	return err
}
