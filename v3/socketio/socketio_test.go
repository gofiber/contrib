package socketio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fasthttp/websocket"
	fws "github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp/fasthttputil"
)

const numTestConn = 10
const numParallelTestConn = 5_000

type HandlerMock struct {
	mock.Mock
	wg sync.WaitGroup
}

type WebsocketMock struct {
	mock.Mock
	mu         sync.RWMutex
	wg         sync.WaitGroup
	Conn       *websocket.Conn
	isAlive    bool
	queue      map[string]message
	attributes map[string]string
	UUID       string
	Locals     func(key string) interface{}
	Params     func(key string, defaultValue ...string) string
	Query      func(key string, defaultValue ...string) string
	Cookies    func(key string, defaultValue ...string) string
}

func (s *WebsocketMock) SetUUID(uuid string) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := pool.get(uuid); err == nil {
		panic(ErrorUUIDDuplication)
	}
	s.UUID = uuid
	return nil
}

func (s *WebsocketMock) GetIntAttribute(key string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.attributes[key]
	if ok {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return 0
}

func (s *WebsocketMock) GetStringAttribute(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.attributes[key]
	if ok {
		return value
	}
	return ""
}

func (h *HandlerMock) OnCustomEvent(payload *EventPayload) {
	h.Called(payload)
	h.wg.Done()
}

func (s *WebsocketMock) Emit(message []byte, _ ...int) {
	s.Called(message)
	s.wg.Done()
}

// EmitWithAck satisfies the ws interface for test mocks. The mock does not
// model the ack callback dispatch; Tests should use the real *Websocket if
// they want to exercise that path.
func (s *WebsocketMock) EmitWithAck(_ string, _ []byte, _ func([]byte)) {}

// EmitWithAckTimeout: same no-op for the mock.
func (s *WebsocketMock) EmitWithAckTimeout(_ string, _ []byte, _ time.Duration, _ AckCallback) {
}

// EmitArgs / EmitWithAckArgs: no-op stubs for the mock.
func (s *WebsocketMock) EmitArgs(_ string, _ ...[]byte) {}
func (s *WebsocketMock) EmitWithAckArgs(_ string, _ [][]byte, _ func([][]byte, error)) {
}

func (s *WebsocketMock) IsAlive() bool {
	args := s.Called()
	return args.Bool(0)
}

func (s *WebsocketMock) GetUUID() string {
	return s.UUID
}

func TestParallelConnections(t *testing.T) {
	resetSIOGlobals(t)

	// create test server
	cfg := fiber.Config{}
	app := fiber.New(cfg)
	ln := fasthttputil.NewInmemoryListener()
	wg := sync.WaitGroup{}

	defer func() {
		_ = app.Shutdown()
		_ = ln.Close()
	}()

	// attach upgrade middleware
	app.Use(upgradeMiddleware)

	// echo "response" JSON string when a "message" event carrying `"test"` arrives
	On(EventMessage, func(payload *EventPayload) {
		if string(payload.Data) == `"test"` {
			payload.Kws.Emit([]byte(`"response"`))
		}
	})

	// create websocket endpoint
	app.Get("/", New(func(kws *Websocket) {
	}))

	// start server
	go func() {
		_ = app.Listener(ln)
	}()

	// Pre-set KeepHijackedConns so the upstream gofiber/contrib/v3/websocket
	// package does not race on the unsynchronised check-then-set inside its
	// upgrade handler ("if !c.App().Server().KeepHijackedConns { ... = true }").
	// The first parallel goroutine writes true and subsequent ones race the
	// read; warming the field serially before the fan-out closes that window.
	app.Server().KeepHijackedConns = true

	wsURL := "ws://" + ln.Addr().String()

	// create concurrent connections – each one performs the full socket.io handshake
	for i := 0; i < numParallelTestConn; i++ {
		wg.Add(1)
		go func() {
			dialer := &websocket.Dialer{
				NetDial: func(network, addr string) (net.Conn, error) {
					return ln.Dial()
				},
				HandshakeTimeout: 45 * time.Second,
			}
			dial, _, err := dialer.Dial(wsURL, nil)
			if err != nil {
				t.Error(err)
				return
			}

			// Perform socket.io handshake via the helper
			if err := sioHandshake(t, dial); err != nil {
				t.Error(err)
				return
			}

			// Send a socket.io "message" event: 42["message","test"]
			if err := dial.WriteMessage(websocket.TextMessage, []byte(`42["message","test"]`)); err != nil {
				t.Error(err)
				return
			}

			// Read back the server's response, skipping any EIO PING packets
			tp, m, err := sioReadSkipPings(dial)
			if err != nil {
				t.Error(err)
				return
			}
			require.Equal(t, TextMessage, tp)
			require.Equal(t, `42["message","response"]`, string(m))
			wg.Done()

			if err := dial.Close(); err != nil {
				t.Error(err)
				return
			}
		}()
	}
	wg.Wait()
}

func TestGlobalFire(t *testing.T) {
	resetSIOGlobals(t)

	// simulate connections
	for i := 0; i < numTestConn; i++ {
		kws := createWS()
		pool.set(kws)
	}

	h := new(HandlerMock)
	// setup expectations
	h.On("OnCustomEvent", mock.Anything).Return(nil)

	// Moved before registration of the event
	// if after can cause: panic: sync: negative WaitGroup counter
	h.wg.Add(numTestConn)

	// register custom event handler
	On("customevent", h.OnCustomEvent)

	// fire global custom event on all connections
	Fire("customevent", []byte("test"))

	h.wg.Wait()

	h.AssertNumberOfCalls(t, "OnCustomEvent", numTestConn)
}

func TestGlobalBroadcast(t *testing.T) {
	resetSIOGlobals(t)

	for i := 0; i < numParallelTestConn; i++ {
		mws := new(WebsocketMock)
		mws.SetUUID(mws.createUUID())
		pool.set(mws)

		// setup expectations
		mws.On("Emit", mock.Anything).Return(nil)

		mws.wg.Add(1)
	}

	// send global broadcast to all connections
	Broadcast([]byte("test"), TextMessage)

	for _, mws := range pool.all() {
		mws.(*WebsocketMock).wg.Wait()
		mws.(*WebsocketMock).AssertNumberOfCalls(t, "Emit", 1)
	}

}

func TestGlobalEmitTo(t *testing.T) {
	resetSIOGlobals(t)

	aliveUUID := "80a80sdf809dsf"
	closedUUID := "las3dfj09808"

	alive := new(WebsocketMock)
	alive.UUID = aliveUUID
	pool.set(alive)

	closed := new(WebsocketMock)
	closed.UUID = closedUUID
	pool.set(closed)

	// setup expectations
	alive.On("Emit", mock.Anything).Return(nil)
	alive.On("IsAlive").Return(true)
	closed.On("IsAlive").Return(false)

	var err error
	err = EmitTo("non-existent", []byte("error"))
	require.Equal(t, ErrorInvalidConnection, err)

	err = EmitTo(closedUUID, []byte("error"))
	require.Equal(t, ErrorInvalidConnection, err)

	alive.wg.Add(1)

	// send global broadcast to all connections
	err = EmitTo(aliveUUID, []byte("test"))
	require.Nil(t, err)

	alive.wg.Wait()

	alive.AssertNumberOfCalls(t, "Emit", 1)
}

func TestGlobalEmitToList(t *testing.T) {
	resetSIOGlobals(t)

	uuids := []string{
		"80a80sdf809dsf",
		"las3dfj09808",
	}

	for _, id := range uuids {
		kws := new(WebsocketMock)
		kws.SetUUID(id)
		kws.On("Emit", mock.Anything).Return(nil)
		kws.On("IsAlive").Return(true)
		kws.wg.Add(1)
		pool.set(kws)
	}

	// send global broadcast to all connections
	EmitToList(uuids, []byte("test"), TextMessage)

	for _, kws := range pool.all() {
		kws.(*WebsocketMock).wg.Wait()
		kws.(*WebsocketMock).AssertNumberOfCalls(t, "Emit", 1)
	}
}

func TestWebsocket_GetIntAttribute(t *testing.T) {
	kws := &Websocket{
		attributes: make(map[string]interface{}),
	}

	// get unset attribute
	// Will return null without panicking

	// get non-int attribute
	// Will return 0 without panicking
	kws.SetAttribute("notInt", "")

	// get int attribute
	kws.SetAttribute("int", 3)
	v := kws.GetIntAttribute("int")
	require.Equal(t, 3, v)
}

func TestWebsocket_GetStringAttribute(t *testing.T) {
	kws := &Websocket{
		attributes: make(map[string]interface{}),
	}

	// get unset attribute

	// get non-string attribute
	kws.SetAttribute("notString", 3)

	// get string attribute
	kws.SetAttribute("str", "3")
	v := kws.GetStringAttribute("str")
	require.Equal(t, "3", v)
}

func TestWebsocket_SetUUIDUpdatesPool(t *testing.T) {
	resetSIOGlobals(t)

	kws := createWS()
	pool.set(kws)

	oldUUID := kws.GetUUID()
	newUUID := "new-uuid"

	err := kws.SetUUID(newUUID)
	require.NoError(t, err)
	require.Equal(t, newUUID, kws.GetUUID())

	_, err = pool.get(oldUUID)
	require.ErrorIs(t, err, ErrorInvalidConnection)

	poolEntry, err := pool.get(newUUID)
	require.NoError(t, err)
	require.Equal(t, kws, poolEntry)

	other := createWS()
	other.UUID = "other-uuid"
	pool.set(other)

	err = kws.SetUUID(other.UUID)
	require.ErrorIs(t, err, ErrorUUIDDuplication)
	require.Equal(t, newUUID, kws.GetUUID())

	poolEntry, err = pool.get(newUUID)
	require.NoError(t, err)
	require.Equal(t, kws, poolEntry)
}

func assertPanic(t *testing.T, f func()) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("The code did not panic")
		}
	}()
	f()
}

func createWS() *Websocket {
	kws := &Websocket{
		Conn: nil,
		Locals: func(key string) interface{} {
			return ""
		},
		Params: func(key string, defaultValue ...string) string {
			return ""
		},
		Query: func(key string, defaultValue ...string) string {
			return ""
		},
		Cookies: func(key string, defaultValue ...string) string {
			return ""
		},
		queue:      make(chan message),
		attributes: make(map[string]interface{}),
	}
	kws.isAlive.Store(true)

	kws.UUID = kws.createUUID()

	return kws
}

func upgradeMiddleware(c fiber.Ctx) error {
	// IsWebSocketUpgrade returns true if the client
	// requested upgrade to the WebSocket protocol.
	if fws.IsWebSocketUpgrade(c) {
		fiber.StoreInContext(c, "allowed", true)
		return c.Next()
	}
	return fiber.ErrUpgradeRequired
}

// resetSIOGlobals isolates a test from any global state left behind by
// previous tests AND from any state THIS test leaves behind. It:
//
//  1. resets the global listener registry and connection pool at start so
//     this test starts from a clean slate, and
//  2. registers t.Cleanup to do the same on teardown so the next test
//     (especially one running in -count=N or after a t.Parallel sibling)
//     also starts clean. Any *Websocket entries still in the pool are
//     closed so their per-connection goroutines do not leak across tests.
//
// Use this in EVERY new test instead of bare pool.reset() / listeners.reset()
// at the top of the function.
func resetSIOGlobals(t *testing.T) {
	t.Helper()
	pool.reset()
	listeners.reset()
	t.Cleanup(func() {
		// Close every still-pooled connection so its read/send/pong
		// goroutines exit before the next test snapshots them.
		for _, w := range pool.all() {
			if k, ok := w.(*Websocket); ok {
				func() {
					defer func() { _ = recover() }()
					k.Close()
				}()
			}
		}
		pool.reset()
		listeners.reset()
	})
}

//
// needed but not used
//

func (s *WebsocketMock) SetAttribute(_ string, _ interface{}) {
	panic("implement me")
}

func (s *WebsocketMock) GetAttribute(_ string) interface{} {
	panic("implement me")
}

func (s *WebsocketMock) EmitToList(_ []string, _ []byte, _ ...int) {
	panic("implement me")
}

func (s *WebsocketMock) EmitTo(_ string, _ []byte, _ ...int) error {
	panic("implement me")
}

