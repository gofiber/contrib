package socketio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp/fasthttputil"
)

// newPollingTestServer starts a Fiber app with EnablePolling=true and the
// socketio middleware mounted on / for both GET and POST. Returns the
// in-memory listener, an http.Client preconfigured to talk to it, and a
// teardown function that closes both.
func newPollingTestServer(t *testing.T, callback func(*Websocket)) (*fasthttputil.InmemoryListener, *http.Client, func()) {
	t.Helper()

	prevEnable := EnablePolling
	prevWait := MaxPollWait
	EnablePolling = true
	// Tighten the long-poll deadline so timeout-driven tests do not have
	// to wait the default 30s.
	MaxPollWait = 2 * time.Second

	app := fiber.New()
	ln := fasthttputil.NewInmemoryListener()

	h := New(callback)
	app.Get("/", h)
	app.Post("/", h)
	app.Options("/", h)

	go func() { _ = app.Listener(ln) }()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return ln.Dial()
			},
		},
		Timeout: 10 * time.Second,
	}

	return ln, client, func() {
		_ = app.Shutdown()
		_ = ln.Close()
		EnablePolling = prevEnable
		MaxPollWait = prevWait
	}
}

func pollOpen(t *testing.T, c *http.Client) (sid string, body []byte, status int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://test/?EIO=4&transport=polling", nil)
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	status = resp.StatusCode
	if status != http.StatusOK {
		return "", body, status
	}
	require.True(t, len(body) > 1 && body[0] == eioOpen, "expected OPEN packet, got %q", body)
	var open eioOpenPacket
	require.NoError(t, json.Unmarshal(body[1:], &open))
	sid = open.SID
	return sid, body, status
}

func pollGet(t *testing.T, c *http.Client, sid string) (body []byte, status int) {
	t.Helper()
	url := "http://test/?EIO=4&transport=polling&sid=" + sid
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return body, resp.StatusCode
}

func pollPost(t *testing.T, c *http.Client, sid string, body []byte) (resp []byte, status int) {
	t.Helper()
	url := "http://test/?EIO=4&transport=polling&sid=" + sid
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	r, err := c.Do(req)
	require.NoError(t, err)
	defer r.Body.Close()
	resp, err = io.ReadAll(r.Body)
	require.NoError(t, err)
	return resp, r.StatusCode
}

func splitPollFrames(body []byte) [][]byte {
	if len(body) == 0 {
		return nil
	}
	if !bytes.ContainsRune(body, 0x1E) {
		return [][]byte{body}
	}
	out := bytes.Split(body, []byte{0x1E})
	clean := out[:0]
	for _, f := range out {
		if len(f) > 0 {
			clean = append(clean, f)
		}
	}
	return clean
}

// TestPollingOpenHandshake verifies the GET-with-no-sid path returns a
// well-formed OPEN packet with the configured timing fields and an
// empty upgrades array (polling-to-WS upgrade is not yet implemented).
func TestPollingOpenHandshake(t *testing.T) {
	resetSIOGlobals(t)
	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, body, status := pollOpen(t, c)
	require.Equal(t, http.StatusOK, status)
	require.NotEmpty(t, sid)

	var open eioOpenPacket
	require.NoError(t, json.Unmarshal(body[1:], &open))
	require.Equal(t, sid, open.SID)
	require.Empty(t, open.Upgrades, "polling sessions advertise no upgrades in MVP")
	require.Equal(t, int(PingInterval.Milliseconds()), open.PingInterval)
	require.Equal(t, int(PingTimeout.Milliseconds()), open.PingTimeout)
	require.Greater(t, open.MaxPayload, 0)
}

// TestPollingDisabledByDefault verifies that without EnablePolling=true,
// polling requests are NOT served and the WebSocket-only flow is preserved.
func TestPollingDisabledByDefault(t *testing.T) {
	resetSIOGlobals(t)
	prev := EnablePolling
	EnablePolling = false
	defer func() { EnablePolling = prev }()

	app := fiber.New()
	ln := fasthttputil.NewInmemoryListener()
	app.Get("/", New(func(_ *Websocket) {}))
	go func() { _ = app.Listener(ln) }()
	defer func() { _ = app.Shutdown(); _ = ln.Close() }()

	c := &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return ln.Dial()
		},
	}, Timeout: 5 * time.Second}

	req, _ := http.NewRequest(http.MethodGet, "http://test/?EIO=4&transport=polling", nil)
	resp, err := c.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.NotEqual(t, http.StatusOK, resp.StatusCode,
		"polling must be rejected when EnablePolling=false")
}

// TestPollingConnectFiresEventConnect verifies the SIO CONNECT flow over
// polling: client OPENs via GET, POSTs "40", and the server fires
// EventConnect exactly once with the auth payload populated.
func TestPollingConnectFiresEventConnect(t *testing.T) {
	resetSIOGlobals(t)

	type connEvt struct{ auth json.RawMessage }
	connCh := make(chan connEvt, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case connCh <- connEvt{auth: p.HandshakeAuth}:
		default:
		}
	})

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, status := pollOpen(t, c)
	require.Equal(t, http.StatusOK, status)

	// POST SIO CONNECT with auth.
	body := []byte(`40{"token":"abc"}`)
	resp, st := pollPost(t, c, sid, body)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "ok", string(resp))

	select {
	case ev := <-connCh:
		require.Equal(t, `{"token":"abc"}`, string(ev.auth))
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire")
	}

	// Subsequent GET drains the SIO CONNECT ack.
	frames, _ := pollGet(t, c, sid)
	hasAck := false
	for _, f := range splitPollFrames(frames) {
		if bytes.HasPrefix(f, []byte{eioMessage, sioConnect}) && bytes.Contains(f, []byte(`"sid"`)) {
			hasAck = true
			break
		}
	}
	require.True(t, hasAck, "expected 40{\"sid\":...} ack frame in drain, got %q", frames)
}

