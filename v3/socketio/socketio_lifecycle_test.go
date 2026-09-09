package socketio

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

// captureConnect registers an EventConnect listener that hands the first
// connected session to the returned channel.
func captureConnect(t *testing.T) <-chan *Websocket {
	t.Helper()
	kwsCh := make(chan *Websocket, 8)
	On(EventConnect, func(p *EventPayload) {
		select {
		case kwsCh <- p.Kws:
		default:
		}
	})
	return kwsCh
}

func awaitSession(t *testing.T, ch <-chan *Websocket) *Websocket {
	t.Helper()
	select {
	case kws := <-ch:
		return kws
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire")
		return nil
	}
}

// rawReadUntilError drains the socket underneath a client until it errors,
// returning that error. A server that never closed the socket shows up as
// a deadline error.
func rawReadUntilError(conn *websocket.Conn, timeout time.Duration) error {
	raw := conn.NetConn()
	_ = raw.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 4096)
	for {
		if _, err := raw.Read(buf); err != nil {
			return err
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// TestSocketIOCloseCompletesClosingHandshakeAndReleasesSocket pins the
// server-initiated close sequence: SIO DISCONNECT, then a Close frame, then,
// once the peer answered, the socket is closed by the server. Before this
// the middleware left the hijacked socket open after the handler returned
// and nothing in socketio closed it.
func TestSocketIOCloseCompletesClosingHandshakeAndReleasesSocket(t *testing.T) {
	resetSIOGlobals(t)
	kwsCh := captureConnect(t)

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))
	kws := awaitSession(t, kwsCh)

	go kws.Close()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	mType, msg, err := sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, mType)
	require.Equal(t, "41", string(msg), "SIO DISCONNECT must precede the Close frame")

	// The library answers the Close frame from inside ReadMessage and
	// reports it as a CloseError.
	_, _, err = conn.ReadMessage()
	require.Truef(t, websocket.IsCloseError(err, websocket.CloseNormalClosure),
		"expected Close frame with code 1000, got %v", err)

	// The peer answered, so the server releases the session right away,
	// well inside CloseTimeout.
	waitClosed(t, kws, 2*time.Second)
	require.False(t, kws.IsAlive())

	// And the socket itself is gone: a raw read reports the close instead
	// of blocking until the deadline.
	err = rawReadUntilError(conn, 2*time.Second)
	require.Error(t, err)
	require.False(t, isTimeout(err), "server did not close the socket: %v", err)
}

// TestSocketIOCloseTimeoutClosesUnresponsivePeer verifies that a peer which
// never answers the Close frame cannot pin the session: after CloseTimeout
// the socket is closed regardless.
func TestSocketIOCloseTimeoutClosesUnresponsivePeer(t *testing.T) {
	resetSIOGlobals(t)
	prev := CloseTimeout
	CloseTimeout = 300 * time.Millisecond
	defer func() { CloseTimeout = prev }()

	kwsCh := captureConnect(t)
	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))
	kws := awaitSession(t, kwsCh)

	start := time.Now()
	kws.Close()
	require.False(t, kws.IsAlive(), "Close must mark the session dead synchronously")

	// The client never reads. The server gives up after CloseTimeout.
	waitClosed(t, kws, 3*time.Second)
	require.Less(t, time.Since(start), 2500*time.Millisecond)

	err := rawReadUntilError(conn, 2*time.Second)
	require.Error(t, err)
	require.False(t, isTimeout(err), "server did not close the socket: %v", err)
}