func (s *WebsocketMock) Broadcast(_ []byte, _ bool, _ ...int) {
	panic("implement me")
}

func (s *WebsocketMock) Fire(_ string, _ []byte) {
	panic("implement me")
}

func (s *WebsocketMock) Close() {
	panic("implement me")
}

func (s *WebsocketMock) pong(_ context.Context) {
	panic("implement me")
}

func (s *WebsocketMock) write(_ int, _ []byte) {
	panic("implement me")
}

func (s *WebsocketMock) run() {
	panic("implement me")
}

func (s *WebsocketMock) read(_ context.Context) {
	panic("implement me")
}

func (s *WebsocketMock) disconnected(_ error) {
	panic("implement me")
}

func (s *WebsocketMock) createUUID() string {
	return s.randomUUID()
}

func (s *WebsocketMock) EmitEvent(_ string, _ []byte) {
	panic("implement me")
}

func (s *WebsocketMock) randomUUID() string {
	return uuid.New().String()
}

func (s *WebsocketMock) fireEvent(_ string, _ []byte, _ error) {
	panic("implement me")
}

// ---------------------------------------------------------------------------
// Socket.IO test helpers
// ---------------------------------------------------------------------------

// sioHandshake performs the Engine.IO / Socket.IO connection handshake:
//
//  1. Reads the EIO OPEN packet ("0{...}") from the server.
//  2. Sends the SIO CONNECT packet ("40").
//  3. Reads and validates the SIO CONNECT confirmation ("40{...}").
func sioHandshake(t *testing.T, conn *websocket.Conn) error {
	t.Helper()

	// 1. Read EIO OPEN
	mType, msg, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	if mType != websocket.TextMessage {
		t.Errorf("sioHandshake: expected TextMessage, got %d", mType)
		return nil
	}
	if len(msg) == 0 || msg[0] != eioOpen {
		t.Errorf("sioHandshake: expected EIO OPEN ('0'), got %q", msg)
		return nil
	}
	// Parse the open data to validate structure
	var openData eioOpenPacket
	if err := json.Unmarshal(msg[1:], &openData); err != nil {
		t.Errorf("sioHandshake: failed to parse EIO OPEN payload: %v", err)
		return nil
	}
	if openData.SID == "" {
		t.Error("sioHandshake: EIO OPEN payload missing 'sid'")
		return nil
	}

	// 2. Send SIO CONNECT ("40")
	if err := conn.WriteMessage(websocket.TextMessage, []byte("40")); err != nil {
		return err
	}

	// 3. Read SIO CONNECT confirmation ("40{...}")
	mType, msg, err = conn.ReadMessage()
	if err != nil {
		return err
	}
	if mType != websocket.TextMessage {
		t.Errorf("sioHandshake: expected TextMessage for SIO CONNECT conf, got %d", mType)
		return nil
	}
	if len(msg) < 2 || msg[0] != eioMessage || msg[1] != sioConnect {
		t.Errorf("sioHandshake: expected SIO CONNECT conf ('40...'), got %q", msg)
	}

	return nil
}

// sioReadSkipPings reads the next meaningful message, transparently responding
// to any EIO PING packets ("2") with PONG packets ("3").
func sioReadSkipPings(conn *websocket.Conn) (int, []byte, error) {
	for {
		mType, msg, err := conn.ReadMessage()
		if err != nil {
			return 0, nil, err
		}
		if mType == websocket.TextMessage && len(msg) == 1 && msg[0] == eioPing {
			// Respond with PONG and keep waiting
			if werr := conn.WriteMessage(websocket.TextMessage, []byte{eioPong}); werr != nil {
				return 0, nil, werr
			}
			continue
		}
		return mType, msg, nil
	}
}

// ---------------------------------------------------------------------------
// Socket.IO integration tests
// ---------------------------------------------------------------------------

// newSIOTestServer starts a Fiber app with the socketio middleware and returns
// the listener and a teardown function.
func newSIOTestServer(t *testing.T, callback func(*Websocket)) (*fasthttputil.InmemoryListener, func()) {
	t.Helper()

	app := fiber.New()
	ln := fasthttputil.NewInmemoryListener()

	app.Use(upgradeMiddleware)
	app.Get("/", New(callback))

	go func() { _ = app.Listener(ln) }()

	return ln, func() {
		_ = app.Shutdown()
		_ = ln.Close()
	}
}

// dialSIO creates a raw WebSocket connection that bypasses the real network.
func dialSIO(t *testing.T, ln *fasthttputil.InmemoryListener) *websocket.Conn {
	t.Helper()

	dialer := &websocket.Dialer{
		NetDial: func(_, _ string) (net.Conn, error) {
			return ln.Dial()
		},
		HandshakeTimeout: 10 * time.Second,
	}
	wsURL := "ws://" + ln.Addr().String()
	conn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	return conn
}

