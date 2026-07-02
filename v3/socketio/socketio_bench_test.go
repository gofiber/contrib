package socketio

import (
	"net"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp/fasthttputil"
)

// benchServer spins up the socketio middleware on an in-memory listener so
// connect/emit timings exclude the kernel TCP stack. It mirrors the production
// New() handler exactly; only the transport is swapped.
func benchServer(b *testing.B) (*fasthttputil.InmemoryListener, func()) {
	b.Helper()
	pool.reset()
	listeners.reset()

	app := fiber.New()
	ln := fasthttputil.NewInmemoryListener()

	app.Use(upgradeMiddleware)
	// Echo handler: every "ping" is answered with a "pong" event so the
	// client can measure round-trip latency.
	On(EventMessage, func(p *EventPayload) { p.Kws.EmitEvent("pong", p.Data) })
	app.Get("/", New(func(_ *Websocket) {}))

	go func() { _ = app.Listener(ln) }()

	return ln, func() { _ = app.Shutdown(); _ = ln.Close() }
}

// benchDial performs the full EIO/SIO handshake and returns a ready-to-use
// raw frame connection. It does NOT use the testing.T helpers so it is safe
// to call in tight benchmark loops.
func benchDial(b *testing.B, ln *fasthttputil.InmemoryListener) *websocket.Conn {
	b.Helper()
	d := &websocket.Dialer{
		NetDial:          func(_, _ string) (net.Conn, error) { return ln.Dial() },
		HandshakeTimeout: 10 * time.Second,
	}
	c, _, err := d.Dial("ws://"+ln.Addr().String(), nil)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	// EIO OPEN
	if _, _, err := c.ReadMessage(); err != nil {
		b.Fatalf("read open: %v", err)
	}
	// SIO CONNECT
	if err := c.WriteMessage(websocket.TextMessage, []byte("40")); err != nil {
		b.Fatalf("write connect: %v", err)
	}
	if _, _, err := c.ReadMessage(); err != nil {
		b.Fatalf("read connect ack: %v", err)
	}
	return c
}

// percentile returns the p-th percentile of d (d is mutated/sorted in place).
func percentile(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	idx := int(float64(len(d)-1) * p)
	return d[idx]
}

// BenchmarkConnectSetup measures the full EIO/SIO handshake latency. With
// -benchmem this also reports allocs/op for the connect path which is the
// hottest allocator in production. Expect <500us/op on Apple M1, <1ms on x86.
func BenchmarkConnectSetup(b *testing.B) {
	ln, stop := benchServer(b)
	defer stop()

	samples := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		t0 := time.Now()
		c := benchDial(b, ln)
		samples = append(samples, time.Since(t0))
		_ = c.Close()
	}
	b.StopTimer()
	b.ReportMetric(float64(percentile(samples, 0.50).Nanoseconds()), "p50-ns")
	b.ReportMetric(float64(percentile(samples, 0.95).Nanoseconds()), "p95-ns")
	b.ReportMetric(float64(percentile(samples, 0.99).Nanoseconds()), "p99-ns")
}

// BenchmarkRoundTrip measures end-to-end emit -> server-listener -> emit-back
// latency on a single warm connection (handshake amortised). Drives the
// "per-message latency" metric in the stress harness.
func BenchmarkRoundTrip(b *testing.B) {
	ln, stop := benchServer(b)
	defer stop()
	c := benchDial(b, ln)
	defer c.Close()

	frame := []byte(`42["message","x"]`)
	samples := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		t0 := time.Now()
		if err := c.WriteMessage(websocket.TextMessage, frame); err != nil {
			b.Fatalf("write: %v", err)
		}
		// Skip transparent EIO PINGs ("2").
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				b.Fatalf("read: %v", err)
			}
			if len(msg) == 1 && msg[0] == eioPing {
				_ = c.WriteMessage(websocket.TextMessage, []byte{eioPong})
				continue
			}
			break
		}
		samples = append(samples, time.Since(t0))
	}
	b.StopTimer()
	b.ReportMetric(float64(percentile(samples, 0.50).Nanoseconds()), "p50-ns")
	b.ReportMetric(float64(percentile(samples, 0.95).Nanoseconds()), "p95-ns")
	b.ReportMetric(float64(percentile(samples, 0.99).Nanoseconds()), "p99-ns")
}

// BenchmarkBroadcastFanout measures Broadcast() amplification with N subscribed
// connections receiving 1 emit. Useful for catching regressions in the
// pool.all() snapshot path or buildSIOEvent allocator.
func BenchmarkBroadcastFanout(b *testing.B) {
	const fanout = 1024
	ln, stop := benchServer(b)
	defer stop()

	conns := make([]*websocket.Conn, fanout)
	var ready sync.WaitGroup
	ready.Add(fanout)
	var connectedFires int32
	On(EventConnect, func(_ *EventPayload) {
		if atomic.AddInt32(&connectedFires, 1) <= int32(fanout) {
			ready.Done()
		}
	})
	for i := 0; i < fanout; i++ {
		conns[i] = benchDial(b, ln)
		// drain stray frames in a goroutine so the queue does not back up.
		go func(c *websocket.Conn) {
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		}(conns[i])
	}
	ready.Wait()

	msg := []byte(`"hello"`)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Broadcast(msg, TextMessage)
	}
	b.StopTimer()
	b.ReportMetric(float64(fanout), "subs")

	for _, c := range conns {
		_ = c.Close()
	}
}

// BenchmarkSteadyStateMemory holds N idle connections open and reports HeapAlloc
// growth + goroutine count after a fixed dwell time. Run with -benchtime=1x to
// take a single sample. Acts as a leak canary for the long-idle scenario in
// the external stress harness.
func BenchmarkSteadyStateMemory(b *testing.B) {
	const conns = 1024
	const dwell = 2 * time.Second
	ln, stop := benchServer(b)
	defer stop()

	gBefore := runtime.NumGoroutine()
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)

	clients := make([]*websocket.Conn, conns)
	for i := 0; i < conns; i++ {
		clients[i] = benchDial(b, ln)
	}
	time.Sleep(dwell)
	runtime.GC()
	runtime.ReadMemStats(&m1)
	gAfter := runtime.NumGoroutine()

	b.ReportMetric(float64(m1.HeapAlloc-m0.HeapAlloc)/float64(conns), "heap-bytes/conn")
	b.ReportMetric(float64(gAfter-gBefore)/float64(conns), "goroutines/conn")
	b.ReportMetric(float64(m1.Mallocs-m0.Mallocs)/dwell.Seconds(), "allocs/s")

	for _, c := range clients {
		_ = c.Close()
	}
}