// TestSocketIOStalledPeerDoesNotWedgeTeardown floods a peer that stopped
// reading over a real TCP socket, so the send goroutine blocks inside a
// write. The queue overflow tears the session down, and the tear-down must
// complete: closing the socket is what returns the stalled write. The old
// implementation waited for the writer before closing anything and hung.
func TestSocketIOStalledPeerDoesNotWedgeTeardown(t *testing.T) {
	resetSIOGlobals(t)
	prevClose, prevDrop := CloseTimeout, DropFramesOnOverflow
	CloseTimeout = 300 * time.Millisecond
	DropFramesOnOverflow = false
	defer func() { CloseTimeout, DropFramesOnOverflow = prevClose, prevDrop }()

	kwsCh := captureConnect(t)
	disc := make(chan error, 1)
	On(EventDisconnect, func(p *EventPayload) {
		select {
		case disc <- p.Error:
		default:
		}
	})

	app := fiber.New()
	app.Use(upgradeMiddleware)
	app.Get("/", New(func(_ *Websocket) {}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = app.Listener(ln) }()
	defer func() { _ = app.ShutdownWithTimeout(5 * time.Second) }()

	dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.Dial("ws://"+ln.Addr().String()+"/", nil)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))
	kws := awaitSession(t, kwsCh)

	// Never read again. 64 KiB frames fill the kernel buffers, then the
	// send goroutine blocks, then the queue overflows.
	payload := bytes.Repeat([]byte("x"), 64<<10)
	go func() {
		for i := 0; i < 4000 && kws.IsAlive(); i++ {
			kws.Emit(payload)
		}
	}()

	select {
	case err := <-disc:
		require.ErrorIs(t, err, ErrSendQueueClosed)
	case <-time.After(15 * time.Second):
		t.Fatal("queue overflow never tore the session down")
	}

	start := time.Now()
	waitClosed(t, kws, 10*time.Second)
	require.Less(t, time.Since(start), 5*time.Second, "stalled write held the tear-down")
}

// TestSocketIOClientDisconnectPacketCompletesClosingHandshake verifies that
// an inbound SIO DISCONNECT is answered with a Close frame, reported as a
// clean disconnect, and followed by the socket being closed.
func TestSocketIOClientDisconnectPacketCompletesClosingHandshake(t *testing.T) {
	resetSIOGlobals(t)
	kwsCh := captureConnect(t)
	disc := make(chan error, 1)
	On(EventDisconnect, func(p *EventPayload) {
		select {
		case disc <- p.Error:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))
	kws := awaitSession(t, kwsCh)

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("41")))

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := sioReadSkipPings(conn)
	require.Truef(t, websocket.IsCloseError(err, websocket.CloseNormalClosure),
		"expected a Close frame after SIO DISCONNECT, got %v", err)

	select {
	case err := <-disc:
		require.NoError(t, err, "a client SIO DISCONNECT is a clean close")
	case <-time.After(2 * time.Second):
		t.Fatal("EventDisconnect did not fire")
	}
	waitClosed(t, kws, 2*time.Second)

	err = rawReadUntilError(conn, 2*time.Second)
	require.Error(t, err)
	require.False(t, isTimeout(err), "server did not close the socket: %v", err)
}