// TestPollingCallbackEmitArrivesAfterConnectAck verifies polling preserves the
// same callback ordering as WebSocket: the user's New callback runs only after
// SIO CONNECT, so callback Emits are drained after the CONNECT ack.
func TestPollingCallbackEmitArrivesAfterConnectAck(t *testing.T) {
	resetSIOGlobals(t)

	callbackAuth := make(chan json.RawMessage, 1)
	_, c, td := newPollingTestServer(t, func(kws *Websocket) {
		callbackAuth <- kws.HandshakeAuth()
		kws.Emit([]byte("welcome raw text"))
	})
	defer td()

	sid, _, status := pollOpen(t, c)
	require.Equal(t, http.StatusOK, status)

	resp, st := pollPost(t, c, sid, []byte(`40{"token":"abc"}`))
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "ok", string(resp))

	select {
	case auth := <-callbackAuth:
		require.JSONEq(t, `{"token":"abc"}`, string(auth))
	case <-time.After(2 * time.Second):
		t.Fatal("polling callback did not run")
	}

	body, _ := pollGet(t, c, sid)
	frames := splitPollFrames(body)
	require.GreaterOrEqual(t, len(frames), 2, "expected CONNECT ack + callback emit, got %q", body)
	require.Truef(t,
		len(frames[0]) >= 2 && frames[0][0] == eioMessage && frames[0][1] == sioConnect,
		"first frame must be SIO CONNECT ack, got %q in body %q", frames[0], body,
	)
	require.Equal(t, `42["message","welcome raw text"]`, string(frames[1]))
}

// TestPollingPostEventDispatch verifies a SIO event posted via polling
// reaches the registered listener and that the listener can Emit a reply
// that is delivered on the next GET drain.
func TestPollingPostEventDispatch(t *testing.T) {
	resetSIOGlobals(t)

	gotPayload := make(chan []byte, 1)
	On("hello", func(p *EventPayload) {
		select {
		case gotPayload <- append([]byte(nil), p.Data...):
		default:
		}
		p.Kws.EmitEvent("reply", []byte(`"world"`))
	})

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)

	// Connect first.
	resp, st := pollPost(t, c, sid, []byte(`40`))
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "ok", string(resp))

	// POST the event.
	resp, st = pollPost(t, c, sid, []byte(`42["hello","world"]`))
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "ok", string(resp))

	select {
	case data := <-gotPayload:
		require.Equal(t, `"world"`, string(data))
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not fire")
	}

	// GET drains the connect ack and the reply event.
	body, _ := pollGet(t, c, sid)
	frames := splitPollFrames(body)
	var sawReply bool
	for _, f := range frames {
		if bytes.HasPrefix(f, []byte("42")) && bytes.Contains(f, []byte(`"reply"`)) {
			sawReply = true
		}
	}
	require.Truef(t, sawReply, "expected reply event in drain, got %q", body)
}

// TestPollingUnknownSid verifies that a GET/POST with an unknown sid
// returns HTTP 400 with engine.io error code 1.
func TestPollingUnknownSid(t *testing.T) {
	resetSIOGlobals(t)
	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	body, status := pollGet(t, c, "deadbeef-0000-0000-0000-000000000000")
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, string(body), `"code":1`)
}

// TestPollingJSONPRejected verifies the JSONP fallback is refused.
func TestPollingJSONPRejected(t *testing.T) {
	resetSIOGlobals(t)
	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	req, _ := http.NewRequest(http.MethodGet, "http://test/?EIO=4&transport=polling&j=0", nil)
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, string(body), `"code":3`)
}

// TestPollingOptionsPreflight verifies the documented app.Options(path, h)
// setup works for sid-less browser CORS preflight requests.
func TestPollingOptionsPreflight(t *testing.T) {
	resetSIOGlobals(t)
	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	req, _ := http.NewRequest(http.MethodOptions, "http://test/?EIO=4&transport=polling", nil)
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Empty(t, body)
}

// TestPollingConcurrentGetsRejected verifies that a second long-poll GET
// while another is in flight is rejected with engine.io code 3.
func TestPollingConcurrentGetsRejected(t *testing.T) {
	resetSIOGlobals(t)
	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	_, _ = pollPost(t, c, sid, []byte(`40`))
	// Drain the connect ack so the next GET will block.
	_, _ = pollGet(t, c, sid)

	// Start a long-poll GET in a goroutine; it will block until MaxPollWait
	// or until something is enqueued.
	firstDone := make(chan struct{})
	go func() {
		_, _ = pollGet(t, c, sid)
		close(firstDone)
	}()

	// Give the first poll time to enter drain.
	time.Sleep(50 * time.Millisecond)

	body, status := pollGet(t, c, sid)
	require.Equal(t, http.StatusBadRequest, status,
		"expected concurrent GET to be rejected with 400, got %d body=%q", status, body)
	require.Contains(t, string(body), `"code":3`)

	// Let the first poll wind down.
	select {
	case <-firstDone:
	case <-time.After(MaxPollWait + 2*time.Second):
		t.Fatal("first long-poll did not return")
	}
}

