package socketio

// Iteration-2 protocol-conformance gap closures (rows 5, 6, 15, 16, 19, 20,
// 22, 28). These exercise CONNECT-with-auth (root + namespaced), namespaced
// inbound EVENT/ACK, ack id 0 round-trip, namespaced DISCONNECT and 0x1E
// batch-splitter edge cases.
//
// This file lives alongside socketio_test.go (same package) and reuses its
// helpers (newSIOTestServer, dialSIO, sioHandshake, sioReadSkipPings).

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/stretchr/testify/require"
)

// sioHandshakeWithConnect performs the EIO/SIO handshake but lets the caller
// supply the body of the SIO CONNECT packet (everything after the "40"
// prefix). It validates the OPEN packet, sends "40"+connectBody, then reads
// the SIO CONNECT confirmation. Returns the raw confirmation frame so the
// caller can assert on the namespace prefix it carries.
func sioHandshakeWithConnect(t *testing.T, conn *websocket.Conn, connectBody string) []byte {
	t.Helper()

	mType, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, mType)
	require.NotEmpty(t, msg)
	require.Equal(t, byte(eioOpen), msg[0])

	var openData eioOpenPacket
	require.NoError(t, json.Unmarshal(msg[1:], &openData))
	require.NotEmpty(t, openData.SID)

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("40"+connectBody)))

	mType, msg, err = conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, mType)
	require.GreaterOrEqual(t, len(msg), 2)
	require.Equal(t, byte(eioMessage), msg[0])
	require.Equal(t, byte(sioConnect), msg[1])
	return msg
}

