package websocket

import (
	"bufio"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// The middleware's own cost, from the router to the socket. Run with
//
//	go test -run '^$' -bench . -benchmem
//
// BenchmarkUpgrade and BenchmarkCoalescingConn measure the middleware alone.
// The loopback benchmarks include the client and the kernel, so compare them
// across commits on one machine rather than against other servers.

var payloadSizes = []struct {
	name string
	size int
}{{"5B", 5}, {"1KB", 1 << 10}, {"16KB", 16 << 10}, {"64KB", 64 << 10}}

func patterned(size int) []byte {
	p := make([]byte, size)
	for i := range p {
		p[i] = byte(i % 251)
	}
	return p
}

// BenchmarkUpgrade is the handshake without a socket: the router, the
// middleware and the library's Upgrade, up to the 101 fasthttp would send.
func BenchmarkUpgrade(b *testing.B) {
	app := fiber.New()
	app.Get("/ws", New(func(*Conn) {}))
	h := app.Handler()
	run := func(name string, fctx *fasthttp.RequestCtx, status int) {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				h(fctx)
				if got := fctx.Response.StatusCode(); got != status {
					b.Fatalf("status %d, want %d", got, status)
				}
				fctx.Response.Reset()
			}
		})
	}
	run("accepted", handshakeRequest(), fiber.StatusSwitchingProtocols)
	noKey := handshakeRequest()
	noKey.Request.Header.Del(fiber.HeaderSecWebSocketKey)
	run("rejected", noKey, fiber.StatusBadRequest)
	plain := &fasthttp.RequestCtx{}
	plain.Request.SetRequestURI("/ws")
	run("notUpgrade", plain, fiber.StatusUpgradeRequired)
}

func BenchmarkIsWebSocketUpgrade(b *testing.B) {
	app := fiber.New()
	c := app.AcquireCtx(handshakeRequest())
	defer app.ReleaseCtx(c)
	b.ReportAllocs()
	for b.Loop() {
		if !IsWebSocketUpgrade(c) {
			b.Fatal("not an upgrade")
		}
	}
}

func BenchmarkConnAccessors(b *testing.B) {
	conn := &Conn{ip: "127.0.0.1"}
	conn.capture(handshakeRequest())
	setEntry(&conn.params, "room", "lobby")
	for _, bc := range []struct {
		name string
		get  func() string
	}{
		{"Params", func() string { return conn.Params("room") }},
		{"Query", func() string { return conn.Query("room") }},
		{"Cookies", func() string { return conn.Cookies("session") }},
		{"Locals", func() string { return conn.Locals("user").(string) }},
		{"IP", conn.IP},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if bc.get() == "" {
					b.Fatal("empty value")
				}
			}
		})
	}
}

// BenchmarkUpgradeLoopback is a full upgrade over a socket: dial, handshake
// and close. Long benchtimes run into TIME_WAIT on the ephemeral ports.
func BenchmarkUpgradeLoopback(b *testing.B) {
	app := fiber.New()
	app.Get("/ws", New(func(c *Conn) {
		defer c.Close()
		_, _, _ = c.ReadMessage() // until the client hangs up
	}))
	url := "ws://" + listenBenchApp(b, app) + "/ws"
	b.ReportAllocs()
	for b.Loop() {
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			b.Fatal(err)
		}
		_ = conn.Close()
	}
}

// listenBenchApp serves app on a free port until the benchmark ends and
// returns the address to dial.
func listenBenchApp(b *testing.B, app *fiber.App) string {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(b, err)
	go func() { _ = app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	b.Cleanup(func() { _ = app.Shutdown() })
	return ln.Addr().String()
}

// dialBenchApp serves app for the benchmark's lifetime and returns a client
// connected to path.
func dialBenchApp(b *testing.B, app *fiber.App, path string) *websocket.Conn {
	b.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+listenBenchApp(b, app)+path, nil)
	require.NoError(b, err)
	b.Cleanup(func() { _ = conn.Close() })
	return conn
}