// TestPollingCloseDeliversDisconnect verifies that Close() on a polling
// session enqueues SIO DISCONNECT + EIO CLOSE so an in-flight long-poll
// receives them and subsequent GETs report unknown sid.
func TestPollingCloseDeliversDisconnect(t *testing.T) {
	resetSIOGlobals(t)

	var captured atomic.Pointer[Websocket]
	On(EventConnect, func(p *EventPayload) {
		captured.Store(p.Kws)
	})

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	_, _ = pollPost(t, c, sid, []byte(`40`))
	require.Eventually(t, func() bool { return captured.Load() != nil }, 2*time.Second, 10*time.Millisecond)

	// Drain the CONNECT ack first.
	_, _ = pollGet(t, c, sid)

	// Start a long-poll, then Close from another goroutine.
	pollResult := make(chan []byte, 1)
	go func() {
		body, _ := pollGet(t, c, sid)
		pollResult <- body
	}()
	time.Sleep(50 * time.Millisecond)
	captured.Load().Close()

	select {
	case body := <-pollResult:
		// Body should contain SIO DISCONNECT and/or EIO CLOSE.
		joined := string(body)
		require.Truef(t,
			strings.Contains(joined, string([]byte{eioMessage, sioDisconnect})) ||
				strings.Contains(joined, string([]byte{eioClose})),
			"expected disconnect/close frames in drain, got %q", body)
	case <-time.After(2 * time.Second):
		t.Fatal("long-poll did not return after Close")
	}

	// Subsequent GET reports unknown sid.
	body, status := pollGet(t, c, sid)
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, string(body), `"code":1`)
}

// TestPollQueueDrainSemantics covers the pollQueue primitive directly:
// FIFO order, batching, byte cap, blocking, close.
func TestPollQueueDrainSemantics(t *testing.T) {
	q := newPollQueue()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// Enqueue 3 small frames; drain returns all under a generous cap.
	q.enqueue([]byte("a"))
	q.enqueue([]byte("bb"))
	q.enqueue([]byte("ccc"))
	frames, closed := q.drain(ctx, 1<<20)
	require.False(t, closed)
	require.Equal(t, [][]byte{[]byte("a"), []byte("bb"), []byte("ccc")}, frames)

	// Byte cap: enqueue 3 frames totalling 3+1+3+1+3 = 11 bytes incl
	// separators; with maxBytes=6 we expect the first two frames only.
	q.enqueue([]byte("xxx"))
	q.enqueue([]byte("yyy"))
	q.enqueue([]byte("zzz"))
	frames, _ = q.drain(ctx, 7) // 3 + 1 + 3 = 7 fits two; third would push past
	require.Equal(t, [][]byte{[]byte("xxx"), []byte("yyy")}, frames)

	// Remaining frame surfaces on the next drain.
	frames, _ = q.drain(ctx, 1024)
	require.Equal(t, [][]byte{[]byte("zzz")}, frames)

	// Blocking: drain with empty queue waits for enqueue.
	wait := make(chan [][]byte, 1)
	go func() {
		f, _ := q.drain(ctx, 1024)
		wait <- f
	}()
	time.Sleep(20 * time.Millisecond)
	q.enqueue([]byte("late"))
	select {
	case f := <-wait:
		require.Equal(t, [][]byte{[]byte("late")}, f)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("drain did not wake")
	}

	// Close releases blocked drain with closed=true and empty frames.
	wait2 := make(chan struct{})
	go func() {
		f, c := q.drain(ctx, 1024)
		require.Empty(t, f)
		require.True(t, c)
		close(wait2)
	}()
	time.Sleep(20 * time.Millisecond)
	q.close()
	select {
	case <-wait2:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("drain did not wake on close")
	}

	// Post-close enqueue is a no-op.
	q.enqueue([]byte("ignored"))
	frames, closed = q.drain(ctx, 1024)
	require.True(t, closed)
	require.Empty(t, frames)
}

// TestPollingConnectInvalidNamespace verifies that a polling CONNECT
// with a malformed namespace yields a CONNECT_ERROR frame to an
// in-flight long-poll and tears the session down.
func TestPollingConnectInvalidNamespace(t *testing.T) {
	resetSIOGlobals(t)
	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)

	// Start the long-poll BEFORE POST so the CONNECT_ERROR enqueued by
	// the dispatch path is delivered to a live GET; otherwise
	// disconnected() removes the session from the pool before the next
	// GET can attach.
	pollResult := make(chan []byte, 1)
	go func() {
		body, _ := pollGet(t, c, sid)
		pollResult <- body
	}()
	time.Sleep(50 * time.Millisecond)

	// Namespace containing a space - rejected by isValidNamespace.
	_, st := pollPost(t, c, sid, []byte("40/bad ns,"))
	require.Equal(t, http.StatusOK, st)

	select {
	case body := <-pollResult:
		require.Contains(t, string(body), "Invalid namespace",
			"expected CONNECT_ERROR with 'Invalid namespace' in drain, got %q", body)
	case <-time.After(2 * time.Second):
		t.Fatal("long-poll did not return after invalid CONNECT")
	}

	// Subsequent GET sees unknown sid: the session was torn down.
	_, status := pollGet(t, c, sid)
	require.Equal(t, http.StatusBadRequest, status)
}