// TestSocketIOPeerCloseCauses checks the EventDisconnect cause for the two
// ways a peer can go away: a Close frame with a normal code is clean, an
// abrupt TCP close is an error.
func TestSocketIOPeerCloseCauses(t *testing.T) {
	resetSIOGlobals(t)
	kwsCh := captureConnect(t)
	disc := make(chan error, 2)
	On(EventDisconnect, func(p *EventPayload) {
		select {
		case disc <- p.Error:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	polite := dialSIO(t, ln)
	defer polite.Close()
	require.NoError(t, sioHandshake(t, polite))
	kwsPolite := awaitSession(t, kwsCh)

	require.NoError(t, polite.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseGoingAway, "bye"), time.Now().Add(time.Second)))
	select {
	case err := <-disc:
		require.NoError(t, err, "a Close frame with code 1001 is a clean close")
	case <-time.After(2 * time.Second):
		t.Fatal("EventDisconnect did not fire for the Close frame")
	}
	waitClosed(t, kwsPolite, 2*time.Second)

	abrupt := dialSIO(t, ln)
	require.NoError(t, sioHandshake(t, abrupt))
	kwsAbrupt := awaitSession(t, kwsCh)
	_ = abrupt.Close() // closes the socket without a Close frame
	select {
	case err := <-disc:
		require.Error(t, err, "an abrupt TCP close is reported as an error")
	case <-time.After(2 * time.Second):
		t.Fatal("EventDisconnect did not fire for the abrupt close")
	}
	waitClosed(t, kwsAbrupt, 2*time.Second)
}

// TestSocketIOEventPingControlFrame verifies that a WebSocket Ping control
// frame reaches EventPing with its payload and is still answered with a
// Pong. The library consumes control frames inside ReadMessage, so the
// events only fire through the installed handlers.
func TestSocketIOEventPingControlFrame(t *testing.T) {
	resetSIOGlobals(t)
	kwsCh := captureConnect(t)
	pingCh := make(chan []byte, 1)
	On(EventPing, func(p *EventPayload) {
		select {
		case pingCh <- p.Data:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))
	_ = awaitSession(t, kwsCh)

	pongCh := make(chan string, 1)
	conn.SetPongHandler(func(data string) error {
		select {
		case pongCh <- data:
		default:
		}
		return nil
	})
	// The pong handler runs inside ReadMessage; keep a reader parked.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	require.NoError(t, conn.WriteControl(websocket.PingMessage, []byte("hello"), time.Now().Add(time.Second)))

	select {
	case data := <-pingCh:
		require.Equal(t, "hello", string(data))
	case <-time.After(2 * time.Second):
		t.Fatal("EventPing did not fire for a Ping control frame")
	}
	select {
	case data := <-pongCh:
		require.Equal(t, "hello", data)
	case <-time.After(2 * time.Second):
		t.Fatal("no Pong answered the Ping")
	}
}

// TestSocketIOLateConnectOtherNamespaceRejected verifies that a CONNECT for
// a second namespace on an established connection is answered with
// CONNECT_ERROR rather than acknowledged into a namespace whose events
// would all be dropped, while a repeated CONNECT for the bound namespace is
// acknowledged again.
func TestSocketIOLateConnectOtherNamespaceRejected(t *testing.T) {
	resetSIOGlobals(t)
	kwsCh := captureConnect(t)

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))
	kws := awaitSession(t, kwsCh)

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("40/admin,")))
	_, msg, err := sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, `44/admin,{"message":"Invalid namespace"}`, string(msg))
	require.True(t, kws.IsAlive(), "a rejected late CONNECT must not end the connection")

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("40")))
	_, msg, err = sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, `40{"sid":"`+kws.GetUUID()+`"}`, string(msg))
}

// TestSocketIOBroadcastAcrossNamespaces verifies that a broadcast builds the
// right frame for each namespace while sharing it between recipients.
func TestSocketIOBroadcastAcrossNamespaces(t *testing.T) {
	resetSIOGlobals(t)
	kwsCh := captureConnect(t)

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	root1 := dialSIO(t, ln)
	defer root1.Close()
	require.NoError(t, sioHandshake(t, root1))
	awaitSession(t, kwsCh)
	root2 := dialSIO(t, ln)
	defer root2.Close()
	require.NoError(t, sioHandshake(t, root2))
	awaitSession(t, kwsCh)

	admin := dialSIO(t, ln)
	defer admin.Close()
	_, _, err := admin.ReadMessage() // EIO OPEN
	require.NoError(t, err)
	require.NoError(t, admin.WriteMessage(websocket.TextMessage, []byte("40/admin,")))
	_, msg, err := admin.ReadMessage()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(msg), "40/admin,"), "got %q", msg)
	awaitSession(t, kwsCh)

	Broadcast([]byte(`"hi"`))

	for _, c := range []*websocket.Conn{root1, root2} {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := sioReadSkipPings(c)
		require.NoError(t, err)
		require.Equal(t, `42["message","hi"]`, string(msg))
	}
	_ = admin.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err = sioReadSkipPings(admin)
	require.NoError(t, err)
	require.Equal(t, `42/admin,["message","hi"]`, string(msg))
}