// TestSocketIOHandshake verifies the full EIO / socket.io connection handshake.
func TestSocketIOHandshake(t *testing.T) {
	resetSIOGlobals(t)

	connectFired := make(chan struct{}, 1)
	On(EventConnect, func(payload *EventPayload) {
		select {
		case connectFired <- struct{}{}:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	require.NoError(t, sioHandshake(t, conn))

	select {
	case <-connectFired:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect was not fired within the timeout")
	}
}

// TestSocketIOEvent verifies that a socket.io event sent by the client is
// delivered to the server-side EventMessage listener with the correct payload.
func TestSocketIOEvent(t *testing.T) {
	resetSIOGlobals(t)

	received := make(chan []byte, 1)
	On(EventMessage, func(payload *EventPayload) {
		received <- payload.Data
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	require.NoError(t, sioHandshake(t, conn))

	// Send: 42["message",{"hello":"world"}]
	msg := `42["message",{"hello":"world"}]`
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(msg)))

	select {
	case data := <-received:
		// The payload should be just the JSON object, without the array wrapper
		require.Equal(t, `{"hello":"world"}`, string(data))
	case <-time.After(2 * time.Second):
		t.Fatal("EventMessage was not fired within the timeout")
	}
}

// TestSocketIOEmitEvent verifies that the server can push a named event to the
// client and that the wire format matches the socket.io protocol.
func TestSocketIOEmitEvent(t *testing.T) {
	resetSIOGlobals(t)

	ready := make(chan *Websocket, 1)
	On(EventConnect, func(payload *EventPayload) {
		select {
		case ready <- payload.Kws:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	require.NoError(t, sioHandshake(t, conn))

	// Wait until EventConnect fires and obtain the server-side handle
	var kws *Websocket
	select {
	case kws = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire within the timeout")
	}

	// Server pushes a named event to the client
	kws.EmitEvent("greet", []byte(`"hello"`))

	tp, raw, err := sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, tp)
	require.Equal(t, `42["greet","hello"]`, string(raw))
}

// TestSocketIOEmit verifies that Emit wraps the payload as a "message" event.
func TestSocketIOEmit(t *testing.T) {
	resetSIOGlobals(t)

	ready := make(chan *Websocket, 1)
	On(EventConnect, func(payload *EventPayload) {
		select {
		case ready <- payload.Kws:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	require.NoError(t, sioHandshake(t, conn))

	var kws *Websocket
	select {
	case kws = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire within the timeout")
	}

	// Server emits raw JSON – should arrive as a socket.io "message" event
	kws.Emit([]byte(`{"key":"value"}`))

	tp, raw, err := sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, tp)
	require.Equal(t, `42["message",{"key":"value"}]`, string(raw))
}

// TestSocketIOCustomEvent verifies that a client-sent custom event (non-"message")
// is routed to a custom listener rather than EventMessage.
func TestSocketIOCustomEvent(t *testing.T) {
	resetSIOGlobals(t)

	customReceived := make(chan []byte, 1)
	messageReceived := make(chan []byte, 1)

	On("custom", func(payload *EventPayload) {
		customReceived <- payload.Data
	})
	On(EventMessage, func(payload *EventPayload) {
		messageReceived <- payload.Data
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	require.NoError(t, sioHandshake(t, conn))

	// Send a "custom" event, not a "message" event
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`42["custom","payload"]`)))

	select {
	case data := <-customReceived:
		require.Equal(t, `"payload"`, string(data))
	case <-time.After(2 * time.Second):
		t.Fatal("custom event listener was not called within the timeout")
	}

	// The EventMessage listener must NOT have been triggered
	select {
	case <-messageReceived:
		t.Fatal("EventMessage was unexpectedly triggered by a custom event")
	default:
	}
}

// TestSocketIODisconnect verifies that sending a SIO DISCONNECT packet ("41")
// causes the server to fire EventDisconnect.
func TestSocketIODisconnect(t *testing.T) {
	resetSIOGlobals(t)

	disconnected := make(chan struct{}, 1)
	On(EventDisconnect, func(_ *EventPayload) {
		select {
		case disconnected <- struct{}{}:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	require.NoError(t, sioHandshake(t, conn))

	// Client sends SIO DISCONNECT
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("41")))

	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("EventDisconnect was not fired within the timeout")
	}
}

// TestSocketIOHeartbeat verifies that an EIO PING from the server to the
// client triggers an EventPong on the server when the client replies.
//
// We intentionally do NOT mutate the global PingInterval here: that races
// with previously-spawned pong goroutines reading the same global. Instead,
// we drive a single PING manually from the server side via kws.write.
func TestSocketIOHeartbeat(t *testing.T) {
	resetSIOGlobals(t)

	pongReceived := make(chan struct{}, 1)
	On(EventPong, func(_ *EventPayload) {
		select {
		case pongReceived <- struct{}{}:
		default:
		}
	})

	kwsCh := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case kwsCh <- p.Kws:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	require.NoError(t, sioHandshake(t, conn))

	var kws *Websocket
	select {
	case kws = <-kwsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire")
	}

	// Force an immediate EIO PING from the server side (avoids the 25s
	// ticker wait and the data race that goes with mutating PingInterval).
	kws.write(TextMessage, []byte{eioPing})

	// Read the PING off the wire.
	tp, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, tp)
	require.Equalf(t, []byte{eioPing}, msg, "expected PING ('2'), got %q", msg)

	// Reply with PONG.
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte{eioPong}))

	select {
	case <-pongReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("EventPong was not fired within the timeout")
	}
}

// TestSocketIOBuildSIOEvent checks the wire-format produced by buildSIOEvent.
func TestSocketIOBuildSIOEvent(t *testing.T) {
	cases := []struct {
		name      string
		namespace []byte
		event     string
		data      []byte
		expected  string
	}{
		{"root no data", nil, "ping", nil, `42["ping"]`},
		{"root string data", nil, "message", []byte(`"hello"`), `42["message","hello"]`},
		{"root raw text data", nil, "message", []byte(`hello`), `42["message","hello"]`},
		{"root object data", nil, "update", []byte(`{"key":"val"}`), `42["update",{"key":"val"}]`},
		{"namespaced", []byte("/admin"), "msg", []byte(`"x"`), `42/admin,["msg","x"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildSIOEvent(tc.namespace, tc.event, tc.data)
			require.Equal(t, tc.expected, string(got))
		})
	}
}

// TestSocketIOParseSIOEvent checks round-trip decoding of SIO EVENT payloads.
func TestSocketIOParseSIOEvent(t *testing.T) {
	t.Run("single string arg", func(t *testing.T) {
		name, args, err := parseSIOEvent([]byte(`["message","hello"]`))
		require.NoError(t, err)
		require.Equal(t, "message", name)
		require.Len(t, args, 1)
		require.Equal(t, `"hello"`, string(args[0]))
	})
	t.Run("single object arg", func(t *testing.T) {
		name, args, err := parseSIOEvent([]byte(`["update",{"k":"v"}]`))
		require.NoError(t, err)
		require.Equal(t, "update", name)
		require.Len(t, args, 1)
		require.Equal(t, `{"k":"v"}`, string(args[0]))
	})
	t.Run("multiple args", func(t *testing.T) {
		name, args, err := parseSIOEvent([]byte(`["event","a","b"]`))
		require.NoError(t, err)
		require.Equal(t, "event", name)
		require.Len(t, args, 2)
		require.Equal(t, `"a"`, string(args[0]))
		require.Equal(t, `"b"`, string(args[1]))
	})
	t.Run("no data arg", func(t *testing.T) {
		name, args, err := parseSIOEvent([]byte(`["ping"]`))
		require.NoError(t, err)
		require.Equal(t, "ping", name)
		require.Nil(t, args)
	})
	t.Run("invalid JSON", func(t *testing.T) {
		_, _, err := parseSIOEvent([]byte(`not json`))
		require.Error(t, err)
	})
	t.Run("empty array", func(t *testing.T) {
		_, _, err := parseSIOEvent([]byte(`[]`))
		require.Error(t, err)
	})
}

// TestSocketIOEmitInsideNewCallbackArrivesAfterHandshake is the regression
// test for issue #1903: a server that calls Emit inside the New() callback
// must not leak the message before the EIO OPEN / SIO CONNECT handshake.
func TestSocketIOEmitInsideNewCallbackArrivesAfterHandshake(t *testing.T) {
	resetSIOGlobals(t)

	ln, teardown := newSIOTestServer(t, func(kws *Websocket) {
		// Calling Emit inside the New() callback used to send before EIO OPEN.
		kws.Emit([]byte(`"early"`))
	})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	// 1. EIO OPEN must be the very first frame on the wire.
	mType, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, mType)
	require.NotEmpty(t, msg)
	require.Equalf(t, byte(eioOpen), msg[0], "first frame must be EIO OPEN, got %q", msg)

	// 2. Send SIO CONNECT.
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("40")))

	// 3. Read SIO CONNECT confirmation.
	_, msg, err = conn.ReadMessage()
	require.NoError(t, err)
	require.Truef(t,
		len(msg) >= 2 && msg[0] == eioMessage && msg[1] == sioConnect,
		"expected SIO CONNECT, got %q", msg,
	)

	// 4. Only AFTER the handshake should the welcome message arrive.
	_, msg, err = sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, `42["message","early"]`, string(msg))
}

func TestSocketIOEmitRawTextIsJSONEncoded(t *testing.T) {
	resetSIOGlobals(t)

	ready := make(chan *Websocket, 1)
	On(EventConnect, func(payload *EventPayload) {
		select {
		case ready <- payload.Kws:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	require.NoError(t, sioHandshake(t, conn))

	var kws *Websocket
	select {
	case kws = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire")
	}

	kws.Emit([]byte(`Hello user`))
	_, msg, err := sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, `42["message","Hello user"]`, string(msg))

	kws.EmitEvent("notice", []byte(`plain text`))
	_, msg, err = sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, `42["notice","plain text"]`, string(msg))
}

func TestSocketIONewCallbackCloseSuppressesEventConnect(t *testing.T) {
	resetSIOGlobals(t)

	connectFired := make(chan struct{}, 1)
	On(EventConnect, func(_ *EventPayload) {
		connectFired <- struct{}{}
	})

	ln, teardown := newSIOTestServer(t, func(kws *Websocket) {
		kws.Close()
	})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	require.NoError(t, sioHandshake(t, conn))

	select {
	case <-connectFired:
		t.Fatal("EventConnect fired after New callback closed the socket")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestSocketIONamespaceHandshake verifies that a client connecting to a
// non-root namespace gets a namespace-prefixed CONNECT ack and that server
// emits are scoped to the same namespace.
func TestSocketIONamespaceHandshake(t *testing.T) {
	resetSIOGlobals(t)

	ready := make(chan *Websocket, 1)
	On(EventConnect, func(payload *EventPayload) {
		select {
		case ready <- payload.Kws:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	// 1. Read EIO OPEN.
	mType, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, mType)
	require.Equal(t, byte(eioOpen), msg[0])

	// 2. CONNECT to /admin namespace.
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("40/admin,")))

	// 3. Confirmation must echo the namespace.
	_, msg, err = conn.ReadMessage()
	require.NoError(t, err)
	require.True(t,
		strings.HasPrefix(string(msg), "40/admin,{"),
		"expected '40/admin,{...}', got %q", msg,
	)

	// 4. Wait for the server-side handle and emit a server-side event.
	var kws *Websocket
	select {
	case kws = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire")
	}
	kws.EmitEvent("hello", []byte(`"world"`))

	// 5. The pushed event must include the namespace prefix.
	_, msg, err = sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, `42/admin,["hello","world"]`, string(msg))
}

// TestSocketIOInboundAck verifies that an event packet with an ack id
// (42<id>[event,data]) is exposed to the listener as HasAck=true / AckID=N
// and that EventPayload.Ack(...) puts the matching 43<id>[<data>] frame on
// the wire.
func TestSocketIOInboundAck(t *testing.T) {
	resetSIOGlobals(t)

	On(EventMessage, func(ep *EventPayload) {
		if ep.HasAck {
			_ = ep.Ack([]byte(`"ok"`))
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	// Client sends: 421["message",{"k":"v"}]  (ack id = 1)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`421["message",{"k":"v"}]`)))

	tp, msg, err := sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, tp)
	require.Equal(t, `431["ok"]`, string(msg))
}

// TestSocketIOOutboundAck verifies that EmitWithAck encodes the ack id and
// that the registered callback fires when the client replies with a 43.
func TestSocketIOOutboundAck(t *testing.T) {
	resetSIOGlobals(t)

	ready := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case ready <- p.Kws:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	var kws *Websocket
	select {
	case kws = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("no EventConnect")
	}

	gotAck := make(chan []byte, 1)
	kws.EmitWithAck("ping", []byte(`"hi"`), func(ack []byte) {
		gotAck <- ack
	})

	tp, msg, err := sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, tp)
	require.Equal(t, `421["ping","hi"]`, string(msg))

	// Client replies with 431["pong"]
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`431["pong"]`)))

	select {
	case ack := <-gotAck:
		require.Equal(t, `"pong"`, string(ack))
	case <-time.After(2 * time.Second):
		t.Fatal("ack callback never fired")
	}
}

// TestSocketIOSplitAckID exercises the leading-digits parser used for
// inbound ack-id detection.
func TestSocketIOSplitAckID(t *testing.T) {
	cases := []struct {
		in       string
		hasID    bool
		id       uint64
		rest     string
		errsLike string
	}{
		{`["x"]`, false, 0, `["x"]`, ""},
		{`5["x"]`, true, 5, `["x"]`, ""},
		{`12345["x","y"]`, true, 12345, `["x","y"]`, ""},
		{`0["x"]`, true, 0, `["x"]`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			id, has, rest, err := splitSIOAckID([]byte(tc.in))
			require.NoError(t, err)
			require.Equal(t, tc.hasID, has)
			require.Equal(t, tc.id, id)
			require.Equal(t, tc.rest, string(rest))
		})
	}
}

// TestSocketIOBuildSIOAck checks the wire format of ack frames.
func TestSocketIOBuildSIOAck(t *testing.T) {
	require.Equal(t, `431[]`, string(buildSIOAck(nil, 1, nil)))
	require.Equal(t, `431["ok"]`, string(buildSIOAck(nil, 1, [][]byte{[]byte(`"ok"`)})))
	require.Equal(t, `431["ok"]`, string(buildSIOAck(nil, 1, [][]byte{[]byte(`ok`)})))
	require.Equal(t, `43/admin,7[{"x":1}]`, string(buildSIOAck([]byte("/admin"), 7, [][]byte{[]byte(`{"x":1}`)})))
	// Multi-arg ack: each arg is comma-separated JSON inside the array.
	require.Equal(t, `431["a","b",{"k":1}]`, string(buildSIOAck(nil, 1, [][]byte{
		[]byte(`"a"`), []byte(`"b"`), []byte(`{"k":1}`),
	})))
}

// TestSocketIOPingTimeoutIsAdvertised checks that the EIO OPEN packet exposes
// the configured PingTimeout, not the deprecated 1s PongTimeout default.
func TestSocketIOPingTimeoutIsAdvertised(t *testing.T) {
	resetSIOGlobals(t)

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	mType, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, mType)
	require.Equal(t, byte(eioOpen), msg[0])

	var open eioOpenPacket
	require.NoError(t, json.Unmarshal(msg[1:], &open))
	require.Equal(t, int(PingInterval.Milliseconds()), open.PingInterval)
	require.Equal(t, int(PingTimeout.Milliseconds()), open.PingTimeout)
	require.GreaterOrEqual(t, open.PingTimeout, 1000,
		"PingTimeout must be at least 1s, got %d ms", open.PingTimeout)
}

// TestSocketIOCloseDoesNotDeadlockOnQuietPeer is the regression test for
// the iteration-1 read-RLock deadlock. Before the fix, Close() (which
// takes kws.mu.Lock) would block forever waiting for the read goroutine
// to release its RLock'd ReadMessage call. Verify that Close completes
// even when the peer sends nothing.
func TestSocketIOCloseDoesNotDeadlockOnQuietPeer(t *testing.T) {
	resetSIOGlobals(t)

	ready := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case ready <- p.Kws:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	var kws *Websocket
	select {
	case kws = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("no EventConnect")
	}

	// From a goroutine that does NOT match the read goroutine, call
	// Close. With the deadlock in place this would hang forever; with
	// the fix it completes within milliseconds.
	closed := make(chan struct{})
	go func() {
		kws.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close() deadlocked when peer was quiet")
	}
}

// TestSocketIOMultipleListenersShareAckGuard verifies that two listeners
// for the same event both calling payload.Ack(...) produce only ONE "43"
// frame on the wire (the second call returns ErrAckAlreadySent).
func TestSocketIOMultipleListenersShareAckGuard(t *testing.T) {
	resetSIOGlobals(t)

	first := make(chan error, 1)
	second := make(chan error, 1)

	On(EventMessage, func(ep *EventPayload) {
		first <- ep.Ack([]byte(`"first"`))
	})
	On(EventMessage, func(ep *EventPayload) {
		second <- ep.Ack([]byte(`"second"`))
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`421["message",{"k":"v"}]`)))

	// Exactly one ack must reach the wire.
	tp, msg, err := sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, tp)
	require.Truef(t,
		string(msg) == `431["first"]` || string(msg) == `431["second"]`,
		"expected one of the listeners' acks, got %q", msg,
	)

	// And the other listener must have been told the ack was already sent.
	collect := func() []error {
		var errs []error
		for i := 0; i < 2; i++ {
			select {
			case e := <-first:
				errs = append(errs, e)
			case e := <-second:
				errs = append(errs, e)
			case <-time.After(2 * time.Second):
				t.Fatal("listeners did not finish")
			}
		}
		return errs
	}
	errs := collect()
	var nilCount, alreadyCount int
	for _, e := range errs {
		switch {
		case e == nil:
			nilCount++
		case errors.Is(e, ErrAckAlreadySent):
			alreadyCount++
		default:
			t.Fatalf("unexpected ack error: %v", e)
		}
	}
	require.Equal(t, 1, nilCount, "exactly one Ack call should succeed")
	require.Equal(t, 1, alreadyCount, "the other Ack call should report ErrAckAlreadySent")
}

// TestSocketIOEmitWithAckTimeout verifies that EmitWithAckTimeout fires
// the callback with ErrAckTimeout when the client never replies.
func TestSocketIOEmitWithAckTimeout(t *testing.T) {
	resetSIOGlobals(t)

	ready := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case ready <- p.Kws:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	var kws *Websocket
	select {
	case kws = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("no EventConnect")
	}

	type ackResult struct {
		data []byte
		err  error
	}
	got := make(chan ackResult, 1)
	kws.EmitWithAckTimeout("ping", []byte(`"hi"`), 100*time.Millisecond, func(ack []byte, err error) {
		got <- ackResult{ack, err}
	})

	// Read the outgoing event so the conn buffer does not back up.
	_, _, _ = sioReadSkipPings(conn)

	select {
	case r := <-got:
		require.Nil(t, r.data)
		require.ErrorIs(t, r.err, ErrAckTimeout)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout callback never fired")
	}
}

// TestSocketIOEmitWithAckDisconnectDelivered verifies that pending ack
// callbacks fire with ErrAckDisconnected when the connection closes
// before the client replies.
func TestSocketIOEmitWithAckDisconnectDelivered(t *testing.T) {
	resetSIOGlobals(t)

	ready := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case ready <- p.Kws:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	require.NoError(t, sioHandshake(t, conn))

	var kws *Websocket
	select {
	case kws = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("no EventConnect")
	}

	type ackResult struct {
		data []byte
		err  error
	}
	got := make(chan ackResult, 1)
	kws.EmitWithAckTimeout("ping", []byte(`"hi"`), 0, func(ack []byte, err error) {
		got <- ackResult{ack, err}
	})

	// Drain the emit so the wire is clean, then close.
	_, _, _ = sioReadSkipPings(conn)
	conn.Close()

	select {
	case r := <-got:
		require.Nil(t, r.data)
		require.ErrorIs(t, r.err, ErrAckDisconnected)
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect callback never fired")
	}
}

// TestSocketIOEventPayloadAck_DoubleSend verifies a single listener
// calling Ack twice gets ErrAckAlreadySent on the second call.
func TestSocketIOEventPayloadAck_DoubleSend(t *testing.T) {
	resetSIOGlobals(t)

	results := make(chan []error, 1)
	On(EventMessage, func(ep *EventPayload) {
		first := ep.Ack([]byte(`"a"`))
		second := ep.Ack([]byte(`"b"`))
		results <- []error{first, second}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`421["message",{"k":"v"}]`)))

	select {
	case errs := <-results:
		require.NoError(t, errs[0])
		require.ErrorIs(t, errs[1], ErrAckAlreadySent)
	case <-time.After(2 * time.Second):
		t.Fatal("listener never called Ack twice")
	}
}

// TestSocketIOMultiArgEvent verifies that an inbound event with multiple
// args is exposed as Args [][]byte and that Data still carries Args[0].
func TestSocketIOMultiArgEvent(t *testing.T) {
	resetSIOGlobals(t)

	got := make(chan *EventPayload, 1)
	On("multi", func(ep *EventPayload) {
		got <- ep
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	// Client emits multi(["a","b",{"k":1}]).
	require.NoError(t, conn.WriteMessage(websocket.TextMessage,
		[]byte(`42["multi","a","b",{"k":1}]`)))

	select {
	case ep := <-got:
		require.Len(t, ep.Args, 3)
		require.Equal(t, `"a"`, string(ep.Args[0]))
		require.Equal(t, `"b"`, string(ep.Args[1]))
		require.Equal(t, `{"k":1}`, string(ep.Args[2]))
		// Backwards compat: Data == Args[0].
		require.Equal(t, `"a"`, string(ep.Data))
	case <-time.After(2 * time.Second):
		t.Fatal("multi-arg event not delivered")
	}
}

// TestSocketIOEmitArgs verifies the multi-arg outbound API produces
// the canonical wire frame.
func TestSocketIOEmitArgs(t *testing.T) {
	resetSIOGlobals(t)

	ready := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case ready <- p.Kws:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	var kws *Websocket
	select {
	case kws = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("no EventConnect")
	}

	kws.EmitArgs("update", []byte(`1`), []byte(`"two"`), []byte(`{"x":3}`))

	tp, msg, err := sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, tp)
	require.Equal(t, `42["update",1,"two",{"x":3}]`, string(msg))
}

// TestSocketIOAckMultiArg verifies that EventPayload.Ack accepts
// multiple args and produces a canonical 43[...] frame.
func TestSocketIOAckMultiArg(t *testing.T) {
	resetSIOGlobals(t)

	On(EventMessage, func(ep *EventPayload) {
		_ = ep.Ack([]byte(`"ok"`), []byte(`42`), []byte(`{"k":"v"}`))
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	require.NoError(t, conn.WriteMessage(websocket.TextMessage,
		[]byte(`421["message",{"q":1}]`)))

	tp, msg, err := sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, tp)
	require.Equal(t, `431["ok",42,{"k":"v"}]`, string(msg))
}

// TestSocketIOListenerPanicRecovered verifies that a panic in one
// listener is recovered, surfaced as EventError, and does NOT kill the
// connection or starve subsequent listeners.
func TestSocketIOListenerPanicRecovered(t *testing.T) {
	resetSIOGlobals(t)

	calledSecond := make(chan struct{}, 1)
	gotErr := make(chan error, 1)

	On(EventMessage, func(_ *EventPayload) {
		panic("boom")
	})
	On(EventMessage, func(_ *EventPayload) {
		select {
		case calledSecond <- struct{}{}:
		default:
		}
	})
	On(EventError, func(ep *EventPayload) {
		select {
		case gotErr <- ep.Error:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	require.NoError(t, conn.WriteMessage(websocket.TextMessage,
		[]byte(`42["message",{"k":"v"}]`)))

	// Second listener still fires.
	select {
	case <-calledSecond:
	case <-time.After(2 * time.Second):
		t.Fatal("second listener was not called after first panicked")
	}
	// EventError carries the panic value.
	select {
	case e := <-gotErr:
		require.Error(t, e)
		require.Contains(t, e.Error(), "boom")
	case <-time.After(2 * time.Second):
		t.Fatal("EventError for the panic was not fired")
	}
}

// TestSocketIOEIOBatchedFrame verifies that two EIO packets concatenated
// with the 0x1E record separator inside one WebSocket frame are split
// and each dispatched individually.
func TestSocketIOEIOBatchedFrame(t *testing.T) {
	resetSIOGlobals(t)

	first := make(chan []byte, 1)
	second := make(chan []byte, 1)
	On("a", func(ep *EventPayload) {
		select {
		case first <- ep.Data:
		default:
		}
	})
	On("b", func(ep *EventPayload) {
		select {
		case second <- ep.Data:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	// One WS frame, two EIO packets separated by 0x1E.
	batched := []byte(`42["a",1]` + "\x1e" + `42["b",2]`)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, batched))

	select {
	case d := <-first:
		require.Equal(t, "1", string(d))
	case <-time.After(2 * time.Second):
		t.Fatal("first batched event missing")
	}
	select {
	case d := <-second:
		require.Equal(t, "2", string(d))
	case <-time.After(2 * time.Second):
		t.Fatal("second batched event missing")
	}
}

// TestSocketIOReservedNamesRejected verifies that EmitEvent on a
// reserved socket.io lifecycle name surfaces ErrReservedEventName via
// EventError and does NOT enqueue a frame on the wire.
func TestSocketIOReservedNamesRejected(t *testing.T) {
	resetSIOGlobals(t)

	gotErr := make(chan error, 6)
	On(EventError, func(p *EventPayload) {
		if errors.Is(p.Error, ErrReservedEventName) {
			select {
			case gotErr <- p.Error:
			default:
			}
		}
	})
	ready := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case ready <- p.Kws:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	var kws *Websocket
	select {
	case kws = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire")
	}

	// Only wire-level reserved names are blocked. Node EventEmitter
	// internals (disconnecting, newListener, removeListener) are not
	// reserved by socket.io on the wire and must remain emittable by
	// users.
	for _, n := range []string{"connect", "connect_error", "disconnect"} {
		kws.EmitEvent(n, []byte(`{"x":1}`))
	}

	// Read for a short window. Any frame that arrives must NOT carry one
	// of the wire-reserved names. (PINGs are auto-skipped by sioReadSkipPings.)
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		require.Falsef(t,
			strings.Contains(string(msg), `"connect"`) ||
				strings.Contains(string(msg), `"disconnect"`) ||
				strings.Contains(string(msg), `"connect_error"`),
			"reserved name leaked onto wire: %q", msg,
		)
	}

	// At least one EventError must have fired. Bumped from 50ms to 2s:
	// the previous tight bound was an unjustified flake risk on slow CI
	// runners where the listener dispatch may lag a few hundred ms behind
	// the test goroutine.
	select {
	case <-gotErr:
	case <-time.After(2 * time.Second):
		t.Fatal("expected ErrReservedEventName via EventError")
	}
}

// TestSocketIOPackageShutdown verifies the package-level Shutdown(ctx)
// closes every connection in the pool, fires EventDisconnect on each,
// and returns nil within the context deadline.
//
// Uses a real TCP listener (rather than fasthttputil.InmemoryListener)
// because pipeConn.SetReadDeadline does not interrupt an already-blocked
// Read; the netpoll-backed real TCP stack does.
func TestSocketIOPackageShutdown(t *testing.T) {
	resetSIOGlobals(t)

	var disc, connd int32
	On(EventConnect, func(_ *EventPayload) { atomic.AddInt32(&connd, 1) })
	On(EventDisconnect, func(_ *EventPayload) { atomic.AddInt32(&disc, 1) })

	app := fiber.New()
	app.Use(upgradeMiddleware)
	app.Get("/", New(func(_ *Websocket) {}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = app.Listener(ln) }()
	defer func() { _ = app.Shutdown() }()

	const n = 5
	conns := make([]*websocket.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	wsURL := "ws://" + ln.Addr().String() + "/"
	for i := 0; i < n; i++ {
		dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
		c, _, derr := dialer.Dial(wsURL, nil)
		require.NoError(t, derr)
		require.NoError(t, sioHandshake(t, c))
		conns = append(conns, c)
	}

	// Wait until every handshake has fired EventConnect.
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&connd) == int32(n)
	}, 2*time.Second, 10*time.Millisecond,
		"only %d/%d connections completed handshake", atomic.LoadInt32(&connd), n)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, Shutdown(ctx))

	// Each connection produced exactly one EventDisconnect.
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&disc) == int32(n)
	}, 2*time.Second, 10*time.Millisecond,
		"expected %d EventDisconnect, got %d", n, atomic.LoadInt32(&disc))
}

// TestSocketIOInvalidNamespaceRejected verifies that a CONNECT with a
// malformed namespace receives a "44" CONNECT_ERROR and the connection
// is closed by the server.
func TestSocketIOInvalidNamespaceRejected(t *testing.T) {
	resetSIOGlobals(t)

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	// Read EIO OPEN so the server is in handshake-wait state.
	_, _, err := conn.ReadMessage()
	require.NoError(t, err)

	// CONNECT to a namespace containing a forbidden character (',' would
	// be the framing delimiter; spaces are also rejected by our charset).
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("40/bad ns,")))

	// Server should send 44/bad ns,{...} and close.
	for {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		mType, msg, rerr := conn.ReadMessage()
		if rerr != nil {
			// Closure is acceptable.
			return
		}
		if mType == websocket.TextMessage && len(msg) >= 2 && msg[0] == eioMessage && msg[1] == sioConnectError {
			require.Truef(t, strings.Contains(string(msg), "invalid namespace"),
				"expected CONNECT_ERROR with 'invalid namespace', got %q", msg,
			)
			return
		}
	}
}

// TestSocketIOServerDisconnectNamespacedComma verifies that Close() on a
// connection bound to a namespace emits "41/<ns>," with the trailing comma
// required by socket.io-protocol v5. Without the comma, strict parsers
// (including socket.io-client when DEBUG is enabled) reject the frame.
func TestSocketIOServerDisconnectNamespacedComma(t *testing.T) {
	resetSIOGlobals(t)

	kwsCh := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case kwsCh <- p.Kws:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	// EIO OPEN
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read open: %v", err)
	}
	// CONNECT to /admin
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("40/admin,")))
	// CONNECT ACK
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read connect ack: %v", err)
	}

	var kws *Websocket
	select {
	case kws = <-kwsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire")
	}

	// Server-initiated close must emit "41/admin," with trailing comma.
	go kws.Close()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		mType, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("expected SIO DISCONNECT before close, got err=%v", err)
		}
		if mType == websocket.TextMessage && len(msg) >= 2 && msg[0] == eioMessage && msg[1] == sioDisconnect {
			require.Equal(t, "41/admin,", string(msg))
			return
		}
	}
}

// TestSocketIOClientReservedEventRejected verifies that a client-emitted
// reserved lifecycle event (e.g. "connect") is dropped with EventError
// instead of being dispatched to user listeners (which would otherwise
// double-fire EventConnect handlers).
func TestSocketIOClientReservedEventRejected(t *testing.T) {
	resetSIOGlobals(t)

	connectFires := atomic.Int32{}
	On(EventConnect, func(_ *EventPayload) { connectFires.Add(1) })
	errCh := make(chan error, 1)
	On(EventError, func(p *EventPayload) {
		select {
		case errCh <- p.Error:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	require.NoError(t, sioHandshake(t, conn))

	// Hostile client emits a reserved event name.
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`42["connect","x"]`)))

	select {
	case err := <-errCh:
		require.ErrorContains(t, err, "reserved event")
	case <-time.After(2 * time.Second):
		t.Fatal("expected EventError for reserved client emit")
	}
	// EventConnect must have fired exactly once (from the framework, not
	// twice from the malicious client emit).
	require.Equal(t, int32(1), connectFires.Load())
}

// TestSocketIOBatchedFrameOverflow verifies that a malicious frame composed
// of more than MaxBatchPackets record separators surfaces an EventError
// instead of allocating a multi-megabyte slice header.
func TestSocketIOBatchedFrameOverflow(t *testing.T) {
	resetSIOGlobals(t)

	errCh := make(chan error, 1)
	On(EventError, func(p *EventPayload) {
		select {
		case errCh <- p.Error:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	require.NoError(t, sioHandshake(t, conn))

	// Build a batched frame of (MaxBatchPackets+5) tiny packets.
	frame := make([]byte, 0, MaxBatchPackets*4)
	for i := 0; i < MaxBatchPackets+5; i++ {
		if i > 0 {
			frame = append(frame, 0x1E)
		}
		frame = append(frame, '2') // EIO PING (cheap, ignored by handler)
	}
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, frame))

	select {
	case err := <-errCh:
		require.ErrorContains(t, err, "MaxBatchPackets")
	case <-time.After(2 * time.Second):
		t.Fatal("expected EventError for over-batched frame")
	}
}

// TestSocketIONoGoroutineLeak verifies that the per-connection goroutines
// (send / pong / read), the Shutdown drain goroutine, and any ack
// time.AfterFunc callbacks do not leak after the connection pool has been
// drained.
//
// Strategy:
//  1. Snapshot runtime.NumGoroutine() as a baseline AFTER the test server is
//     up (so the server's accept goroutines are part of the baseline and we
//     only measure per-connection goroutines).
//  2. Open 100 connections, perform the EIO/SIO handshake on each, send 10
//     events per connection, then close them client-side.
//  3. Call package-level Shutdown(ctx) with a 5s deadline to tear the pool
//     down deterministically.
//  4. Poll up to 5s for the live goroutine count to settle within +2 of the
//     baseline.
//  5. On failure dump runtime.Stack(buf, true) so the offending goroutine
//     is identifiable from its stack frames.
func TestSocketIONoGoroutineLeak(t *testing.T) {
	resetSIOGlobals(t)

	const numConns = 100
	const eventsPerConn = 10

	// Real TCP listener: pipe-backed listeners do not honour SetReadDeadline,
	// which the read goroutine relies on to break out of ReadMessage during
	// teardown. Without that the leak detector would always flag a "false"
	// leak that is really just a stuck unit-test transport.
	app := fiber.New()
	app.Use(upgradeMiddleware)
	app.Get("/", New(func(_ *Websocket) {}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = app.Listener(ln) }()
	defer func() { _ = app.Shutdown() }()

	wsURL := "ws://" + ln.Addr().String() + "/"

	// Give Fiber/fasthttp a beat to spin up its accept goroutines so they
	// land in the baseline rather than the diff.
	require.Eventually(t, func() bool {
		c, derr := net.DialTimeout("tcp", ln.Addr().String(), 200*time.Millisecond)
		if derr != nil {
			return false
		}
		_ = c.Close()
		return true
	}, 2*time.Second, 10*time.Millisecond, "listener never accepted")

	// Flush any stragglers, then snapshot. Replace the previous one-shot
	// 50ms sleep with a stability check: wait until runtime.NumGoroutine
	// reports the SAME value across three consecutive GC cycles spaced
	// 20ms apart. This is robust on slow runners where 50ms may be
	// insufficient for the listener accept goroutines to settle.
	stableBaseline := func() int {
		deadline := time.Now().Add(2 * time.Second)
		var prev1, prev2, cur int
		for time.Now().Before(deadline) {
			runtime.GC()
			cur = runtime.NumGoroutine()
			if prev1 == cur && prev2 == cur && cur > 0 {
				return cur
			}
			prev2 = prev1
			prev1 = cur
			time.Sleep(20 * time.Millisecond)
		}
		return cur
	}
	baseline := stableBaseline()

	// 1) Open 100 connections + handshake.
	conns := make([]*websocket.Conn, 0, numConns)
	for i := 0; i < numConns; i++ {
		dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
		c, _, derr := dialer.Dial(wsURL, nil)
		require.NoErrorf(t, derr, "dial %d", i)
		require.NoErrorf(t, sioHandshake(t, c), "handshake %d", i)
		conns = append(conns, c)
	}

	// 2) 10 events per connection.
	for i, c := range conns {
		for j := 0; j < eventsPerConn; j++ {
			frame := []byte(`42["leakprobe",{"i":` + strconv.Itoa(i) + `,"j":` + strconv.Itoa(j) + `}]`)
			require.NoError(t, c.WriteMessage(websocket.TextMessage, frame))
		}
	}

	// 3) Close client-side.
	for _, c := range conns {
		_ = c.Close()
	}

	// 4) Drain the pool deterministically.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, Shutdown(shutdownCtx))

	// 5) Poll up to 5s for the goroutine count to settle within +2 of baseline.
	const tolerance = 2
	deadline := time.Now().Add(5 * time.Second)
	var current int
	for time.Now().Before(deadline) {
		runtime.GC()
		current = runtime.NumGoroutine()
		if current <= baseline+tolerance {
			return // clean exit
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Did not settle. Dump every goroutine's stack so the leaker is named.
	buf := make([]byte, 1<<20) // 1 MiB
	n := runtime.Stack(buf, true)
	t.Fatalf("goroutine leak: baseline=%d, current=%d (delta=%d, tolerance=%d)\n=== full goroutine dump ===\n%s",
		baseline, current, current-baseline, tolerance, buf[:n])
}

// TestExtractSIOConnectAuthEdgeCases pins the current behaviour of the
// SIO CONNECT parser for the bytes following the "40" type prefix.
//
// The wire format per socket.io-protocol v5 is:
//
//	[ "/" namespace "," ] [ <json_auth_object> ]
//
// Notes on observed (current) behaviour, documented here for fuzz coverage:
//   - The parser splits on the first ',' to separate the namespace from the
//     auth payload. It does NOT validate that auth is a JSON value, let alone
//     a JSON OBJECT (which the spec mandates). See "non-object auth" cases
//     below: arrays, garbage strings, and truncated JSON all flow through
//     verbatim. Validation/rejection is the handshake layer's responsibility,
//     not this lexer.
//   - "40/admin" (namespace, no comma) currently returns ns=`/admin`, auth=nil.
//   - "40banana" (no leading slash) is treated as a root-namespace auth blob
//     and currently returns ns=nil, auth=`banana`.
func TestExtractSIOConnectAuthEdgeCases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		in       string // bytes AFTER the "40" prefix
		wantNS   string // "" means nil
		wantAuth string // "" means nil
		// note documents intentional/known-quirk behaviour we are pinning,
		// not endorsing.
		note string
	}{
		{
			name:   "root_no_auth",
			in:     "",
			wantNS: "", wantAuth: "",
		},
		{
			name:   "root_with_object_auth",
			in:     `{"token":"x"}`,
			wantNS: "", wantAuth: `{"token":"x"}`,
		},
		{
			name:   "namespaced_no_auth",
			in:     "/admin,",
			wantNS: "/admin", wantAuth: "",
		},
		{
			name:   "namespaced_with_object_auth",
			in:     `/admin,{"token":"x"}`,
			wantNS: "/admin", wantAuth: `{"token":"x"}`,
		},
		{
			// QUIRK: "/admin" without a trailing comma is interpreted as a
			// namespace with no auth. Auth is nil. Pin this so any future
			// reinterpretation (e.g. treating it as malformed) trips the test.
			name:   "namespace_without_trailing_comma",
			in:     "/admin",
			wantNS: "/admin", wantAuth: "",
			note: "no comma, no auth: ns='/admin', auth=nil",
		},
		{
			// QUIRK: bytes that don't start with '/' are taken as a root-ns
			// auth blob without ANY structural validation. The handshake
			// layer is expected to reject this when it tries to decode JSON,
			// but the lexer happily forwards it.
			name:   "garbage_root_auth_not_json",
			in:     "banana",
			wantNS: "", wantAuth: "banana",
			note: "non-JSON root auth currently accepted by parser",
		},
		{
			// QUIRK: namespace + non-JSON auth is also forwarded verbatim.
			name:   "namespaced_garbage_auth",
			in:     "/admin,banana",
			wantNS: "/admin", wantAuth: "banana",
			note: "non-JSON namespaced auth currently accepted by parser",
		},
		{
			// QUIRK: truncated JSON object is forwarded verbatim. The
			// handshake layer must defend against this at decode time.
			name:   "truncated_object_open_brace",
			in:     "{",
			wantNS: "", wantAuth: "{",
			note: "truncated JSON forwarded verbatim",
		},
		{
			name:   "truncated_mid_field",
			in:     `{"token":`,
			wantNS: "", wantAuth: `{"token":`,
			note: "truncated mid-field JSON forwarded verbatim",
		},
		{
			// SPEC VIOLATION (documented, not fixed): per socket.io-protocol
			// v5 the auth payload MUST be a JSON OBJECT. The current parser
			// also accepts JSON arrays (and any other JSON value) because it
			// performs no JSON-shape validation. Pin the current behaviour.
			name:   "json_array_as_auth_spec_violation",
			in:     "[1,2,3]",
			wantNS: "", wantAuth: "[1,2,3]",
			note: "spec says auth MUST be object; parser accepts arrays",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ns, auth := extractSIOConnect([]byte(tc.in))

			if tc.wantNS == "" {
				require.Nil(t, ns, "namespace: input=%q note=%s", tc.in, tc.note)
			} else {
				require.Equal(t, tc.wantNS, string(ns),
					"namespace mismatch: input=%q note=%s", tc.in, tc.note)
			}
			if tc.wantAuth == "" {
				require.Nil(t, auth, "auth: input=%q note=%s", tc.in, tc.note)
			} else {
				require.Equal(t, tc.wantAuth, string(auth),
					"auth mismatch: input=%q note=%s", tc.in, tc.note)
			}
		})
	}
}

// TestSocketIOMalformedAuthRejected sends a CONNECT packet with a truncated
// JSON auth payload (`40{"unclosed`) and verifies that the server rejects
// the handshake with a CONNECT_ERROR ("44") frame (and/or closes the
// connection). The hardened handshake validates the auth payload via
// isValidAuthPayload before storing it on kws.handshakeAuth.
func TestSocketIOMalformedAuthRejected(t *testing.T) {
	resetSIOGlobals(t)

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	// Drain EIO OPEN.
	_, _, err := conn.ReadMessage()
	require.NoError(t, err)

	// Truncated JSON auth, root namespace.
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`40{"unclosed`)))

	assertConnectErrorOrClose(t, conn, "malformed auth")
}

// TestSocketIOAuthArrayRejected sends a CONNECT packet whose auth payload is
// a JSON array (`40[1,2,3]`). Per socket.io-protocol v5 the auth field MUST
// be a JSON object, so the server must reply with CONNECT_ERROR (or close).
func TestSocketIOAuthArrayRejected(t *testing.T) {
	resetSIOGlobals(t)

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	_, _, err := conn.ReadMessage()
	require.NoError(t, err)

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`40[1,2,3]`)))

	assertConnectErrorOrClose(t, conn, "array auth")
}

// TestSocketIOAuthOversizeRejected sends a CONNECT packet whose auth payload
// is a syntactically valid JSON object but exceeds MaxAuthPayload by 1 byte.
// The server must reject the handshake with CONNECT_ERROR (or close).
func TestSocketIOAuthOversizeRejected(t *testing.T) {
	resetSIOGlobals(t)

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	_, _, err := conn.ReadMessage()
	require.NoError(t, err)

	// Build `{"token":"<filler>"}` so the total auth payload length is
	// exactly MaxAuthPayload + 1 bytes (one byte over the cap).
	prefix := `{"token":"`
	suffix := `"}`
	overhead := len(prefix) + len(suffix)
	fillerLen := MaxAuthPayload + 1 - overhead
	require.Greater(t, fillerLen, 0)
	filler := bytes.Repeat([]byte{'a'}, fillerLen)
	auth := append([]byte(prefix), filler...)
	auth = append(auth, []byte(suffix)...)
	require.Equal(t, MaxAuthPayload+1, len(auth))

	frame := append([]byte("40"), auth...)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, frame))

	assertConnectErrorOrClose(t, conn, "oversize auth")
}

// assertConnectErrorOrClose reads one frame from conn and asserts that the
// server either (a) closed the connection or (b) emitted a Socket.IO
// CONNECT_ERROR ("44") frame. Anything else (notably a CONNECT_ACK) is a
// regression of the auth-payload validation and fails the test.
func assertConnectErrorOrClose(t *testing.T, conn *websocket.Conn, label string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	mType, msg, rerr := conn.ReadMessage()

	switch {
	case rerr != nil:
		t.Logf("%s: server closed connection: %v", label, rerr)
	case mType == websocket.TextMessage && len(msg) >= 2 && msg[0] == eioMessage && msg[1] == sioConnectError:
		t.Logf("%s: server sent CONNECT_ERROR: %q", label, msg)
	default:
		t.Fatalf("%s: expected CONNECT_ERROR or close, got type=%d payload=%q", label, mType, msg)
	}
}

// TestSocketIOListenerCoWConsistency stress-tests the safeListeners CoW
// registry. 8 writer goroutines call On() concurrently while 8 firer
// goroutines call kws.fireEvent(). Under -race we expect:
//   - no panic, no data race
//   - every callback invocation is accounted for via an atomic counter
//     (no "missed" firings beyond the natural CoW visibility window and
//     no double-firings: each fire delivers to exactly the snapshot len)
func TestSocketIOListenerCoWConsistency(t *testing.T) {
	resetSIOGlobals(t)

	kws := createWS()
	pool.set(kws)

	const numEvents = 4
	eventNames := [numEvents]string{"cow_evt_0", "cow_evt_1", "cow_evt_2", "cow_evt_3"}

	var invocations atomic.Int64
	cb := func(_ *EventPayload) {
		invocations.Add(1)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 8 writer goroutines: each registers listeners on random events.
	// Use a deterministic per-goroutine xorshift so we don't depend on
	// math/rand seeding.
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			x := seed | 1
			for {
				select {
				case <-stop:
					return
				default:
				}
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				On(eventNames[x%numEvents], cb)
			}
		}(uint64(w + 1))
	}

	// 8 firer goroutines: each fires an event and counts how many
	// listeners it OBSERVED at fire time, by comparing the registry
	// snapshot length to the post-fire invocation delta.
	var fires atomic.Int64
	var minExpected atomic.Int64 // sum of pre-fire snapshot sizes (lower bound)
	for f := 0; f < 8; f++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			x := seed | 1
			for {
				select {
				case <-stop:
					return
				default:
				}
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				ev := eventNames[x%numEvents]
				snapshot := listeners.get(ev)
				expected := int64(len(snapshot))
				kws.fireEvent(ev, []byte("d"), nil)
				fires.Add(1)
				minExpected.Add(expected)
			}
		}(uint64(100 + f))
	}

	time.Sleep(1 * time.Second)
	close(stop)
	wg.Wait()

	got := invocations.Load()
	low := minExpected.Load()
	t.Logf("fires=%d invocations=%d minExpected=%d", fires.Load(), got, low)
	require.Greater(t, fires.Load(), int64(0))
	// CoW invariant: fireEvent loads its own snapshot AFTER our
	// pre-fire load, so invocations must be >= the lower bound we
	// recorded. A regression that truncates dispatch (e.g. ranging
	// over a mutated slice) would make got < low.
	require.GreaterOrEqual(t, got, low,
		"missed firings: invocations < pre-fire snapshot sum")
	// Sanity upper bound: each fire dispatches at most numListeners
	// across all events. Total registrations is bounded by writer
	// count over the second; if we observed double-firing, got would
	// exceed this generous cap (effectively unbounded vs. fires *
	// total registrations).
	totalRegistrations := int64(0)
	for _, ev := range eventNames {
		totalRegistrations += int64(len(listeners.get(ev)))
	}
	require.LessOrEqual(t, got, fires.Load()*totalRegistrations,
		"double-firing suspected: invocations exceed fires * total registered listeners")
}