// TestSocketIOHandshakeWithAuth verifies that an auth payload sent in the
// initial root-namespace SIO CONNECT ("40{...}") is captured and exposed via
// EventPayload.HandshakeAuth and Kws.HandshakeAuth(). Closes test row 5.
func TestSocketIOHandshakeWithAuth(t *testing.T) {
	pool.reset()
	listeners.reset()

	type connectInfo struct {
		auth json.RawMessage
		kws  *Websocket
	}
	connected := make(chan connectInfo, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case connected <- connectInfo{auth: p.HandshakeAuth, kws: p.Kws}:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	sioHandshakeWithConnect(t, conn, `{"token":"abc123"}`)

	select {
	case info := <-connected:
		require.JSONEq(t, `{"token":"abc123"}`, string(info.auth),
			"EventConnect payload must surface the raw auth JSON")
		require.JSONEq(t, `{"token":"abc123"}`, string(info.kws.HandshakeAuth()),
			"Kws.HandshakeAuth() must return the raw auth JSON")
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire within the timeout")
	}
}

// TestSocketIONamespacedHandshakeWithAuth verifies that a namespaced CONNECT
// with an auth payload ("40/admin,{...}") is parsed correctly: namespace is
// captured and mirrored on the ack frame, auth payload is captured.
// Closes test row 6.
func TestSocketIONamespacedHandshakeWithAuth(t *testing.T) {
	pool.reset()
	listeners.reset()

	type connectInfo struct {
		auth json.RawMessage
		kws  *Websocket
	}
	connected := make(chan connectInfo, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case connected <- connectInfo{auth: p.HandshakeAuth, kws: p.Kws}:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	ack := sioHandshakeWithConnect(t, conn, `/admin,{"token":"abc"}`)
	require.Truef(t, strings.HasPrefix(string(ack), "40/admin,"),
		"expected namespaced CONNECT ack, got %q", ack)

	select {
	case info := <-connected:
		require.JSONEq(t, `{"token":"abc"}`, string(info.auth))
		require.Equal(t, "/admin", string(info.kws.getNamespace()))
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire within the timeout")
	}
}

// TestSocketIONamespacedInboundEvent verifies that an inbound EVENT packet
// addressed to a namespace ("42/admin,[...]") is dispatched to the listener.
// Closes test row 16.
func TestSocketIONamespacedInboundEvent(t *testing.T) {
	pool.reset()
	listeners.reset()

	got := make(chan []byte, 1)
	On("msg", func(p *EventPayload) {
		select {
		case got <- p.Data:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()

	sioHandshakeWithConnect(t, conn, "/admin,")

	require.NoError(t, conn.WriteMessage(websocket.TextMessage,
		[]byte(`42/admin,["msg","x"]`)))

	select {
	case data := <-got:
		require.Equal(t, `"x"`, string(data))
	case <-time.After(2 * time.Second):
		t.Fatal("namespaced inbound event was not dispatched")
	}
}

// TestSocketIONamespacedInboundAck verifies the round-trip for an outbound
// EmitWithAck on a namespaced connection: server frame is "42/admin,N[...]"
// and the client reply "43/admin,N[...]" fires the registered callback.
// Closes test row 19.
func TestSocketIONamespacedInboundAck(t *testing.T) {
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

	sioHandshakeWithConnect(t, conn, "/admin,")

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

	require.Truef(t, strings.HasPrefix(string(msg), "42/admin,"),
		"expected namespaced EVENT-with-ack, got %q", msg)
	tail := strings.TrimPrefix(string(msg), "42/admin,")
	bracket := strings.IndexByte(tail, '[')
	require.GreaterOrEqual(t, bracket, 1, "expected ack id before bracket in %q", msg)
	ackID := tail[:bracket]
	require.Equal(t, `["ping","hi"]`, tail[bracket:])

	reply := "43/admin," + ackID + `[{"x":1}]`
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(reply)))

	select {
	case ack := <-gotAck:
		require.JSONEq(t, `{"x":1}`, string(ack))
	case <-time.After(2 * time.Second):
		t.Fatal("namespaced ack callback never fired")
	}
}

// TestSocketIOAckIDZeroRoundTrip verifies that an inbound EVENT with literal
// ack id 0 ("420[...]") is round-tripped: the server-side payload sees
// HasAck=true and AckID=0, and Ack(nil) produces "430[]".
// Closes test rows 15 and 20.
func TestSocketIOAckIDZeroRoundTrip(t *testing.T) {
	pool.reset()
	listeners.reset()

	type seen struct {
		hasAck bool
		ackID  uint64
		err    error
	}
	gotEvent := make(chan seen, 1)
	On("m", func(p *EventPayload) {
		err := p.Ack()
		select {
		case gotEvent <- seen{hasAck: p.HasAck, ackID: p.AckID, err: err}:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	require.NoError(t, conn.WriteMessage(websocket.TextMessage,
		[]byte(`420["m",{}]`)))

	select {
	case s := <-gotEvent:
		require.True(t, s.hasAck, "HasAck must be true for explicit id 0")
		require.Equal(t, uint64(0), s.ackID)
		require.NoError(t, s.err)
	case <-time.After(2 * time.Second):
		t.Fatal("event listener did not fire for ack-id-0 frame")
	}

	tp, msg, err := sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, tp)
	require.Equal(t, `430[]`, string(msg),
		"empty ack with id 0 must serialize as 430[]")
}

// TestSocketIONamespacedDisconnectFromClient verifies that an inbound
// "41/admin," (DISCONNECT on a namespace) fires EventDisconnect.
// Closes test row 22.
func TestSocketIONamespacedDisconnectFromClient(t *testing.T) {
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

	sioHandshakeWithConnect(t, conn, "/admin,")

	require.NoError(t, conn.WriteMessage(websocket.TextMessage,
		[]byte("41/admin,")))

	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("EventDisconnect was not fired for namespaced DISCONNECT")
	}
}

// TestSocketIOEIOBatchEdges exercises the 0x1E batch splitter at its edges:
// leading-only separator (empty batch), trailing separator, double separator
// in the middle, and a leading separator followed by a real packet.
// Closes test row 28.
func TestSocketIOEIOBatchEdges(t *testing.T) {
	pool.reset()
	listeners.reset()

	events := make(chan []byte, 8)
	On("m", func(p *EventPayload) {
		select {
		case events <- p.Data:
		default:
		}
	})
	errs := make(chan error, 8)
	On(EventError, func(p *EventPayload) {
		select {
		case errs <- p.Error:
		default:
		}
	})

	ln, teardown := newSIOTestServer(t, func(_ *Websocket) {})
	defer teardown()

	conn := dialSIO(t, ln)
	defer conn.Close()
	require.NoError(t, sioHandshake(t, conn))

	drainNoEvent := func(label string) {
		select {
		case d := <-events:
			t.Fatalf("%s: unexpected event delivery: %q", label, d)
		case e := <-errs:
			t.Fatalf("%s: unexpected error: %v", label, e)
		case <-time.After(150 * time.Millisecond):
		}
	}

	// 1. Leading 0x1E only (empty batch): splitter must skip the empty
	//    leading packet and emit nothing.
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte{0x1E}))
	drainNoEvent("leading-only 0x1E")

	// 2. Leading 0x1E followed by a real packet must still dispatch the
	//    real packet.
	frame := append([]byte{0x1E}, []byte(`42["m","a"]`)...)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, frame))
	select {
	case d := <-events:
		require.Equal(t, `"a"`, string(d))
	case <-time.After(2 * time.Second):
		t.Fatal("leading 0x1E + packet: event was dropped")
	}

	// 3. Trailing 0x1E after a real packet: dispatched once, no error.
	frame = append([]byte(`42["m","b"]`), 0x1E)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, frame))
	select {
	case d := <-events:
		require.Equal(t, `"b"`, string(d))
	case <-time.After(2 * time.Second):
		t.Fatal("trailing 0x1E: event was dropped")
	}
	drainNoEvent("trailing 0x1E spurious second event")

	// 4. Double 0x1E in the middle: both real packets dispatched, the
	//    empty middle slot is skipped.
	frame = []byte(`42["m","c"]`)
	frame = append(frame, 0x1E, 0x1E)
	frame = append(frame, []byte(`42["m","d"]`)...)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, frame))

	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case d := <-events:
			got[string(d)] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("double 0x1E: missing packet %d, got so far: %v", i, got)
		}
	}
	require.True(t, got[`"c"`], "first packet missing across double-RS split")
	require.True(t, got[`"d"`], "second packet missing across double-RS split")
}