// TestPollingConnectInvalidAuth verifies that a polling CONNECT with a
// non-object auth payload is rejected with CONNECT_ERROR and disconnect.
func TestPollingConnectInvalidAuth(t *testing.T) {
	resetSIOGlobals(t)
	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)

	pollResult := make(chan []byte, 1)
	go func() {
		body, _ := pollGet(t, c, sid)
		pollResult <- body
	}()
	time.Sleep(50 * time.Millisecond)

	// Auth payload is a JSON array, not an object: rejected.
	_, st := pollPost(t, c, sid, []byte(`40[1,2,3]`))
	require.Equal(t, http.StatusOK, st)

	select {
	case body := <-pollResult:
		require.Contains(t, string(body), "Invalid auth payload",
			"expected CONNECT_ERROR with auth message in drain, got %q", body)
	case <-time.After(2 * time.Second):
		t.Fatal("long-poll did not return after invalid CONNECT")
	}
}

// TestPollingBatchedPostStopsAfterCallbackDisconnect verifies that when the
// first CONNECT packet in an RS-separated polling POST is rejected by the
// deferred New callback, later packets in that same POST are not dispatched.
func TestPollingBatchedPostStopsAfterCallbackDisconnect(t *testing.T) {
	resetSIOGlobals(t)

	helloCh := make(chan []byte, 1)
	On("hello", func(p *EventPayload) {
		select {
		case helloCh <- append([]byte(nil), p.Data...):
		default:
		}
	})

	_, c, td := newPollingTestServer(t, func(kws *Websocket) {
		if string(kws.HandshakeAuth()) == `{"token":"deny"}` {
			kws.Close()
		}
	})
	defer td()

	sid, _, _ := pollOpen(t, c)

	body := append([]byte(`40{"token":"deny"}`), eioPacketSeparator)
	body = append(body, []byte(`42["hello","world"]`)...)

	resp, status := pollPost(t, c, sid, body)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "ok", string(resp))

	select {
	case data := <-helloCh:
		t.Fatalf("batched event dispatched after callback disconnect: %q", data)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestPollingConnectFiresOnceOnRepeat verifies the connectFired CAS
// gates EventConnect to fire exactly once even if the client sends
// multiple SIO CONNECT packets.
func TestPollingConnectFiresOnceOnRepeat(t *testing.T) {
	resetSIOGlobals(t)

	var fires int32
	On(EventConnect, func(_ *EventPayload) { atomic.AddInt32(&fires, 1) })

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	_, _ = pollPost(t, c, sid, []byte(`40`))
	_, _ = pollPost(t, c, sid, []byte(`40`))
	_, _ = pollPost(t, c, sid, []byte(`40{"x":1}`))

	require.Eventually(t, func() bool { return atomic.LoadInt32(&fires) >= 1 },
		2*time.Second, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond) // settle any in-flight extras
	require.EqualValues(t, 1, atomic.LoadInt32(&fires),
		"EventConnect must fire exactly once across repeated CONNECT packets")
}

// TestPollingConnectNoAuth verifies the auth-absent path yields a nil
// HandshakeAuth on the EventConnect payload.
func TestPollingConnectNoAuth(t *testing.T) {
	resetSIOGlobals(t)

	authCh := make(chan json.RawMessage, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case authCh <- p.HandshakeAuth:
		default:
		}
	})

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	_, _ = pollPost(t, c, sid, []byte(`40`))

	select {
	case auth := <-authCh:
		require.Nil(t, auth, "expected nil HandshakeAuth for connect without auth")
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire")
	}
}

// TestPollingBinaryInbound verifies that a "b<base64>" packet on POST
// decodes and surfaces as EventMessage with the raw bytes.
func TestPollingBinaryInbound(t *testing.T) {
	resetSIOGlobals(t)

	gotCh := make(chan []byte, 1)
	On(EventMessage, func(p *EventPayload) {
		select {
		case gotCh <- append([]byte(nil), p.Data...):
		default:
		}
	})

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	_, _ = pollPost(t, c, sid, []byte(`40`))

	// "hello" base64-encoded with the "b" prefix.
	post := append([]byte("b"), []byte("aGVsbG8=")...)
	_, st := pollPost(t, c, sid, post)
	require.Equal(t, http.StatusOK, st)

	select {
	case data := <-gotCh:
		require.Equal(t, "hello", string(data))
	case <-time.After(2 * time.Second):
		t.Fatal("binary inbound not surfaced as EventMessage")
	}
}