// TestSocketIOEventCloseListenerEmitsBeforeDisconnect verifies that frames
// emitted from an EventClose listener are delivered ahead of the SIO
// DISCONNECT packet: the closing frames are queued after the listener ran.
func TestSocketIOEventCloseListenerEmitsBeforeDisconnect(t *testing.T) {
	resetSIOGlobals(t)
	kwsCh := captureConnect(t)
	On(EventClose, func(p *EventPayload) {
		p.Kws.EmitEvent("bye", []byte(`"now"`))
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))
	kws := awaitSession(t, kwsCh)

	go kws.Close()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, `42["bye","now"]`, string(msg))
	_, msg, err = sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, "41", string(msg))
}

// TestSocketIOHandshakeRejectionDeliversConnectError verifies that a
// rejected CONNECT still gets its CONNECT_ERROR through before the socket
// is closed, with a Close frame in between.
func TestSocketIOHandshakeRejectionDeliversConnectError(t *testing.T) {
	resetSIOGlobals(t)
	prev := CloseTimeout
	CloseTimeout = 500 * time.Millisecond
	defer func() { CloseTimeout = prev }()

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	_, _, err := conn.ReadMessage() // EIO OPEN
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`40["not","an","object"]`)))

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, `44{"message":"Invalid auth payload"}`, string(msg))
	_, _, err = conn.ReadMessage()
	require.Truef(t, websocket.IsCloseError(err, websocket.ClosePolicyViolation),
		"expected Close frame with code 1008, got %v", err)

	err = rawReadUntilError(conn, 2*time.Second)
	require.Error(t, err)
	require.False(t, isTimeout(err), "server did not close the socket: %v", err)
}

// TestSocketIOPollingSessionReleasesOnDisconnect verifies that a polling
// session, which owns no goroutine, still signals its release.
func TestSocketIOPollingSessionReleasesOnDisconnect(t *testing.T) {
	resetSIOGlobals(t)
	kwsCh := captureConnect(t)

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	_, _ = pollPost(t, c, sid, []byte(`40`))
	kws := awaitSession(t, kwsCh)
	require.True(t, kws.IsPolling())

	kws.Close()
	waitClosed(t, kws, 2*time.Second)
}

// ---------------------------------------------------------------------------
// codec
// ---------------------------------------------------------------------------

func TestSplitJSONArray(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		err  bool
	}{
		{in: `["ev"]`, want: []string{`"ev"`}},
		{in: `[]`, want: nil},
		{in: ` [ ] `, want: nil},
		{in: `["ev",{"a":[1,2]},[3,4],"x,y]"]`, want: []string{`"ev"`, `{"a":[1,2]}`, `[3,4]`, `"x,y]"`}},
		{in: " [ \"ev\" , 1 ,\t\"a\\\"b\" ]\n", want: []string{`"ev"`, `1`, `"a\"b"`}},
		{in: `["a\\",1]`, want: []string{`"a\\"`, `1`}},
		{in: `["[","{","}",","]`, want: []string{`"["`, `"{"`, `"}"`, `","`}},
		{in: `[null,true,false,-1.5e3,"é"]`, want: []string{`null`, `true`, `false`, `-1.5e3`, `"é"`}},
		{in: `[{"deep":{"deeper":[{"x":"]"}]}}]`, want: []string{`{"deep":{"deeper":[{"x":"]"}]}}`}},
		{in: `{"a":1}`, err: true},
		{in: `["a"`, err: true},
		{in: `["a",]`, err: true},
		{in: `[,]`, err: true},
		{in: `not json`, err: true},
		{in: ``, err: true},
		{in: `"str"`, err: true},
	}
	for _, tc := range cases {
		got, err := splitJSONArray([]byte(tc.in), nil)
		if tc.err {
			require.Errorf(t, err, "input %q", tc.in)
			continue
		}
		require.NoErrorf(t, err, "input %q", tc.in)
		require.Lenf(t, got, len(tc.want), "input %q: %q", tc.in, got)
		for i := range tc.want {
			require.Equalf(t, tc.want[i], string(got[i]), "input %q element %d", tc.in, i)
		}
	}
}