// TestSocketIOListenersResetUnderFire verifies that a listeners.reset()
// performed concurrently with an in-flight fireEvent does NOT race with
// nor truncate the dispatch loop: fireEvent reads the snapshot once and
// iterates it locally, so reset() only affects subsequent fires.
func TestSocketIOListenersResetUnderFire(t *testing.T) {
	resetSIOGlobals(t)

	kws := createWS()
	pool.set(kws)

	const numListeners = 6
	var entered atomic.Int32
	var completed atomic.Int32
	gate := make(chan struct{})
	resetDone := make(chan struct{})

	for i := 0; i < numListeners; i++ {
		idx := i
		On("reset_evt", func(_ *EventPayload) {
			entered.Add(1)
			if idx == 0 {
				// First listener blocks until reset() has fired,
				// guaranteeing the in-flight dispatch overlaps
				// with the reset.
				<-gate
				<-resetDone
			}
			completed.Add(1)
		})
	}

	var fireWG sync.WaitGroup
	fireWG.Add(1)
	go func() {
		defer fireWG.Done()
		kws.fireEvent("reset_evt", []byte("payload"), nil)
	}()

	// Wait for the dispatch loop to begin, then trigger reset()
	// from a separate goroutine while listener 0 is still blocked.
	require.Eventually(t, func() bool { return entered.Load() >= 1 },
		1*time.Second, 1*time.Millisecond, "first listener never entered")

	go func() {
		listeners.reset()
		close(resetDone)
	}()

	// Wait until the reset has actually completed (channel close) before
	// unblocking listener 0. That removes the prior 20ms sleep, which was
	// non-deterministic on slow runners and could let listener 0 unblock
	// BEFORE reset() acquired its lock, defeating the test's intent.
	<-resetDone
	close(gate)

	fireWG.Wait()

	require.Equal(t, int32(numListeners), completed.Load(),
		"all listeners on the OLD snapshot must complete despite reset()")

	// And a fresh fireEvent after reset() must see ZERO listeners.
	var post atomic.Int32
	On("reset_evt_post", func(_ *EventPayload) { post.Add(1) }) // unrelated event
	kws.fireEvent("reset_evt", []byte("again"), nil)
	require.Equal(t, int32(numListeners), completed.Load(),
		"post-reset fire must NOT invoke any old listener")
}