// TestPollingBatchedBinaryStopsAfterDisconnect verifies that when a binary
// polling packet listener closes the session, later packets in the same
// RS-separated POST body are not dispatched.
func TestPollingBatchedBinaryStopsAfterDisconnect(t *testing.T) {
	resetSIOGlobals(t)

	gotBinary := make(chan []byte, 1)
	helloCh := make(chan []byte, 1)
	On(EventMessage, func(p *EventPayload) {
		select {
		case gotBinary <- append([]byte(nil), p.Data...):
		default:
		}
		p.Kws.Close()
	})
	On("hello", func(p *EventPayload) {
		select {
		case helloCh <- append([]byte(nil), p.Data...):
		default:
		}
	})

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	_, _ = pollPost(t, c, sid, []byte(`40`))

	body := append([]byte("b"), []byte(base64.StdEncoding.EncodeToString([]byte("hello")))...)
	body = append(body, eioPacketSeparator)
	body = append(body, []byte(`42["hello","world"]`)...)

	resp, status := pollPost(t, c, sid, body)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "ok", string(resp))

	select {
	case data := <-gotBinary:
		require.Equal(t, "hello", string(data))
	case <-time.After(2 * time.Second):
		t.Fatal("binary inbound not surfaced as EventMessage")
	}

	select {
	case data := <-helloCh:
		t.Fatalf("batched event dispatched after binary disconnect: %q", data)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestPollingEmitWithAckRoundTrip verifies a server-initiated
// EmitWithAck round-trips its callback id over polling: the event
// frame is delivered on a GET, and the client's POST of the matching
// "43<id>[result]" frame fires the callback.
func TestPollingEmitWithAckRoundTrip(t *testing.T) {
	resetSIOGlobals(t)

	kwsCh := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case kwsCh <- p.Kws:
		default:
		}
	})

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	_, _ = pollPost(t, c, sid, []byte(`40`))

	var kws *Websocket
	select {
	case kws = <-kwsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("never connected")
	}
	// Drain the connect ack.
	_, _ = pollGet(t, c, sid)

	ackCh := make(chan []byte, 1)
	kws.EmitWithAck("question", []byte(`"q"`), func(ack []byte) {
		select {
		case ackCh <- append([]byte(nil), ack...):
		default:
		}
	})

	body, _ := pollGet(t, c, sid)
	// Find "42<id>[\"question\",\"q\"]" frame and parse the id.
	frames := splitPollFrames(body)
	var ackID string
	for _, f := range frames {
		if len(f) >= 2 && f[0] == eioMessage && f[1] == sioEvent {
			rest := string(f[2:])
			brk := strings.IndexByte(rest, '[')
			require.GreaterOrEqual(t, brk, 1, "no ack id present in %q", f)
			ackID = rest[:brk]
			break
		}
	}
	require.NotEmpty(t, ackID, "could not find event frame in drain: %q", body)

	// Client replies with "43<id>[\"answer\"]".
	_, st := pollPost(t, c, sid, []byte(fmt.Sprintf(`43%s["answer"]`, ackID)))
	require.Equal(t, http.StatusOK, st)

	select {
	case ack := <-ackCh:
		require.Contains(t, string(ack), "answer")
	case <-time.After(2 * time.Second):
		t.Fatal("ack callback did not fire")
	}
}

// TestPollingBatchedEmitsRSSeparated verifies that several Emits issued
// before any GET are batched into a single response separated by RS.
func TestPollingBatchedEmitsRSSeparated(t *testing.T) {
	resetSIOGlobals(t)

	kwsCh := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case kwsCh <- p.Kws:
		default:
		}
	})

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	_, _ = pollPost(t, c, sid, []byte(`40`))
	var kws *Websocket
	select {
	case kws = <-kwsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire")
	}
	// Drain the connect ack first.
	_, _ = pollGet(t, c, sid)

	kws.EmitEvent("a", []byte(`1`))
	kws.EmitEvent("b", []byte(`2`))
	kws.EmitEvent("c", []byte(`3`))

	body, _ := pollGet(t, c, sid)
	frames := splitPollFrames(body)
	require.GreaterOrEqual(t, len(frames), 3,
		"expected >= 3 batched frames, got %d in %q", len(frames), body)
}

// TestPollingMaxBufferSizeRejected verifies oversized POST bodies are
// rejected with engine.io code 3.
func TestPollingMaxBufferSizeRejected(t *testing.T) {
	resetSIOGlobals(t)

	prev := PollingMaxBufferSize
	PollingMaxBufferSize = 64
	defer func() { PollingMaxBufferSize = prev }()

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	body := append([]byte("42[\"big\",\""), bytes.Repeat([]byte{'x'}, 200)...)
	body = append(body, []byte("\"]")...)
	resp, st := pollPost(t, c, sid, body)
	require.Equal(t, http.StatusBadRequest, st)
	require.Contains(t, string(resp), `"code":3`)
}

// TestPollingMaxBufferSizeTearsDown verifies that an oversize POST body
// not only returns 400 but also disconnects the session, matching the
// WebSocket SetReadLimit behaviour. Without this, an attacker could
// keep sessions alive by repeatedly posting oversize bodies.
func TestPollingMaxBufferSizeTearsDown(t *testing.T) {
	resetSIOGlobals(t)

	prev := PollingMaxBufferSize
	PollingMaxBufferSize = 64
	t.Cleanup(func() { PollingMaxBufferSize = prev })

	disconnectErr := make(chan error, 1)
	On(EventDisconnect, func(p *EventPayload) {
		select {
		case disconnectErr <- p.Error:
		default:
		}
	})

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	body := append([]byte("42[\"big\",\""), bytes.Repeat([]byte{'x'}, 200)...)
	body = append(body, []byte("\"]")...)
	resp, st := pollPost(t, c, sid, body)
	require.Equal(t, http.StatusBadRequest, st)
	require.Contains(t, string(resp), `"code":3`)

	select {
	case err := <-disconnectErr:
		require.ErrorIs(t, err, ErrPollingBodyTooLarge)
	case <-time.After(2 * time.Second):
		t.Fatal("oversize POST did not tear session down")
	}

	// Subsequent GET sees the session gone.
	_, status := pollGet(t, c, sid)
	require.Equal(t, http.StatusBadRequest, status)
}

