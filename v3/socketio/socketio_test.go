package socketio

import (
	"context"
	"encoding/json"
	"errors"
	"net"
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

	if pool.contains(uuid) {
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
func (s *WebsocketMock) EmitArgs(_ string, _ ...[]byte)                           {}
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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()

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
	pool.reset()

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
	pool.reset()

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
	pool.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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

// TestSocketIONamespaceHandshake verifies that a client connecting to a
// non-root namespace gets a namespace-prefixed CONNECT ack and that server
// emits are scoped to the same namespace.
func TestSocketIONamespaceHandshake(t *testing.T) {
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	require.Equal(t, `43/admin,7[{"x":1}]`, string(buildSIOAck([]byte("/admin"), 7, [][]byte{[]byte(`{"x":1}`)})))
	// Multi-arg ack: each arg is comma-separated JSON inside the array.
	require.Equal(t, `431["a","b",{"k":1}]`, string(buildSIOAck(nil, 1, [][]byte{
		[]byte(`"a"`), []byte(`"b"`), []byte(`{"k":1}`),
	})))
}

// TestSocketIOPingTimeoutIsAdvertised checks that the EIO OPEN packet exposes
// the configured PingTimeout, not the deprecated 1s PongTimeout default.
func TestSocketIOPingTimeoutIsAdvertised(t *testing.T) {
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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

	for _, n := range []string{"connect", "connect_error", "disconnect",
		"disconnecting", "newListener", "removeListener"} {
		kws.EmitEvent(n, []byte(`{"x":1}`))
	}

	// Read for a short window. Any frame that arrives must NOT carry one
	// of the reserved names. (PINGs are auto-skipped by sioReadSkipPings.)
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		require.Falsef(t,
			strings.Contains(string(msg), `"connect"`) ||
				strings.Contains(string(msg), `"disconnect"`) ||
				strings.Contains(string(msg), `"connect_error"`) ||
				strings.Contains(string(msg), `"disconnecting"`) ||
				strings.Contains(string(msg), `"newListener"`) ||
				strings.Contains(string(msg), `"removeListener"`),
			"reserved name leaked onto wire: %q", msg,
		)
	}

	// At least one EventError must have fired.
	select {
	case <-gotErr:
	case <-time.After(50 * time.Millisecond):
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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
	pool.reset()
	listeners.reset()

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