// TestSocketIOMultipleListenersAllFire is the non-ack equivalent of
// TestSocketIOMultipleListenersShareAckGuard: registers 5 listeners on the
// same event and asserts every one is invoked exactly once per fire.
func TestSocketIOMultipleListenersAllFire(t *testing.T) {
	resetSIOGlobals(t)

	kws := createWS()
	pool.set(kws)

	const n = 5
	var counters [n]atomic.Int32
	for i := 0; i < n; i++ {
		idx := i
		On("multi_fire_evt", func(_ *EventPayload) {
			counters[idx].Add(1)
		})
	}

	kws.fireEvent("multi_fire_evt", []byte("once"), nil)

	for i := 0; i < n; i++ {
		require.Equalf(t, int32(1), counters[i].Load(),
			"listener %d invocation count", i)
	}
}

// TestSocketIOOnAfterFireDoesNotAffectInflight verifies the CoW snapshot
// semantic: a listener that registers a NEW listener for the same event
// during dispatch must NOT have the new listener fire for the in-flight
// event, but the new listener MUST fire on the NEXT event.
func TestSocketIOOnAfterFireDoesNotAffectInflight(t *testing.T) {
	resetSIOGlobals(t)

	kws := createWS()
	pool.set(kws)

	var firstCalls atomic.Int32
	var newListenerCalls atomic.Int32
	var registered atomic.Bool

	On("inflight_evt", func(_ *EventPayload) {
		firstCalls.Add(1)
		// Register the new listener exactly once, during the very
		// first dispatch. fireEvent has already taken its snapshot,
		// so the new listener must not appear in this dispatch.
		if registered.CompareAndSwap(false, true) {
			On("inflight_evt", func(_ *EventPayload) {
				newListenerCalls.Add(1)
			})
		}
	})

	// First fire: snapshot has 1 listener.
	kws.fireEvent("inflight_evt", []byte("a"), nil)
	require.Equal(t, int32(1), firstCalls.Load())
	require.Equal(t, int32(0), newListenerCalls.Load(),
		"newly-registered listener must NOT fire for in-flight event (CoW)")

	// Second fire: snapshot now has 2 listeners.
	kws.fireEvent("inflight_evt", []byte("b"), nil)
	require.Equal(t, int32(2), firstCalls.Load())
	require.Equal(t, int32(1), newListenerCalls.Load(),
		"newly-registered listener MUST fire for next event")
}