// TestPollingErrorBodyAllCodes covers the pollingErrorBody mapping for
// every documented engine.io error code.
func TestPollingErrorBodyAllCodes(t *testing.T) {
	cases := []struct {
		code   int
		status int
		want   string
	}{
		{0, http.StatusBadRequest, "Transport unknown"},
		{1, http.StatusBadRequest, "Session ID unknown"},
		{2, http.StatusBadRequest, "Bad handshake method"},
		{3, http.StatusBadRequest, "Bad request"},
		{4, http.StatusForbidden, "Forbidden"},
		{5, http.StatusBadRequest, "Unsupported protocol version"},
		{99, http.StatusBadRequest, "Unknown error"},
	}
	for _, tc := range cases {
		st, body := pollingErrorBody(tc.code)
		require.Equal(t, tc.status, st)
		require.Contains(t, body, tc.want, "code=%d body=%q", tc.code, body)
		require.Contains(t, body, fmt.Sprintf(`"code":%d`, tc.code))
	}
}

// TestPollQueueOversizeFrameEmittedAlone covers the docstring guarantee:
// a single frame larger than maxBytes is emitted alone instead of
// blocking forever.
func TestPollQueueOversizeFrameEmittedAlone(t *testing.T) {
	q := newPollQueue()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	big := bytes.Repeat([]byte{'x'}, 4096)
	q.enqueue(big)
	frames, _ := q.drain(ctx, 256) // maxBytes is 256, but the frame is 4096
	require.Len(t, frames, 1)
	require.Equal(t, big, frames[0])
}

// TestPollingHandshakeTimeoutEnforced verifies a polling client that
// opens a session and never POSTs SIO CONNECT is torn down within the
// configured HandshakeTimeout, parity with the WebSocket path.
func TestPollingHandshakeTimeoutEnforced(t *testing.T) {
	resetSIOGlobals(t)

	prev := HandshakeTimeout
	HandshakeTimeout = 80 * time.Millisecond
	defer func() { HandshakeTimeout = prev }()

	disconnected := make(chan error, 1)
	On(EventDisconnect, func(p *EventPayload) {
		select {
		case disconnected <- p.Error:
		default:
		}
	})

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)

	// Do not POST "40". Wait for the handshake timer to fire.
	select {
	case err := <-disconnected:
		require.ErrorIs(t, err, ErrHandshakeClosed)
	case <-time.After(2 * time.Second):
		t.Fatal("polling handshake timer did not tear session down")
	}

	// Subsequent GET sees the session gone.
	_, status := pollGet(t, c, sid)
	require.Equal(t, http.StatusBadRequest, status)
}

// TestPollingHeartbeatTimeoutTearsDown verifies that a polling session
// torn down with ErrHeartbeatTimeout (as the pong goroutine would call
// when the client stops responding) cleans up the session, drains any
// in-flight long-poll, and rejects subsequent GETs with engine.io code
// 1.
//
// We invoke disconnected directly rather than waiting for the real
// heartbeat enforcer, mirroring the same pattern used by
// TestSocketIOHeartbeat to avoid racing previously-spawned pong
// goroutines on the package-level PingInterval/PingTimeout globals.
func TestPollingHeartbeatTimeoutTearsDown(t *testing.T) {
	resetSIOGlobals(t)

	disconnectErr := make(chan error, 1)
	On(EventDisconnect, func(p *EventPayload) {
		select {
		case disconnectErr <- p.Error:
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

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	_, _ = pollPost(t, c, sid, []byte(`40`))
	var kws *Websocket
	select {
	case kws = <-kwsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire")
	}
	// Drain the connect ack first so the next GET will block on the
	// queue and be released by disconnected.
	_, _ = pollGet(t, c, sid)

	// Start an in-flight long-poll, then simulate a heartbeat timeout
	// from another goroutine (the production pong goroutine would do
	// the same on PingInterval+PingTimeout expiry).
	pollResp := make(chan int, 1)
	go func() {
		_, st := pollGet(t, c, sid)
		pollResp <- st
	}()
	time.Sleep(50 * time.Millisecond)
	kws.disconnected(ErrHeartbeatTimeout)

	select {
	case err := <-disconnectErr:
		require.ErrorIs(t, err, ErrHeartbeatTimeout)
	case <-time.After(2 * time.Second):
		t.Fatal("EventDisconnect did not fire with ErrHeartbeatTimeout")
	}

	// The blocked long-poll wakes up and returns code 1 (empty buffer
	// + closed flag).
	select {
	case st := <-pollResp:
		require.Equal(t, http.StatusBadRequest, st,
			"long-poll should report unknown sid after heartbeat timeout")
	case <-time.After(2 * time.Second):
		t.Fatal("long-poll did not return after heartbeat timeout")
	}

	// Subsequent GET also reports unknown sid.
	_, status := pollGet(t, c, sid)
	require.Equal(t, http.StatusBadRequest, status)
}

// TestPollingGateReleasedBeforeBodyWrite verifies that the long-poll
// gate is released BEFORE the response body is written, so a slow client
// (or any post-drain blocking) does not strand legitimate retry GETs.
func TestPollingGateReleasedBeforeBodyWrite(t *testing.T) {
	resetSIOGlobals(t)

	kwsCh := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case kwsCh <- p.Kws:
		default:
		}
	})

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	_, _ = pollPost(t, c, sid, []byte(`40`))
	var kws *Websocket
	select {
	case kws = <-kwsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire")
	}
	_, _ = pollGet(t, c, sid) // drain CONNECT ack

	// Start a long-poll, then enqueue a frame and immediately fire a
	// second GET. The first GET should already have released the gate
	// after drain, so the second GET should not be rejected with code 3.
	first := make(chan int, 1)
	go func() {
		_, st := pollGet(t, c, sid)
		first <- st
	}()
	time.Sleep(50 * time.Millisecond)
	kws.EmitEvent("ping", []byte(`1`))
	require.Equal(t, http.StatusOK, <-first)

	// Now emit again and immediately fire two parallel polls. The
	// gate's early-release should let one of them succeed without
	// blocking on the other.
	kws.EmitEvent("ping", []byte(`2`))
	g1 := make(chan int, 1)
	g2 := make(chan int, 1)
	go func() {
		_, st := pollGet(t, c, sid)
		g1 <- st
	}()
	go func() {
		_, st := pollGet(t, c, sid)
		g2 <- st
	}()
	// One returns 200 (drained the queued frame), the other may return
	// either 200 (drained empty after timeout) or 400 (concurrent gate).
	// The invariant we test: the WINNER is unblocked promptly. Both
	// channels MUST be drained so the goroutines do not outlive the
	// test (writing to a closed t via require would race the runtime).
	deadline := time.After(MaxPollWait + 2*time.Second)
	for got := 0; got < 2; got++ {
		select {
		case st := <-g1:
			require.Contains(t, []int{http.StatusOK, http.StatusBadRequest}, st)
		case st := <-g2:
			require.Contains(t, []int{http.StatusOK, http.StatusBadRequest}, st)
		case <-deadline:
			t.Fatalf("only %d/2 GETs completed; gate likely still held", got)
		}
	}
}

