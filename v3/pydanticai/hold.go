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

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/gofiber/contrib/v3/pydanticai/internal/agentrun"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/sse"
	"github.com/gofiber/utils/v2"
	"github.com/valyala/fasthttp"
)

const streamEndGrace = 5 * time.Second

// serveStream sends what serve writes as a server-sent events response for a run
// it supervises. The context of serve ends when a write fails, the client is gone,
// Timeout elapses, the server shuts down or the stream ends, not with the request.
func serveStream[Deps, A any](
	c fiber.Ctx,
	opts *StreamConfig[Deps, A],
	deps Deps,
	options []ai.RunOption,
	serve func(ctx context.Context, run *agentrun.Run[Deps], options []ai.RunOption, send func(data []byte) error) error,
) error {
	// A request that a middleware gave up on is never answered, so run nothing.
	if err := context.Cause(c.Context()); err != nil {
		return err
	}

	// The run outlives the handler: keep the request values, not its cancellation.
	run, options := agentrun.New(context.WithoutCancel(c.Context()), agentrun.Config[Deps]{
		Deps:     deps,
		OnRunEnd: opts.OnRunEnd,
		Timeout:  opts.Timeout,
		Shutdown: shutdownSignal(c, opts.CancelOnShutdown),
	}, options)
	hold := newRequestHold(c, c.RequestCtx().Conn(), run, cmp.Or(opts.grace, streamEndGrace), func(err error) {
		if opts.OnClose != nil {
			agentrun.Safely("OnClose", func() { opts.OnClose(deps, err) })
		}
	})
	config := sse.Config{
		HeartbeatInterval: heartbeatInterval(opts.HeartbeatInterval),
		DisableHeartbeat:  opts.DisableHeartbeat,
		Handler: func(_ fiber.Ctx, stream *sse.Stream) error {
			return serve(stream.Context(), run, options, func(data []byte) error {
				// Safe: Event writes the string before it returns.
				return stream.Event(sse.Event{Data: utils.UnsafeString(data)})
			})
		},
		OnClose: func(_ fiber.Ctx, err error) { hold.streamEnded(err) },
	}
	return sse.New(config)(hold)
}

// supervised is the run that a hold serves, as agentrun.Run supervises it.
type supervised interface {
	Context() context.Context
	// AfterExpiry calls fn once when the server ends the run, not when it is canceled.
	AfterExpiry(fn func()) (stop func() bool)
	Cancel()
	End(err error)
}

// requestHold is the fiber.Ctx that sse writes to. It holds the request until the
// run has ended, as fasthttp reuses a request once its connection is done with
// it, and closes the connection of a client that stopped reading.
type requestHold struct {
	fiber.Ctx

	run      supervised
	onClose  func(err error)
	ended    chan error
	released sync.Once

	// conn is the connection of the request, taken while the handler runs.
	conn net.Conn
	// writing is when fasthttp started the write it is in, in Unix ns; 0 if none.
	writing atomic.Int64

	grace time.Duration

	mu sync.Mutex
	// done is set once fasthttp is done with the body: the connection is left alone.
	done  bool
	check *time.Timer

	// stopExpiry stops the call to expire once fasthttp is done: it would only
	// start a goroutine.
	stopExpiry func() bool
}

func newRequestHold(c fiber.Ctx, conn net.Conn, run supervised, grace time.Duration, onClose func(err error)) *requestHold {
	h := &requestHold{
		Ctx:     c,
		run:     run,
		onClose: onClose,
		ended:   make(chan error, 1),
		conn:    conn,
		grace:   grace,
	}
	h.stopExpiry = run.AfterExpiry(h.expire)
	return h
}

func (h *requestHold) streamEnded(err error) { h.ended <- err }

// release cancels the run, waits for it and reports its end, once.
func (h *requestHold) release() {
	h.released.Do(func() {
		h.run.Cancel()
		err := <-h.ended
		h.run.End(err)
		h.onClose(err)
	})
}

// expire looks for a client that stopped reading: its blocked write only fails
// when TCP gives up.
func (h *requestHold) expire() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done || h.conn == nil {
		return
	}
	h.check = time.AfterFunc(h.grace, h.cut)
}

// cut closes the connection if a write has been blocked for the grace.
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

func (h *requestHold) finish() {
	h.stopExpiry()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.done = true
	if h.check != nil {
		h.check.Stop()
	}
}

// Context is the context of the run, which sse derives the stream context from.
func (h *requestHold) Context() context.Context { return h.run.Context() }

func (h *requestHold) SendStreamWriter(write func(*bufio.Writer)) error {
	return h.SendStream(&holdingBody{ReadCloser: fasthttp.NewStreamReader(write), hold: h})
}

type holdingBody struct {
	io.ReadCloser
	hold *requestHold
}

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
	// fasthttp calls Close when the response is complete or cannot be written.
	b.hold.release()
	return err
}