// TestParseSIOEventAliasesPayload pins the zero-copy contract: arguments
// are sub-slices of the frame, whose buffer belongs to the connection.
func TestParseSIOEventAliasesPayload(t *testing.T) {
	payload := []byte(`["ev",{"k":1},2]`)
	name, args, err := parseSIOEvent(payload)
	require.NoError(t, err)
	require.Equal(t, "ev", name)
	require.Len(t, args, 2)
	off := bytes.Index(payload, []byte(`{"k":1}`))
	require.Same(t, &payload[off], &args[0][0], "args must alias the payload, not copy it")
	require.Equal(t, len(args[0]), cap(args[0]), "capacity must be clipped so appends cannot clobber the next argument")
}

func TestParseSIOEventNames(t *testing.T) {
	name, args, err := parseSIOEvent([]byte(`["a\"bé",1]`))
	require.NoError(t, err)
	require.Equal(t, "a\"bé", name)
	require.Len(t, args, 1)

	_, _, err = parseSIOEvent([]byte(`[1,2]`))
	require.ErrorIs(t, err, errEventNameNotString)

	_, _, err = parseSIOEvent([]byte(`[]`))
	require.ErrorIs(t, err, ErrEmptyEventArray)

	_, _, err = parseSIOEvent([]byte(`{"ev":1}`))
	require.ErrorIs(t, err, errNotJSONArray)
	require.Contains(t, err.Error(), "failed to parse event payload")
}

// TestSplitJSONArrayBoundsElements pins the MaxEventArgs cap: an array with
// more elements than the limit is rejected before its slice headers are
// allocated, on both the EVENT and the ACK path.
func TestSplitJSONArrayBoundsElements(t *testing.T) {
	prev := MaxEventArgs
	MaxEventArgs = 3
	defer func() { MaxEventArgs = prev }()

	got, err := splitJSONArray([]byte(`[1,2,3]`), nil)
	require.NoError(t, err)
	require.Len(t, got, 3)
	_, err = splitJSONArray([]byte(`[1,2,3,4]`), nil)
	require.ErrorIs(t, err, ErrTooManyArgs)
	_, _, err = parseSIOEvent([]byte(`["ev",1,2,3]`))
	require.ErrorIs(t, err, ErrTooManyArgs)
	_, err = parseSIOAckArgs([]byte(`[1,2,3,4]`))
	require.ErrorIs(t, err, ErrTooManyArgs)

	MaxEventArgs = 0
	got, err = splitJSONArray([]byte(`[1,2,3,4,5,6,7,8,9]`), nil)
	require.NoError(t, err)
	require.Len(t, got, 9)
}

func TestParseSIOAckArgs(t *testing.T) {
	args, err := parseSIOAckArgs([]byte(`[]`))
	require.NoError(t, err)
	require.Nil(t, args)
	args, err = parseSIOAckArgs([]byte(`[[1,2]]`))
	require.NoError(t, err)
	require.Equal(t, [][]byte{[]byte(`[1,2]`)}, args)
	args, err = parseSIOAckArgs([]byte(`[1,2]`))
	require.NoError(t, err)
	require.Equal(t, [][]byte{[]byte(`1`), []byte(`2`)}, args)
	_, err = parseSIOAckArgs([]byte(`1`))
	require.Error(t, err)
}