// TestPollingBinaryOutboundEncoded verifies that Emit(data, BinaryMessage)
// on a polling session encodes the bytes as the engine.io v4 polling
// "b<base64>" text packet rather than enqueuing raw binary.
func TestPollingBinaryOutboundEncoded(t *testing.T) {
	resetSIOGlobals(t)

	kwsCh := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case kwsCh <- p.Kws:
		default:
		}
	})

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	_, _ = pollPost(t, c, sid, []byte(`40`))
	var kws *Websocket
	select {
	case kws = <-kwsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire")
	}
	_, _ = pollGet(t, c, sid) // drain CONNECT ack

	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	kws.Emit(payload, BinaryMessage)

	body, _ := pollGet(t, c, sid)
	frames := splitPollFrames(body)
	var sawBinary bool
	for _, f := range frames {
		if len(f) > 1 && f[0] == 'b' {
			dec := make([]byte, base64.StdEncoding.DecodedLen(len(f)-1))
			n, derr := base64.StdEncoding.Decode(dec, f[1:])
			require.NoError(t, derr)
			require.Equal(t, payload, dec[:n])
			sawBinary = true
			break
		}
	}
	require.Truef(t, sawBinary, "expected b<base64> frame in drain, got %q", body)
}

// TestPollingQueueOverflowDrop verifies that with DropFramesOnOverflow
// true and PollQueueMaxFrames small, excess emits drop the offending
// frame and surface ErrSendQueueOverflow without tearing the session
// down.
func TestPollingQueueOverflowDrop(t *testing.T) {
	resetSIOGlobals(t)

	// Restore via t.Cleanup (runs AFTER all defers, so AFTER td()
	// shuts down the test server and AFTER resetSIOGlobals' workersWg
	// wait). Plain defer here would restore globals BEFORE the pong
	// goroutine has actually exited, racing it on PollQueueMaxFrames
	// / DropFramesOnOverflow.
	prevCap, prevDrop := PollQueueMaxFrames, DropFramesOnOverflow
	PollQueueMaxFrames = 4
	DropFramesOnOverflow = true
	t.Cleanup(func() {
		PollQueueMaxFrames = prevCap
		DropFramesOnOverflow = prevDrop
	})

	overflow := make(chan struct{}, 16)
	On(EventError, func(p *EventPayload) {
		if errors.Is(p.Error, ErrSendQueueOverflow) {
			select {
			case overflow <- struct{}{}:
			default:
			}
		}
	})

	kwsCh := make(chan *Websocket, 1)
	On(EventConnect, func(p *EventPayload) {
		select {
		case kwsCh <- p.Kws:
		default:
		}
	})

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	_, _ = pollPost(t, c, sid, []byte(`40`))
	var kws *Websocket
	select {
	case kws = <-kwsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire")
	}
	_, _ = pollGet(t, c, sid)

	// Emit more frames than the queue can hold. Without GETs to drain
	// them, the queue saturates after PollQueueMaxFrames.
	for i := 0; i < 20; i++ {
		kws.EmitEvent("e", []byte(`1`))
	}

	require.Eventually(t, func() bool { return len(overflow) > 0 },
		2*time.Second, 10*time.Millisecond,
		"expected ErrSendQueueOverflow event after exceeding cap")
	require.True(t, kws.IsAlive(),
		"DropFramesOnOverflow=true must keep the session alive")
}