// benchFrame builds one masked client binary frame carrying payload.
func benchFrame(payload []byte) []byte {
	n := len(payload)
	f := make([]byte, 0, 14+n)
	f = append(f, 0x80|byte(websocket.BinaryMessage))
	switch {
	case n < 126:
		f = append(f, 0x80|byte(n))
	case n < 1<<16:
		f = append(f, 0x80|126, byte(n>>8), byte(n))
	default:
		f = append(f, 0x80|127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	key := [4]byte{1, 2, 3, 4}
	f = append(f, key[:]...)
	for i, c := range payload {
		f = append(f, c^key[i&3])
	}
	return f
}

// BenchmarkEcho is one message each way with one in flight, the shape of a
// request-reply client.
func BenchmarkEcho(b *testing.B) {
	for _, s := range payloadSizes {
		b.Run(s.name, func(b *testing.B) {
			app := fiber.New()
			app.Get("/ws", New(echoHandler))
			conn := dialBenchApp(b, app, "/ws")
			payload := patterned(s.size)
			b.SetBytes(int64(s.size))
			b.ReportAllocs()
			for b.Loop() {
				if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
					b.Fatal(err)
				}
				_, p, err := conn.ReadMessage()
				if err != nil {
					b.Fatal(err)
				}
				if len(p) != s.size {
					b.Fatalf("echoed %d bytes, want %d", len(p), s.size)
				}
			}
		})
	}
}

// BenchmarkEchoPipelined sends sixteen 5-byte frames in one write and reads
// the sixteen replies, the shape that write coalescing answers with one
// syscall.
func BenchmarkEchoPipelined(b *testing.B) {
	const frames = 16
	app := fiber.New()
	app.Get("/ws", New(echoHandler))
	conn := dialBenchApp(b, app, "/ws")
	var burst []byte
	for range frames {
		burst = append(burst, benchFrame([]byte("hello"))...)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := conn.NetConn().Write(burst); err != nil {
			b.Fatal(err)
		}
		for range frames {
			if _, _, err := conn.ReadMessage(); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*frames), "ns/frame")
}

// BenchmarkPush streams messages from the handler to a client that only
// reads: nothing is pending on the read side, so every write goes straight
// out.
func BenchmarkPush(b *testing.B) {
	for _, s := range payloadSizes {
		b.Run(s.name, func(b *testing.B) {
			payload := patterned(s.size)
			app := fiber.New()
			app.Get("/ws", New(func(c *Conn) {
				defer c.Close()
				for c.WriteMessage(websocket.BinaryMessage, payload) == nil {
				}
			}))
			conn := dialBenchApp(b, app, "/ws")
			b.SetBytes(int64(s.size))
			b.ReportAllocs()
			for b.Loop() {
				_, p, err := conn.ReadMessage()
				if err != nil {
					b.Fatal(err)
				}
				if len(p) != s.size {
					b.Fatalf("received %d bytes, want %d", len(p), s.size)
				}
			}
		})
	}
}

// frameBench is an upgraded loopback connection whose client side is a raw
// TCP socket, so the server can be fed batches of pre-built frames.
type frameBench struct {
	server *Conn
	client net.Conn
}

func newFrameBench(b *testing.B) *frameBench {
	b.Helper()
	done := make(chan struct{})
	connCh := make(chan *Conn, 1)
	app := fiber.New()
	app.Get("/", New(func(c *Conn) {
		connCh <- c
		<-done
	}))
	addr := listenBenchApp(b, app)

	client, err := net.Dial("tcp", addr)
	require.NoError(b, err)
	_, err = fmt.Fprintf(client, "GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", addr)
	require.NoError(b, err)
	br := bufio.NewReader(client)
	status, err := br.ReadString('\n')
	require.NoError(b, err)
	require.Contains(b, status, "101")
	for {
		line, err := br.ReadString('\n')
		require.NoError(b, err)
		if line == "\r\n" {
			break
		}
	}
	var server *Conn
	select {
	case server = <-connCh:
	case <-time.After(5 * time.Second):
		b.Fatal("server side never upgraded")
	}
	b.Cleanup(func() {
		close(done)
		_ = server.Close()
		_ = client.Close()
	})
	return &frameBench{server: server, client: client}
}

// BenchmarkReadMessage reads frames a raw client keeps the socket full of:
// the library's ReadMessage against the middleware's pooled one.
func BenchmarkReadMessage(b *testing.B) {
	for _, impl := range []struct {
		name   string
		pooled bool
	}{{"library", false}, {"pooled", true}} {
		for _, s := range payloadSizes {
			b.Run(impl.name+"/"+s.name, func(b *testing.B) {
				benchmarkReadMessage(b, s.size, impl.pooled)
			})
		}
	}
}

func benchmarkReadMessage(b *testing.B, size int, pooled bool) {
	fb := newFrameBench(b)
	payload := patterned(size)
	frame := benchFrame(payload)
	batch := frame
	for len(batch) < 256<<10 {
		batch = append(batch, frame...)
	}
	go func() {
		for {
			if _, err := fb.client.Write(batch); err != nil {
				return
			}
		}
	}()

	b.SetBytes(int64(size))
	b.ReportAllocs()
	for b.Loop() {
		var msg []byte
		var err error
		if pooled {
			_, msg, err = fb.server.ReadMessage()
		} else {
			_, msg, err = fb.server.Conn.ReadMessage()
		}
		if err != nil {
			b.Fatal(err)
		}
		if len(msg) != size || msg[size/2] != payload[size/2] {
			b.Fatalf("bad message of %d bytes", len(msg))
		}
	}
}

// fillConn answers every Read with fill bytes and swallows writes: a socket
// with no kernel behind it, leaving the wrapper's own work.
type fillConn struct{ fill int }

func (c *fillConn) Read([]byte) (int, error)       { return c.fill, nil }
func (*fillConn) Write(p []byte) (int, error)      { return len(p), nil }
func (*fillConn) Close() error                     { return nil }
func (*fillConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*fillConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*fillConn) SetDeadline(time.Time) error      { return nil }
func (*fillConn) SetReadDeadline(time.Time) error  { return nil }
func (*fillConn) SetWriteDeadline(time.Time) error { return nil }

// BenchmarkCoalescingConn is one exchange through the wrapper alone: a fill
// holding the given number of 5-byte client frames, a reply per frame, and
// the flush the next read triggers. One frame takes the direct path.
func BenchmarkCoalescingConn(b *testing.B) {
	reply := make([]byte, 7) // header plus 5 bytes
	for _, frames := range []int{1, 16} {
		b.Run(fmt.Sprintf("%dframes", frames), func(b *testing.B) {
			c := newCoalescingConn()
			c.attach(&fillConn{fill: 11 * frames})
			buf := make([]byte, 1024)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := c.Read(buf); err != nil {
					b.Fatal(err)
				}
				for range frames {
					if _, err := c.Write(reply); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
