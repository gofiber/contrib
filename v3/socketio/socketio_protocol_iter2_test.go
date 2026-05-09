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
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/gofiber/fiber/v3"
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

// TestSocketIONamespacedDisconnectMismatchIgnored verifies that an inbound
// "41/<ns>," frame whose namespace does NOT match the bound namespace is
// silently ignored instead of tearing down the EIO connection. Per
// socket.io-protocol v5, a DISCONNECT only detaches the targeted namespace;
// since this implementation is single-namespace-per-conn, sibling-namespace
// disconnects must not kill the conn (otherwise a malicious client could
// drop any session by sending "41/foreign,").
func TestSocketIONamespacedDisconnectMismatchIgnored(t *testing.T) {
	pool.reset()
	listeners.reset()

	ready := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case ready <- p.Kws:
		default:
		}
	})
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

	// 1. Client connects on the root namespace (kws.namespace == nil).
	require.NoError(t, sioHandshake(t, conn))

	var kws *Websocket
	select {
	case kws = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire within the timeout")
	}

	// 2. Client sends a mismatched DISCONNECT addressed at "/admin" while
	//    the conn is bound to root. The handler must NOT call disconnected().
	require.NoError(t, conn.WriteMessage(websocket.TextMessage,
		[]byte("41/admin,")))

	// 3. EventDisconnect must NOT fire within a reasonable window.
	select {
	case <-disconnected:
		t.Fatal("foreign-namespace DISCONNECT killed the conn")
	case <-time.After(200 * time.Millisecond):
	}

	// 4. The connection must still be alive: a server-side Emit reaches the
	//    client, proving the read loop and send pipeline survived.
	require.True(t, kws.IsAlive(), "kws must remain alive after mismatched DISCONNECT")
	kws.Emit([]byte(`"still-here"`))

	tp, raw, err := sioReadSkipPings(conn)
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, tp)
	require.Equal(t, `42["message","still-here"]`, string(raw))
}

// TestSocketIOEIO3Rejected verifies that a client requesting an unsupported
// Engine.IO protocol version (e.g. "?EIO=3") is rejected with HTTP 400 and
// the JSON error body shape used by the reference socket.io server, BEFORE
// the WebSocket upgrade is attempted. This prevents older clients from
// silently establishing a session against a v4-only server.
//
// Uses a real net.Listen("tcp") server (not InmemoryListener) so http.Get
// can reach it via the OS networking stack. The standard upgradeMiddleware
// helper used by the other tests is intentionally omitted here because it
// short-circuits any non-WebSocket request with HTTP 426 before our EIO
// check runs; the audit asks New() itself to perform the version gate so
// applications can wire their own pre-upgrade middleware order.
func TestSocketIOEIO3Rejected(t *testing.T) {
	resetSIOGlobals(t)

	app := fiber.New()
	app.Get("/", New(func(_ *Websocket) {}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = app.Listener(ln) }()
	defer func() { _ = app.Shutdown() }()

	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get("http://" + ln.Addr().String() + "/?EIO=3")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"EIO v3 must be rejected with 400, got %d", resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.JSONEq(t,
		`{"code":5,"message":"Unsupported protocol version"}`,
		string(body),
		"unexpected error body: %q", string(body),
	)
}

// TestSocketIOEmitWithAckArgsDistinguishesArrayArg verifies that a client ack
// of "43<id>[[1,2]]" (single argument that IS a JSON array) and
// "43<id>[1,2]" (two arguments) are surfaced distinctly to EmitWithAckArgs
// callbacks. Earlier code collapsed both shapes into one []byte at the
// internal callback boundary, making them indistinguishable; this test pins
// the fix so future regressions are caught.
func TestSocketIOEmitWithAckArgsDistinguishesArrayArg(t *testing.T) {
	resetSIOGlobals(t)

	type ackResult struct {
		args [][]byte
		err  error
	}

	cases := []struct {
		name           string
		clientAckFrame string
		want           [][]byte
	}{
		{
			name:           "single arg that is a JSON array",
			clientAckFrame: `[[1,2]]`,
			want:           [][]byte{[]byte(`[1,2]`)},
		},
		{
			name:           "two scalar args",
			clientAckFrame: `[1,2]`,
			want:           [][]byte{[]byte(`1`), []byte(`2`)},
		},
		{
			name:           "three args mixed",
			clientAckFrame: `[1,"two",{"k":3}]`,
			want:           [][]byte{[]byte(`1`), []byte(`"two"`), []byte(`{"k":3}`)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
			require.NoError(t, sioHandshake(t, conn))

			var kws *Websocket
			select {
			case kws = <-kwsCh:
			case <-time.After(2 * time.Second):
				t.Fatal("EventConnect did not fire")
			}

			result := make(chan ackResult, 1)
			kws.EmitWithAckArgs("ping", [][]byte{[]byte(`"hello"`)}, func(args [][]byte, err error) {
				result <- ackResult{args: args, err: err}
			})

			// Read the outbound EVENT to learn the assigned ack id.
			tp, msg, err := sioReadSkipPings(conn)
			require.NoError(t, err)
			require.Equal(t, websocket.TextMessage, tp)
			require.True(t, strings.HasPrefix(string(msg), "42"), "want '42<id>[...]' got %q", msg)

			// Extract the ack id (digits between "42" and the next non-digit).
			rest := string(msg)[2:]
			i := 0
			for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
				i++
			}
			require.Greater(t, i, 0, "no ack id in frame %q", msg)
			ackID := rest[:i]

			// Reply with the chosen ack-frame shape.
			ackFrame := "43" + ackID + tc.clientAckFrame
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(ackFrame)))

			select {
			case r := <-result:
				require.NoError(t, r.err)
				require.Equal(t, len(tc.want), len(r.args), "arg count mismatch")
				for i := range tc.want {
					require.Equal(t, string(tc.want[i]), string(r.args[i]), "arg %d", i)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("ack callback did not fire")
			}
		})
	}
}