// TestSocketIOConcurrentEmitClose stress-tests concurrent
// Broadcast / Close / Shutdown / On registration interactions.
//
//   - 50 connections live in the pool.
//   - 10 broadcaster goroutines push at ~100/s for 2s.
//   - 5 closer goroutines randomly Close() connections.
//   - 1 listener-churn goroutine register/deregisters listeners via On().
//   - After 2s, emitters stop and Shutdown(ctx) is invoked.
//
// Verifies: no panic, no deadlock, Shutdown returns within deadline,
// every connection ends up gone from the pool.
//
// Run under -race to surface any latent data race.
func TestSocketIOConcurrentEmitClose(t *testing.T) {
	// Iteration-2 race fixed in socketio.go:
	//   - Close() now gates its kws.mu-protected write block behind a
	//     dedicated closeOnce so concurrent callers cannot double-write.
	//   - run() now performs a kws.mu.Lock()/Unlock() barrier after
	//     workersWg.Wait() so any in-flight Close() writer has finished
	//     before the upgrade handler returns and the vendored websocket
	//     package's deferred releaseConn() nils the embedded *fasthttp.Conn.
	//   - Close() skips direct Conn writes once handlerDone marks that the
	//     upgrade handler is returning and releaseConn may nil the embedded
	//     *fasthttp.Conn without taking kws.mu.
	resetSIOGlobals(t)

	// Stable connect listener: stash kws handles as connections come in.
	kwsCh := make(chan *Websocket, 64)
	On(EventConnect, func(p *EventPayload) {
		select {
		case kwsCh <- p.Kws:
		default:
		}
	})
	// Drop EventError to avoid noise; the broadcaster fires it on dead UUIDs.
	On(EventError, func(_ *EventPayload) {})
	On(EventDisconnect, func(_ *EventPayload) {})

	app := fiber.New()
	app.Use(upgradeMiddleware)
	app.Get("/", New(func(_ *Websocket) {}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = app.Listener(ln) }()
	defer func() { _ = app.Shutdown() }()

	const numConns = 50
	wsURL := "ws://" + ln.Addr().String() + "/"

	clientConns := make([]*websocket.Conn, 0, numConns)
	defer func() {
		for _, c := range clientConns {
			_ = c.Close()
		}
	}()

	// Spawn 50 connections through the full socket.io handshake.
	for i := 0; i < numConns; i++ {
		dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
		c, _, derr := dialer.Dial(wsURL, nil)
		require.NoError(t, derr)
		require.NoError(t, sioHandshake(t, c))
		clientConns = append(clientConns, c)
		// Drain inbound frames so the read buffer never blocks the server.
		go func(cc *websocket.Conn) {
			for {
				if _, _, rerr := cc.ReadMessage(); rerr != nil {
					return
				}
			}
		}(c)
	}

	// Wait until at least most connections registered into the pool.
	require.Eventually(t, func() bool {
		return len(pool.all()) >= numConns/2
	}, 3*time.Second, 10*time.Millisecond, "pool never populated")

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 10 broadcasters at ~100 frames/s.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			payload := []byte(`"stress"`)
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					Broadcast(payload, TextMessage)
					if all := pool.all(); len(all) > 0 {
						for uuid := range all {
							_ = EmitTo(uuid, payload, TextMessage)
							break
						}
					}
				}
			}
		}(i)
	}

	// 5 closers: pick a live connection and Close() it.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					all := pool.all()
					for _, w := range all {
						if k, ok := w.(*Websocket); ok {
							k.Close() // safe: idempotent via sync.Once
							break
						}
					}
				}
			}
		}()
	}

	// 1 listener-churn goroutine: register fresh listeners and reset.
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		var n int
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				n++
				name := "stress-" + strconv.Itoa(n%8)
				On(name, func(_ *EventPayload) {})
				if n%50 == 0 {
					listeners.reset()
				}
			}
		}
	}()

	// Drain kwsCh in the background so EventConnect handler never blocks.
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-kwsCh:
			}
		}
	}()

	// Run for 2 seconds.
	time.Sleep(2 * time.Second)
	close(stop)
	wg.Wait()

	// Shutdown with a generous deadline; failure here means deadlock.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, Shutdown(ctx), "Shutdown did not complete within deadline")

	// Verify pool drained.
	require.Eventually(t, func() bool {
		return len(pool.all()) == 0
	}, 3*time.Second, 20*time.Millisecond,
		"pool not drained after Shutdown: %d entries remain", len(pool.all()))
}

