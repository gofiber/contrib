package pydanticai

import (
	"bufio"
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
)

// streamEndGrace is how long a write to the client may block once Timeout
// elapsed or the server started shutting down. A client that takes nothing for
// that long stopped reading, and its connection is closed.
const streamEndGrace = 5 * time.Second

// requestHold is the fiber.Ctx that sse writes its response to. It ties the
// lifetime of the request to the end of the run, and gives the run a context
// of its own.
//
// fasthttp starts the goroutine that writes the response as soon as the handler
// sets it, and reuses the request when the connection is done with it, which
// for a client that went away is while the run is still winding down. A string
// that a tool took from the request would then read another request. Fiber
// keeps its own ctx, not the request of fasthttp.
//
// The body stream of the response that requestHold installs is closed by
// fasthttp before it reuses the request, after the response was written or has
// failed to be. Its Close waits for the run.
type requestHold struct {
	fiber.Ctx

	// ctx is the context of the run, see Context.
	ctx context.Context
	// release cancels the run, waits for it and reports the end of the stream
	// to OnRunEnd and OnClose. It does that once, however often it is called.
	release func()

	// conn is the connection of the request, taken while the handler runs.
	conn net.Conn
	// writing is when fasthttp started the write to the client that it is in,
	// in Unix nanoseconds, or 0 while it waits for the run.
	writing atomic.Int64

	// grace is how long a write may block once the run was told to end, see
	// streamEndGrace. It is taken when the stream starts.
	grace time.Duration

	mu sync.Mutex
	// done is set once fasthttp is done with the body: from then on the
	// connection can serve another request, and is left alone.
	done bool
	// check looks for a write that blocks, once the run was told to end.
	check *time.Timer
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
func (h *requestHold) Context() context.Context { return h.ctx }

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