// TestBuildSIOEventEncodesLikeEncodingJSON pins the SWAR JSON string
// encoder to encoding/json's output for event names and raw-text arguments.
func TestBuildSIOEventEncodesLikeEncodingJSON(t *testing.T) {
	for _, name := range []string{"plain", `q"uote`, "<tag>&", "ünï", "\x00ctl\t", "  sep", "back\\slash"} {
		want, err := json.Marshal(name)
		require.NoError(t, err)
		require.Equal(t, `42[`+string(want)+`]`, string(buildSIOEvent(nil, name, nil)), "name %q", name)
		require.Equal(t, `42["e",`+string(want)+`]`, string(buildSIOEvent(nil, "e", []byte(name))), "raw arg %q", name)
	}
	// Invalid UTF-8 is replaced with U+FFFD by both encoders, but how the
	// replacement character itself is spelled differs between Go releases
	// (escaped as \ufffd up to Go 1.26, literal from Go 1.27), so compare
	// what the frames decode to rather than their bytes.
	invalid := "\xff invalid"
	var want string
	require.NoError(t, json.Unmarshal([]byte(`"\ufffd invalid"`), &want))
	for _, frame := range [][]byte{buildSIOEvent(nil, invalid, nil), buildSIOEvent(nil, "e", []byte(invalid))} {
		elems, err := splitJSONArray(frame[2:], nil)
		require.NoError(t, err, "frame %q", frame)
		var got string
		require.NoError(t, json.Unmarshal(elems[len(elems)-1], &got), "frame %q", frame)
		require.Equal(t, want, got, "frame %q", frame)
	}
	// Valid JSON passes through untouched.
	require.Equal(t, `42/ns,["e",{"a":1},[2],"s",3]`,
		string(buildSIOEventWithAck([]byte("/ns"), 0, false, "e", [][]byte{[]byte(`{"a":1}`), nil, []byte(`[2]`), []byte(`"s"`), []byte(`3`)})))
	require.Equal(t, `427["e"]`, string(buildSIOEventWithAck(nil, 7, true, "e", nil)))
}

func TestBuildEIOOpenFrameDecodes(t *testing.T) {
	frame, err := buildEIOOpenFrame(`s"id`)
	require.NoError(t, err)
	require.Equal(t, byte(eioOpen), frame[0])
	var open eioOpenPacket
	require.NoError(t, json.Unmarshal(frame[1:], &open))
	require.Equal(t, `s"id`, open.SID)
	require.NotNil(t, open.Upgrades)
	require.Empty(t, open.Upgrades)
	require.Equal(t, int(PingInterval.Milliseconds()), open.PingInterval)
	require.Equal(t, int(PingTimeout.Milliseconds()), open.PingTimeout)
	require.Equal(t, int(MaxPayload), open.MaxPayload)
}

func TestBuildSIOConnectFrames(t *testing.T) {
	require.Equal(t, `40{"sid":"abc"}`, string(buildSIOConnectAckSID(nil, "abc")))
	require.Equal(t, `40/admin,{"sid":"a\"b"}`, string(buildSIOConnectAckSID([]byte("/admin"), `a"b`)))
	require.Equal(t, `44/admin,{"message":"x"}`, string(buildSIOConnectError([]byte("/admin"), `{"message":"x"}`)))
	require.Equal(t, `41`, string(buildSIODisconnect(nil)))
	require.Equal(t, `41/admin,`, string(buildSIODisconnect([]byte("/admin"))))
}

func TestIsValidNamespaceTable(t *testing.T) {
	long := "/" + strings.Repeat("abcDEF019_-./", 8)
	for _, ok := range []string{"", "/", "/a", "/a-b.c_d/e", long} {
		require.Truef(t, isValidNamespace([]byte(ok)), "%q should be valid", ok)
	}
	for _, bad := range []string{"admin", "/a b", "/ab,c", "/ünï", "/a\"b", long + " ", "/" + strings.Repeat("a", 40) + "," + strings.Repeat("b", 40)} {
		require.Falsef(t, isValidNamespace([]byte(bad)), "%q should be invalid", bad)
	}
}

func TestBroadcastFramesCache(t *testing.T) {
	var c broadcastFrames
	msg := []byte(`"m"`)
	root := c.frame(nil, msg)
	require.Equal(t, `42["message","m"]`, string(root))
	require.Same(t, &root[0], &c.frame(nil, msg)[0], "root frame must be built once")
	admin := c.frame([]byte("/admin"), msg)
	require.Equal(t, `42/admin,["message","m"]`, string(admin))
	require.Same(t, &admin[0], &c.frame([]byte("/admin"), msg)[0], "namespace frame must be built once")
	require.NotSame(t, &root[0], &admin[0])
}