// TestSocketIOEmitToVanishedConn opens a connection, has another goroutine
// abruptly drop the underlying WebSocket (network drop simulation), and
// expects EmitTo on that UUID to surface ErrorInvalidConnection without
// panicking or blocking.
func TestSocketIOEmitToVanishedConn(t *testing.T) {
	resetSIOGlobals(t)

	kwsCh := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case kwsCh <- p.Kws:
		default:
		}
	})
	On(EventError, func(_ *EventPayload) {})
	On(EventDisconnect, func(_ *EventPayload) {})

	app := fiber.New()
	app.Use(upgradeMiddleware)
	app.Get("/", New(func(_ *Websocket) {}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = app.Listener(ln) }()
	defer func() { _ = app.Shutdown() }()

	wsURL := "ws://" + ln.Addr().String() + "/"
	dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	c, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	require.NoError(t, sioHandshake(t, c))

	var kws *Websocket
	select {
	case kws = <-kwsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire")
	}
	uuidA := kws.GetUUID()

	// Goroutine 2: abruptly close A's underlying WS (simulate network drop).
	dropped := make(chan struct{})
	go func() {
		// Yank the underlying TCP conn out from under both peers. This
		// emulates a hard network disconnect rather than a graceful
		// SIO DISCONNECT frame.
		if uc := c.UnderlyingConn(); uc != nil {
			_ = uc.Close()
		}
		_ = c.Close()
		close(dropped)
	}()
	<-dropped

	// Wait until the server-side read goroutine notices the drop and
	// removes the entry from the pool.
	require.Eventually(t, func() bool {
		_, gerr := pool.get(uuidA)
		return errors.Is(gerr, ErrorInvalidConnection)
	}, 3*time.Second, 10*time.Millisecond,
		"server never observed the abrupt drop")

	// Goroutine 3: EmitTo on the vanished UUID. Must not panic, must not
	// block forever, must return ErrorInvalidConnection.
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- errors.New("EmitTo panicked")
			}
		}()
		done <- EmitTo(uuidA, []byte(`"hello"`))
	}()

	select {
	case emitErr := <-done:
		require.ErrorIs(t, emitErr, ErrorInvalidConnection,
			"expected ErrorInvalidConnection, got %v", emitErr)
	case <-time.After(3 * time.Second):
		t.Fatal("EmitTo on vanished connection blocked")
	}
}

// TestSocketIOAckRaceTimeoutVsDelivery hammers the timer-vs-delivery race
// inside deliverOutboundAck/fireAckTimeout. For each of 1000 iterations
// we register a callback with a microsecond-class timeout and concurrently
// drive deliverOutboundAck on the same id. Exactly one of the two paths
// (timeout or delivery) must win because the map-delete-wins guard fences
// both code paths through outboundAcksMu. Any double-fire, missed-fire,
// or panic surfaces as a test failure.
func TestSocketIOAckRaceTimeoutVsDelivery(t *testing.T) {
	resetSIOGlobals(t)

	const iterations = 1000

	// Drive the ack state machine in isolation: no Conn is required because
	// .write() is never called.
	kws := &Websocket{
		queue:        make(chan message, 16),
		done:         make(chan struct{}, 1),
		attributes:   make(map[string]any),
		outboundAcks: make(map[uint64]*pendingAck),
	}
	kws.isAlive.Store(true)
	kws.UUID = uuid.New().String()

	var timeoutWins, deliveryWins atomic.Int32

	for i := 0; i < iterations; i++ {
		var fires atomic.Int32
		var sawTimeout, sawDelivery atomic.Bool
		done := make(chan struct{}, 2)

		// Register the pending ack manually so we control the id. The timer
		// is armed under the same lock that guards the map, mirroring the
		// production EmitWithAckTimeout path so the race-detector can flag
		// any future regression that arms the timer outside the mutex.
		kws.outboundAcksMu.Lock()
		kws.outboundAckSeq++
		id := kws.outboundAckSeq
		p := &pendingAck{cb: func(_ [][]byte, err error) {
			fires.Add(1)
			if err == ErrAckTimeout {
				sawTimeout.Store(true)
			} else if err == nil {
				sawDelivery.Store(true)
			}
			done <- struct{}{}
		}}
		// Microsecond-class timer to maximise the race window.
		p.timer = time.AfterFunc(50*time.Microsecond, func() { kws.fireAckTimeout(id) })
		if kws.outboundAcks == nil {
			kws.outboundAcks = make(map[uint64]*pendingAck)
		}
		kws.outboundAcks[id] = p
		kws.outboundAcksMu.Unlock()

		// Stagger delivery so it lands close to the timer fire.
		spin := i % 32
		for k := 0; k < spin; k++ {
			runtime.Gosched()
		}

		go kws.deliverOutboundAck(id, [][]byte{[]byte(`"x"`)})

		// Wait for at least one fire.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: callback never fired", i)
		}

		// Brief grace window to detect any double fire.
		select {
		case <-done:
			t.Fatalf("iter %d: callback fired twice (race)", i)
		case <-time.After(2 * time.Millisecond):
		}

		require.Equalf(t, int32(1), fires.Load(), "iter %d: expected exactly 1 fire", i)
		if sawTimeout.Load() {
			timeoutWins.Add(1)
		}
		if sawDelivery.Load() {
			deliveryWins.Add(1)
		}

		// Map must be empty between iterations.
		kws.outboundAcksMu.Lock()
		size := len(kws.outboundAcks)
		kws.outboundAcksMu.Unlock()
		require.Equalf(t, 0, size, "iter %d: map not drained", i)
	}

	// Safety property: every iteration must produce exactly one fire (no
	// double-fire, no missed fire). The win distribution between the two
	// arms is scheduler-dependent: on fast hosts the delivery arm wins
	// nearly every iteration, on slow CI the timeout arm wins more often.
	// We log the split for visibility but do not gate on it; the at-most-
	// once invariant is the load-bearing assertion above.
	t.Logf("ack race split: timeout=%d delivery=%d", timeoutWins.Load(), deliveryWins.Load())
}

// TestSocketIOAckRaceDisconnectVsDelivery hammers the third leg of the
// at-most-once invariant: the disconnected() drain racing against
// deliverOutboundAck and fireAckTimeout on the same id. Without the
// pendingAck.fired CAS guard, a timer goroutine that already captured a
// pending entry could double-fire after disconnected() drained the map.
func TestSocketIOAckRaceDisconnectVsDelivery(t *testing.T) {
	resetSIOGlobals(t)

	const iterations = 500

	for i := 0; i < iterations; i++ {
		kws := &Websocket{
			queue:        make(chan message, 16),
			done:         make(chan struct{}, 1),
			attributes:   make(map[string]any),
			outboundAcks: make(map[uint64]*pendingAck),
		}
		kws.isAlive.Store(true)
		kws.UUID = uuid.New().String()

		var fires atomic.Int32
		done := make(chan struct{}, 4)

		kws.outboundAcksMu.Lock()
		kws.outboundAckSeq++
		id := kws.outboundAckSeq
		p := &pendingAck{cb: func(_ [][]byte, _ error) {
			fires.Add(1)
			done <- struct{}{}
		}}
		p.timer = time.AfterFunc(40*time.Microsecond, func() { kws.fireAckTimeout(id) })
		kws.outboundAcks[id] = p
		kws.outboundAcksMu.Unlock()

		// Race three callers against the same pending ack: the timer (already
		// scheduled), an inbound delivery, and the disconnect drain.
		spin := i % 16
		for k := 0; k < spin; k++ {
			runtime.Gosched()
		}
		go kws.deliverOutboundAck(id, [][]byte{[]byte(`"x"`)})
		go kws.disconnected(nil)

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: callback never fired", i)
		}

		select {
		case <-done:
			t.Fatalf("iter %d: callback fired twice (race)", i)
		case <-time.After(3 * time.Millisecond):
		}

		require.Equalf(t, int32(1), fires.Load(), "iter %d: expected exactly 1 fire", i)
	}
}

// TestSocketIOAckUnsolicited verifies that a "43<unknown_id>[]" frame for
// an ack id we never registered is silently dropped: the connection stays
// alive, no panic occurs, and the outboundAcks map does not grow.
func TestSocketIOAckUnsolicited(t *testing.T) {
	resetSIOGlobals(t)

	ready := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case ready <- p.Kws:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	var kws *Websocket
	select {
	case kws = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("no EventConnect")
	}

	// Snapshot map size BEFORE injecting unsolicited acks.
	kws.outboundAcksMu.Lock()
	sizeBefore := len(kws.outboundAcks)
	kws.outboundAcksMu.Unlock()

	// Send a flurry of unsolicited acks for ids that were never registered.
	for _, id := range []string{"1", "999", "424242", "0"} {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage,
			[]byte(`43`+id+`[]`)))
	}
	// And one with a payload.
	require.NoError(t, conn.WriteMessage(websocket.TextMessage,
		[]byte(`4377777["surprise"]`)))

	// Round-trip an EIO PING so we know the read goroutine has processed
	// every frame above before we inspect state.
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte{eioPing}))
	_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	for {
		mType, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mType == websocket.TextMessage && len(msg) == 1 && msg[0] == eioPong {
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{})

	// Connection must still be alive.
	require.True(t, kws.IsAlive(), "conn must survive unsolicited acks")

	// Map must not have grown.
	kws.outboundAcksMu.Lock()
	sizeAfter := len(kws.outboundAcks)
	kws.outboundAcksMu.Unlock()
	require.Equal(t, sizeBefore, sizeAfter,
		"unsolicited ack must not grow outboundAcks map")
}

// TestSocketIOAckCallbackPanicIsolated verifies that a panic inside an
// outbound ack callback is recovered, leaves the connection alive, and
// does not corrupt the ack state machine: a subsequent ack on a DIFFERENT
// id still fires its callback exactly once.
func TestSocketIOAckCallbackPanicIsolated(t *testing.T) {
	resetSIOGlobals(t)

	ready := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case ready <- p.Kws:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	var kws *Websocket
	select {
	case kws = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("no EventConnect")
	}

	panicFired := make(chan struct{}, 1)
	kws.EmitWithAck("first", []byte(`"a"`), func(_ []byte) {
		select {
		case panicFired <- struct{}{}:
		default:
		}
		panic("boom in ack callback")
	})

	// Read the outbound emit (id=1) to keep the wire clean.
	tp, msg, err := sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, tp)
	require.Equal(t, `421["first","a"]`, string(msg))

	// Trigger the panicking callback.
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`431["pong"]`)))

	select {
	case <-panicFired:
	case <-time.After(2 * time.Second):
		t.Fatal("panicking callback never fired")
	}

	// Connection must still be alive after the recovered panic.
	require.True(t, kws.IsAlive(), "conn must survive callback panic")

	// Register a SECOND ack with a different id (id=2) and verify clean dispatch.
	gotSecond := make(chan []byte, 1)
	kws.EmitWithAck("second", []byte(`"b"`), func(ack []byte) {
		gotSecond <- ack
	})

	tp, msg, err = sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, tp)
	require.Equal(t, `422["second","b"]`, string(msg))

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`432["ok"]`)))

	select {
	case ack := <-gotSecond:
		require.Equal(t, `"ok"`, string(ack))
	case <-time.After(2 * time.Second):
		t.Fatal("second ack callback never fired (state corrupted)")
	}
}