// TestPollingQueueOverflowDisconnect verifies that with the default
// DropFramesOnOverflow=false, exceeding PollQueueMaxFrames tears the
// session down with ErrSendQueueClosed.
func TestPollingQueueOverflowDisconnect(t *testing.T) {
	resetSIOGlobals(t)

	// See TestPollingQueueOverflowDrop for why t.Cleanup is required
	// instead of a plain defer.
	prevCap := PollQueueMaxFrames
	PollQueueMaxFrames = 4
	t.Cleanup(func() { PollQueueMaxFrames = prevCap })

	disconnectErr := make(chan error, 1)
	On(EventDisconnect, func(p *EventPayload) {
		select {
		case disconnectErr <- p.Error:
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

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	_, _ = pollPost(t, c, sid, []byte(`40`))
	var kws *Websocket
	select {
	case kws = <-kwsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire")
	}
	_, _ = pollGet(t, c, sid)

	for i := 0; i < 20; i++ {
		kws.EmitEvent("e", []byte(`1`))
	}

	select {
	case err := <-disconnectErr:
		require.ErrorIs(t, err, ErrSendQueueClosed)
	case <-time.After(2 * time.Second):
		t.Fatal("queue overflow did not disconnect session")
	}
}

// TestPollingHandshakeTimerStoppedAfterConnect verifies the polling
// HandshakeTimeout AfterFunc is stopped as soon as SIO CONNECT arrives, so the
// closure does not pin the *Websocket on the runtime timer heap after the
// handshake has completed.
func TestPollingHandshakeTimerStoppedAfterConnect(t *testing.T) {
	resetSIOGlobals(t)

	prevHS := HandshakeTimeout
	HandshakeTimeout = 5 * time.Second
	t.Cleanup(func() { HandshakeTimeout = prevHS })

	disconnected := make(chan struct{}, 1)
	On(EventDisconnect, func(_ *EventPayload) {
		select {
		case disconnected <- struct{}{}:
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

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {})
	defer td()

	sid, _, _ := pollOpen(t, c)
	_, _ = pollPost(t, c, sid, []byte(`40`))
	var kws *Websocket
	select {
	case kws = <-kwsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("EventConnect did not fire")
	}
	timer := kws.handshakeTimer.Load()
	require.NotNil(t, timer, "polling session should schedule handshakeTimer")

	kws.Close()
	<-disconnected

	// Stop returns false in two cases: timer was already stopped, OR it had
	// already fired and run its closure. Calling Stop here after CONNECT and
	// Close discriminates: if the CONNECT path stopped the timer, this returns
	// false and the AfterFunc closure never gets a chance to run. We assert
	// both: timer.Stop()==false AND no second EventDisconnect fired.
	require.False(t, timer.Stop(),
		"handshakeTimer should be stopped after SIO CONNECT")
	select {
	case <-disconnected:
		t.Fatal("handshakeTimer fired after Close; timer was not stopped")
	case <-time.After(50 * time.Millisecond):
		// expected: no second disconnect event
	}
}

// TestPollingCallbackPanicCleansUp verifies that a panic in the user callback
// during the first SIO CONNECT runs disconnected() so the lifecycle goroutine,
// pong goroutine, and pool entry do not leak.
func TestPollingCallbackPanicCleansUp(t *testing.T) {
	resetSIOGlobals(t)

	_, c, td := newPollingTestServer(t, func(_ *Websocket) {
		panic("intentional callback panic")
	})
	defer td()

	sid, _, status := pollOpen(t, c)
	require.Equal(t, http.StatusOK, status)
	_, status = pollPost(t, c, sid, []byte(`40`))
	require.Equal(t, http.StatusOK, status)

	require.Eventually(t, func() bool {
		for _, w := range pool.all() {
			if k, ok := w.(*Websocket); ok && k.pollQ != nil {
				return false
			}
		}
		return true
	}, 2*time.Second, 10*time.Millisecond,
		"pool should not retain polling session after callback panic")
}

// TestPollingTransportMismatch verifies that a polling request whose sid
// resolves to a WebSocket-only session returns engine.io code 3.
func TestPollingTransportMismatch(t *testing.T) {
	resetSIOGlobals(t)
	prev := EnablePolling
	EnablePolling = true
	defer func() { EnablePolling = prev }()

	// Build a fake WS-only entry by inserting a plain *Websocket without
	// pollQ into the pool under a known sid.
	const sid = "deadbeef-0000-0000-0000-000000000001"
	kws := &Websocket{UUID: sid}
	kws.isAlive.Store(true)
	pool.set(kws)
	defer pool.delete(sid)

	app := fiber.New()
	ln := fasthttputil.NewInmemoryListener()
	app.Get("/", New(func(_ *Websocket) {}))
	go func() { _ = app.Listener(ln) }()
	defer func() { _ = app.Shutdown(); _ = ln.Close() }()

	c := &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return ln.Dial()
		},
	}, Timeout: 5 * time.Second}

	url := fmt.Sprintf("http://test/?EIO=4&transport=polling&sid=%s", sid)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, string(body), `"code":3`)
}