// TestSocketIOInboundAckDoubleSendIsHardGuard verifies the wire-level
// invariant: even if the listener calls payload.Ack(...) twice, exactly
// ONE "43" frame reaches the wire (the second Ack returns
// ErrAckAlreadySent). Read the next frame to confirm no second ack is
// queued behind the first.
func TestSocketIOInboundAckDoubleSendIsHardGuard(t *testing.T) {
	resetSIOGlobals(t)

	results := make(chan []error, 1)
	On(EventMessage, func(ep *EventPayload) {
		first := ep.Ack([]byte(`"first"`))
		second := ep.Ack([]byte(`"second"`))
		results <- []error{first, second}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	require.NoError(t, conn.WriteMessage(websocket.TextMessage,
		[]byte(`421["message",{"k":"v"}]`)))

	// First (and only) ack frame.
	tp, msg, err := sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, tp)
	require.Equal(t, `431["first"]`, string(msg))

	// Listener saw exactly one success and one ErrAckAlreadySent.
	select {
	case errs := <-results:
		require.NoError(t, errs[0])
		require.ErrorIs(t, errs[1], ErrAckAlreadySent)
	case <-time.After(2 * time.Second):
		t.Fatal("listener never returned")
	}

	// Read again with a short deadline: NO second ack frame must arrive.
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		_, extra, rerr := conn.ReadMessage()
		if rerr != nil {
			break // deadline hit, expected
		}
		// Pings are fine; anything else is a hard fail.
		if len(extra) == 1 && extra[0] == eioPing {
			continue
		}
		t.Fatalf("unexpected second frame on the wire: %q", extra)
	}
	_ = conn.SetReadDeadline(time.Time{})
}

// TestSocketIOEmitWithAck10000Concurrent registers 10000 outstanding
// pending acks on a single connection and verifies:
//   - every callback fires exactly once during teardown with
//     ErrAckDisconnected,
//   - the outboundAcks map is fully drained after disconnect (no leak),
//   - no double-fire and no panic.
//
// The pending-ack entries are inserted directly into kws.outboundAcks
// (bypassing kws.write -> kws.queue) so we exercise the disconnect drain
// path at high cardinality without overflowing the 100-deep send queue.
// This is the state-machine-level test the brief calls for.
func TestSocketIOEmitWithAck10000Concurrent(t *testing.T) {
	resetSIOGlobals(t)

	ready := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case ready <- p.Kws:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	require.NoError(t, sioHandshake(t, conn))

	var kws *Websocket
	select {
	case kws = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("no EventConnect")
	}

	const N = 10000

	var fires atomic.Int64
	var doubleFires atomic.Int64
	var panicCount atomic.Int64
	var unexpectedErr atomic.Int64

	// Per-callback once-flag slice keeps each fired observation isolated
	// from compiler-loop-variable capture issues.
	flags := make([]*atomic.Bool, N)

	// Build N pending acks WITH timers (30s, well past the test horizon)
	// inserted directly into the registry. Both the timer-fire path and
	// the disconnect drain path race through outboundAcksMu, so registering
	// timers exercises the same map-delete-wins guard used in production.
	cbFor := func(idx int) func(args [][]byte, err error) {
		fired := &atomic.Bool{}
		flags[idx] = fired
		return func(_ [][]byte, err error) {
			defer func() {
				if r := recover(); r != nil {
					panicCount.Add(1)
				}
			}()
			if !fired.CompareAndSwap(false, true) {
				doubleFires.Add(1)
				return
			}
			if !errors.Is(err, ErrAckDisconnected) {
				unexpectedErr.Add(1)
			}
			fires.Add(1)
		}
	}

	for i := 0; i < N; i++ {
		cb := cbFor(i)
		kws.outboundAcksMu.Lock()
		kws.outboundAckSeq++
		id := kws.outboundAckSeq
		p := &pendingAck{cb: cb}
		// outboundAcks is lazy-initialised on first EmitWithAck*; this
		// test bypasses those APIs and reaches into the map directly,
		// so we must init it ourselves on the first iteration.
		if kws.outboundAcks == nil {
			kws.outboundAcks = make(map[uint64]*pendingAck)
		}
		kws.outboundAcks[id] = p
		kws.outboundAcksMu.Unlock()
		// Long timer so disconnect drains BEFORE any timer fires; this
		// confirms the drain path explicitly stops timers and dispatches
		// ErrAckDisconnected exactly once.
		p.timer = time.AfterFunc(30*time.Second, func() { kws.fireAckTimeout(id) })
	}

	// Confirm the map grew to N.
	kws.outboundAcksMu.Lock()
	queued := len(kws.outboundAcks)
	kws.outboundAcksMu.Unlock()
	require.Equal(t, N, queued, "all N ack registrations should be queued")

	// Disconnect: drains every pending callback with ErrAckDisconnected.
	_ = conn.Close()
	kws.Close()

	// Poll: every callback must have fired exactly once.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if fires.Load() == int64(N) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	require.Equalf(t, int64(N), fires.Load(), "expected %d single fires", N)
	require.Equalf(t, int64(0), doubleFires.Load(), "no callback may fire twice")
	require.Equalf(t, int64(0), panicCount.Load(), "no callback may panic")
	require.Equalf(t, int64(0), unexpectedErr.Load(),
		"every callback must observe ErrAckDisconnected")

	// Map must be drained.
	kws.outboundAcksMu.Lock()
	leftover := len(kws.outboundAcks)
	kws.outboundAcksMu.Unlock()
	require.Equal(t, 0, leftover, "outboundAcks must be fully drained after disconnect")
}

// TestSocketIOSendQueueOverflowDropMode verifies that when DropFramesOnOverflow
// is enabled, a saturated send queue causes individual frames to be dropped
// (with EventError firing) instead of tearing down the connection.
//
// We use a real WebSocket connection where the client side never reads, so
// the server-side send goroutine eventually blocks writing to the wire.
// Once that happens, write() can no longer drain the queue and the
// configurable overflow path is exercised.
func TestSocketIOSendQueueOverflowDropMode(t *testing.T) {
	resetSIOGlobals(t)

	// Tunables: tiny queue, drop-mode on. Restore on cleanup.
	prevDrop := DropFramesOnOverflow
	prevSize := SendQueueSize
	DropFramesOnOverflow = true
	SendQueueSize = 4
	t.Cleanup(func() {
		DropFramesOnOverflow = prevDrop
		SendQueueSize = prevSize
	})

	var (
		errFired       atomic.Int64
		sawOverflowErr atomic.Bool
	)
	On(EventError, func(payload *EventPayload) {
		errFired.Add(1)
		if payload != nil && payload.Error != nil &&
			errors.Is(payload.Error, ErrSendQueueOverflow) {
			sawOverflowErr.Store(true)
		}
	})

	// Capture the server-side *Websocket so the test can inject frames
	// directly via Emit (which routes through write()).
	kwsCh := make(chan *Websocket, 1)
	ln, teardown := newSIOTestServer(t, func(kws *Websocket) {
		select {
		case kwsCh <- kws:
		default:
		}
	})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	require.NoError(t, sioHandshake(t, conn))

	var kws *Websocket
	select {
	case kws = <-kwsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("server-side Websocket was not handed to callback")
	}

	// The client deliberately never reads. To accelerate back-pressure on
	// the server's send goroutine we shrink the wire-side write deadline
	// indirectly by flooding with payloads large enough to fill the
	// in-memory listener's pipe buffer quickly.
	bigPayload := bytes.Repeat([]byte("x"), 64*1024)

	// Emit ~100 frames. With SendQueueSize=4 and a stalled wire, the
	// queue must overflow well before the loop ends.
	const N = 100
	for i := 0; i < N; i++ {
		kws.Emit(bigPayload)
	}

	// Allow EventError dispatch to settle.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if errFired.Load() > 0 && sawOverflowErr.Load() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	require.True(t, sawOverflowErr.Load(),
		"expected EventError with ErrSendQueueOverflow at least once")
	require.Greater(t, errFired.Load(), int64(0),
		"expected EventError to fire on queue overflow")
	require.True(t, kws.IsAlive(),
		"connection must stay alive in DropFramesOnOverflow mode")
}

// TestSocketIOAckCrossNamespaceDropped verifies that an inbound ACK frame
// addressed to a foreign namespace ("43/admin,<id>[...]") arriving on a
// connection bound to the root namespace ("/") does NOT fire the root
// namespace's pending ack callback. ACK ids are per-namespace per the
// socket.io v5 spec; without this guard a malicious client could trigger
// any pending callback by tagging a foreign namespace.
//
// Sequence:
//  1. Client connects to root (no /admin handshake).
//  2. Server registers an outbound ack with EmitWithAckTimeout.
//  3. Client replies with "43/admin,<id>[...]" instead of "43<id>[...]".
//  4. The original callback must NOT fire; it should still be waiting for
//     a properly-namespaced ack.
func TestSocketIOAckCrossNamespaceDropped(t *testing.T) {
	resetSIOGlobals(t)

	ready := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case ready <- p.Kws:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	// Plain root-namespace handshake: NO "/admin" CONNECT.
	require.NoError(t, sioHandshake(t, conn))

	var kws *Websocket
	select {
	case kws = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("no EventConnect")
	}
	require.Empty(t, kws.getNamespace(), "connection must be bound to root")

	// Register an outbound ack. Use a long timeout so the callback only
	// fires on actual delivery, not on timeout.
	gotAck := make(chan []byte, 1)
	kws.EmitWithAckTimeout("ping", []byte(`"hi"`), 5*time.Second,
		func(ack []byte, err error) {
			// Either ack or timeout would push here; both are failures
			// for this test (ack -> wrong-ns frame fired callback;
			// timeout-with-data should not happen in 5s either).
			if err == nil {
				gotAck <- ack
			}
		})

	// Read the outbound emit so we know the server-assigned id.
	tp, msg, err := sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, tp)
	// Wire format: "42<id>[\"ping\",\"hi\"]" (root namespace, no prefix).
	require.True(t, bytes.HasPrefix(msg, []byte(`42`)),
		"expected event frame, got %q", msg)
	// Extract the id from "42<id>[".
	rest := msg[2:]
	bracket := bytes.IndexByte(rest, '[')
	require.GreaterOrEqual(t, bracket, 1, "expected ack id before payload")
	idStr := string(rest[:bracket])

	// Inject the cross-namespace ack: "43/admin,<id>[\"pong\"]".
	crossNS := []byte(`43/admin,` + idStr + `["pong"]`)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, crossNS))

	// Round-trip an EIO PING/PONG to ensure the read loop has processed
	// the cross-namespace frame BEFORE we assert on the channel state.
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte{eioPing}))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		mType, m, rerr := conn.ReadMessage()
		if rerr != nil {
			break
		}
		if mType == websocket.TextMessage && len(m) == 1 && m[0] == eioPong {
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{})

	// The callback MUST NOT have fired: cross-namespace ack should be
	// dropped silently, leaving the pending callback registered.
	select {
	case ack := <-gotAck:
		t.Fatalf("cross-namespace ack must not fire callback, got %q", ack)
	case <-time.After(50 * time.Millisecond):
		// Expected: no fire.
	}

	// Connection must still be alive.
	require.True(t, kws.IsAlive(), "conn must survive cross-ns ack")

	// The pending callback must still be registered, waiting for a
	// properly-namespaced ack.
	kws.outboundAcksMu.Lock()
	pending := len(kws.outboundAcks)
	kws.outboundAcksMu.Unlock()
	require.Equal(t, 1, pending,
		"original callback must still be waiting for a properly-namespaced ack")

	// Sanity: a properly-namespaced ack DOES still deliver.
	require.NoError(t, conn.WriteMessage(websocket.TextMessage,
		[]byte(`43`+idStr+`["pong"]`)))
	select {
	case ack := <-gotAck:
		require.Equal(t, `"pong"`, string(ack))
	case <-time.After(2 * time.Second):
		t.Fatal("properly-namespaced ack failed to deliver after cross-ns drop")
	}
}
