package websocket

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestWebSocketMiddlewareDefaultConfig(t *testing.T) {
	app := setupTestApp(Config{}, nil)
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
	defer conn.Close()
	assert.NoError(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.NoError(t, err)
	assert.Equal(t, "hello websocket", msg["message"])
}

func TestWebSocketMiddlewareConfigOrigin(t *testing.T) {
	t.Run("allow all origins", func(t *testing.T) {
		app := setupTestApp(Config{
			Origins: []string{"*"},
		}, nil)
		defer app.Shutdown()

		conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", http.Header{
			"Origin": []string{"http://localhost:3000"},
		})
		defer conn.Close()
		assert.NoError(t, err)
		assert.Equal(t, fiber.StatusSwitchingProtocols, resp.StatusCode)
		assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

		var msg fiber.Map
		err = conn.ReadJSON(&msg)
		assert.Nil(t, err)
		assert.Equal(t, "hello websocket", msg["message"])
	})

	t.Run("allowed origin", func(t *testing.T) {
		app := setupTestApp(Config{
			Origins: []string{"http://localhost:3000"},
		}, nil)
		defer app.Shutdown()
		conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", http.Header{
			"Origin": []string{"http://localhost:3000"},
		})
		defer conn.Close()
		assert.NoError(t, err)
		assert.Equal(t, fiber.StatusSwitchingProtocols, resp.StatusCode)
		assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

		var msg fiber.Map
		err = conn.ReadJSON(&msg)
		assert.NoError(t, err)
		assert.Equal(t, "hello websocket", msg["message"])
	})

	t.Run("empty origin rejected by default", func(t *testing.T) {
		app := setupTestApp(Config{
			Origins: []string{"http://localhost:3000"},
		}, nil)
		defer app.Shutdown()
		conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
		if conn != nil {
			defer conn.Close()
		}
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "bad handshake")
		// RFC 6455 section 4.2.2: an origin the server does not accept is
		// answered with 403, and section 4.4 asks a rejecting server to
		// advertise the versions it understands.
		assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
		assert.Equal(t, "13", resp.Header.Get(fiber.HeaderSecWebSocketVersion))
		assert.Equal(t, "", resp.Header.Get("Upgrade"))

		assert.Nil(t, conn)
	})

	t.Run("empty origin allowed with config", func(t *testing.T) {
		app := setupTestApp(Config{
			Origins:          []string{"http://localhost:3000"},
			AllowEmptyOrigin: true,
		}, nil)
		defer app.Shutdown()
		conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
		defer conn.Close()
		assert.NoError(t, err)
		assert.Equal(t, fiber.StatusSwitchingProtocols, resp.StatusCode)
		assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

		var msg fiber.Map
		err = conn.ReadJSON(&msg)
		assert.NoError(t, err)
		assert.Equal(t, "hello websocket", msg["message"])
	})

	t.Run("wildcard in list", func(t *testing.T) {
		app := setupTestApp(Config{
			Origins: []string{"http://localhost:3000", "*"},
		}, nil)
		defer app.Shutdown()
		conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", http.Header{
			"Origin": []string{"http://localhost:5000"},
		})
		if !assert.NoError(t, err) {
			return
		}
		defer conn.Close()
		assert.Equal(t, fiber.StatusSwitchingProtocols, resp.StatusCode)
		assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

		var msg fiber.Map
		err = conn.ReadJSON(&msg)
		assert.NoError(t, err)
		assert.Equal(t, "hello websocket", msg["message"])
	})

	t.Run("wildcard in list allows empty origin", func(t *testing.T) {
		app := setupTestApp(Config{
			Origins: []string{"http://localhost:3000", "*"},
		}, nil)
		defer app.Shutdown()
		// Explicitly test with no Origin header (nil headers = no Origin sent)
		conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
		if !assert.NoError(t, err) {
			return
		}
		defer conn.Close()
		assert.Equal(t, fiber.StatusSwitchingProtocols, resp.StatusCode)
		assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

		var msg fiber.Map
		err = conn.ReadJSON(&msg)
		assert.NoError(t, err)
		assert.Equal(t, "hello websocket", msg["message"])
	})

	t.Run("disallowed origin", func(t *testing.T) {
		app := setupTestApp(Config{
			Origins: []string{"http://localhost:3000"},
		}, nil)
		defer app.Shutdown()
		conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", http.Header{
			"Origin": []string{"http://localhost:5000"},
		})
		defer conn.Close()
		assert.Equal(t, err.Error(), "websocket: bad handshake")
		assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
		assert.Equal(t, "13", resp.Header.Get(fiber.HeaderSecWebSocketVersion))
		assert.Equal(t, "", resp.Header.Get("Upgrade"))

		assert.Nil(t, conn)
	})
}

func TestWebSocketMiddlewareBufferSize(t *testing.T) {
	app := setupTestApp(Config{
		Origins:         []string{"*"},
		WriteBufferSize: 10,
	}, nil)
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
	defer conn.Close()
	assert.NoError(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.NoError(t, err)
	assert.Equal(t, "hello websocket", msg["message"])
}

func TestWebSocketConnParams(t *testing.T) {
	app := setupTestApp(Config{}, func(c *Conn) {
		param1 := c.Params("param1")
		param2 := c.Params("param2")
		paramDefault := c.Params("paramDefault", "default")

		assert.Equal(t, "value1", param1)
		assert.Equal(t, "value2", param2)
		assert.Equal(t, "default", paramDefault)

		c.WriteJSON(fiber.Map{
			"message": "hello websocket",
		})
	})
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message/value1/value2", nil)
	defer conn.Close()
	assert.NoError(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.NoError(t, err)
	assert.Equal(t, "hello websocket", msg["message"])
}

func TestWebSocketConnQuery(t *testing.T) {
	app := setupTestApp(Config{}, func(c *Conn) {
		query1 := c.Query("query1")
		query2 := c.Query("query2")
		queryDefault := c.Query("queryDefault", "default")

		assert.Equal(t, "value1", query1)
		assert.Equal(t, "value2", query2)
		assert.Equal(t, "default", queryDefault)

		c.WriteJSON(fiber.Map{
			"message": "hello websocket",
		})
	})
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message?query1=value1&query2=value2", nil)
	defer conn.Close()
	assert.NoError(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.NoError(t, err)
	assert.Equal(t, "hello websocket", msg["message"])
}

func TestWebSocketConnHeaders(t *testing.T) {
	app := setupTestApp(Config{}, func(c *Conn) {
		header1 := c.Headers("Header1")
		header2 := c.Headers("Header2")
		contentType := c.Headers("Content-Type")
		headerDefault := c.Headers("HeaderDefault", "valueDefault")

		assert.Equal(t, "value1", header1)
		assert.Equal(t, "value2", header2)
		assert.Equal(t, "application/json", contentType)
		assert.Equal(t, "valueDefault", headerDefault)

		c.WriteJSON(fiber.Map{
			"message": "hello websocket",
		})
	})
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", http.Header{
		"header1":      []string{"value1"},
		"header2":      []string{"value2"},
		"content-type": []string{"application/json"},
	})
	defer conn.Close()
	assert.NoError(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.NoError(t, err)
	assert.Equal(t, "hello websocket", msg["message"])
}

func TestWebSocketConnCookies(t *testing.T) {
	app := setupTestApp(Config{}, func(c *Conn) {
		cookie1 := c.Cookies("Cookie1")
		cookie2 := c.Cookies("Cookie2")
		cookieDefault := c.Headers("CookieDefault", "valueDefault")

		assert.Equal(t, "value1", cookie1)
		assert.Equal(t, "value2", cookie2)
		assert.Equal(t, "valueDefault", cookieDefault)

		c.WriteJSON(fiber.Map{
			"message": "hello websocket",
		})
	})
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", http.Header{
		"header1": []string{"value1"},
		"header2": []string{"value2"},
		"Cookie":  []string{"Cookie1=value1; Cookie2=value2"},
	})
	defer conn.Close()
	assert.NoError(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.NoError(t, err)
	assert.Equal(t, "hello websocket", msg["message"])
}

func TestWebSocketConnLocals(t *testing.T) {
	app := setupTestApp(Config{}, func(c *Conn) {
		local1 := c.Locals("local1")
		local2 := c.Locals("local2")

		assert.Equal(t, "value1", local1)
		assert.Equal(t, "value2", local2)

		c.WriteJSON(fiber.Map{
			"message": "hello websocket",
		})
	})
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
	defer conn.Close()
	assert.NoError(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.NoError(t, err)
	assert.Equal(t, "hello websocket", msg["message"])
}

func TestWebSocketConnIP(t *testing.T) {
	app := setupTestApp(Config{}, func(c *Conn) {
		ip := c.IP()

		assert.Equal(t, "127.0.0.1", ip)

		c.WriteJSON(fiber.Map{
			"message": "hello websocket",
		})
	})
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
	defer conn.Close()
	assert.NoError(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.NoError(t, err)
	assert.Equal(t, "hello websocket", msg["message"])
}

// TestWebSocketConnIPSafeCopy verifies that conn.IP() returns a safe copy
// that is not corrupted when fasthttp reuses its internal buffer for
// subsequent requests. See: gofiber/fiber#4208, gofiber/contrib#1800
func TestWebSocketConnIPSafeCopy(t *testing.T) {
	const iterations = 5
	ips := make(chan string, iterations)

	app := setupTestApp(Config{}, func(c *Conn) {
		// Read the IP and send it back; the value must remain "127.0.0.1"
		// even after fasthttp recycles the underlying request buffer.
		ips <- c.IP()
		c.WriteJSON(fiber.Map{"ip": c.IP()})
	})
	defer app.Shutdown()

	for i := 0; i < iterations; i++ {
		conn, _, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
		require.NoError(t, err)
		var msg fiber.Map
		err = conn.ReadJSON(&msg)
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1", msg["ip"])
		conn.Close()
	}

	close(ips)
	for ip := range ips {
		assert.Equal(t, "127.0.0.1", ip, "conn.IP() must be a safe copy, not a reference to recycled fasthttp buffer")
	}
}

func TestWebSocketCompressionAfterHandlerReturns(t *testing.T) {
	writeErr := make(chan error, 1)
	handlerReturning := make(chan struct{})
	app := setupTestApp(Config{
		EnableCompression: true,
	}, func(c *Conn) {
		defer close(handlerReturning)
		conn := c.Conn
		go func() {
			<-handlerReturning
			conn.EnableWriteCompression(true)
			if err := conn.SetCompressionLevel(2); err != nil {
				writeErr <- err
				return
			}
			writeErr <- conn.WriteJSON(fiber.Map{"message": "hello websocket"})
		}()
	})
	defer app.Shutdown()

	dialer := websocket.Dialer{
		EnableCompression: true,
	}
	conn, resp, err := dialer.Dial("ws://localhost:3000/ws/message", nil)
	require.NoError(t, err)
	defer conn.Close()
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))
	assert.Contains(t, resp.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate")

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	require.NoError(t, err)
	assert.Equal(t, "hello websocket", msg["message"])

	select {
	case err := <-writeErr:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for async compressed write")
	}
}

func setupTestApp(cfg Config, h func(c *Conn)) *fiber.App {
	var handler fiber.Handler
	if h == nil {
		handler = New(func(c *Conn) {
			c.WriteJSON(fiber.Map{
				"message": "hello websocket",
			})
		}, cfg)
	} else {
		handler = New(h, cfg)
	}

	app := fiber.New(fiber.Config{})

	app.Use("/ws", func(c fiber.Ctx) error {
		if IsWebSocketUpgrade(c) {
			fiber.StoreInContext(c, "allowed", true)
			fiber.StoreInContext(c, "local1", "value1")
			fiber.StoreInContext(c, "local2", "value2")
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})

	app.Get("/ws/message", handler)
	app.Get("/ws/message/:param1/:param2", handler)
	go app.Listen(":3000", fiber.ListenConfig{DisableStartupMessage: true})

	readyCh := make(chan struct{})

	go func() {
		for {
			conn, err := net.Dial("tcp", "localhost:3000")
			if err != nil {
				continue
			}

			if conn != nil {
				readyCh <- struct{}{}
				conn.Close()
				break
			}
		}
	}()

	<-readyCh

	return app
}

func TestWebSocketIsCloseError(t *testing.T) {
	closeError := IsCloseError(&websocket.CloseError{
		Code: websocket.CloseNormalClosure,
	}, websocket.CloseNormalClosure)
	assert.Equal(t, true, closeError)
}

func TestWebSocketIsUnexpectedCloseError(t *testing.T) {
	closeError := IsUnexpectedCloseError(&websocket.CloseError{
		Code: websocket.CloseNormalClosure,
	}, websocket.CloseAbnormalClosure)
	assert.Equal(t, true, closeError)
}

func TestWebSocketFormatCloseMessage(t *testing.T) {
	closeMsg := FormatCloseMessage(websocket.CloseNormalClosure, "test")

	assert.Equal(t, []byte{0x3, 0xe8, 0x74, 0x65, 0x73, 0x74}, closeMsg)
}

func TestWebsocketRecoverDefaultHandlerShouldNotPanic(t *testing.T) {
	app := setupTestApp(Config{}, func(c *Conn) {
		panic("test panic")
	})
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
	defer conn.Close()
	assert.NoError(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.NoError(t, err)
	assert.Equal(t, defaultRecoverMessage, msg["error"])
}

func TestWebsocketRecoverDoesNotLeakErrorDetail(t *testing.T) {
	// Nothing derived from the panic may reach the peer: the operator already
	// gets the full value and stack on stderr, so putting any of it in the frame
	// would only hand a remote client an oracle for internal paths, DSNs and
	// schema names. This holds for a string panic as much as for an error one,
	// since a string is sent verbatim by any encoder.
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"error", errors.New("dial tcp 10.0.3.14:5432: connection refused")},
		{"string", "loading /srv/app/conf/tenants.yaml failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := setupTestApp(Config{}, func(*Conn) {
				panic(tc.value)
			})
			defer app.Shutdown()

			conn, _, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
			require.NoError(t, err)
			defer conn.Close()

			_, raw, err := conn.ReadMessage()
			require.NoError(t, err)
			assert.JSONEq(t, `{"error":"internal error"}`, string(raw))
		})
	}
}

func TestWebsocketPanicClosesConnection(t *testing.T) {
	app := setupTestApp(Config{}, func(*Conn) {
		panic("test panic")
	})
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
	require.NoError(t, err)
	defer conn.Close()
	assert.Equal(t, fiber.StatusSwitchingProtocols, resp.StatusCode)

	// The recover handler still gets to write its error frame...
	var msg fiber.Map
	require.NoError(t, conn.ReadJSON(&msg))
	assert.Equal(t, defaultRecoverMessage, msg["error"])

	// ...and only then is the socket closed. Nothing else would ever close it:
	// KeepHijackedConns leaves that to this package. The deadline is only so a
	// regression fails instead of blocking forever; a timeout means the socket
	// was left open, which is the failure being guarded against.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, _, err = conn.ReadMessage()
	require.Error(t, err)
	var netErr net.Error
	assert.False(t, errors.As(err, &netErr) && netErr.Timeout(),
		"panicking handler left the connection open: %v", err)
}

func TestWebsocketRecoverCustomHandlerShouldNotPanic(t *testing.T) {
	app := setupTestApp(Config{
		RecoverHandler: func(conn *Conn) {
			if err := recover(); err != nil {
				conn.WriteJSON(fiber.Map{"customError": "error occurred"})
			}
		},
	}, func(c *Conn) {
		panic("test panic")
	})
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", nil)
	defer conn.Close()
	assert.NoError(t, err)
	assert.Equal(t, 101, resp.StatusCode)
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	var msg fiber.Map
	err = conn.ReadJSON(&msg)
	assert.NoError(t, err)
	assert.Equal(t, "error occurred", msg["customError"])
}

func TestWebSocketMiddlewareOriginCaseInsensitive(t *testing.T) {
	// RFC 6454 section 4 serializes an origin with a case-insensitive scheme
	// and host, so a configured origin must match regardless of its case.
	app := setupTestApp(Config{
		Origins: []string{"HTTP://LocalHost:3000"},
	}, nil)
	defer app.Shutdown()

	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost:3000/ws/message", http.Header{
		"Origin": []string{"http://localhost:3000"},
	})
	require.NoError(t, err)
	defer conn.Close()
	assert.Equal(t, fiber.StatusSwitchingProtocols, resp.StatusCode)

	var msg fiber.Map
	require.NoError(t, conn.ReadJSON(&msg))
	assert.Equal(t, "hello websocket", msg["message"])
}

func TestNewPanicsOnNilHandler(t *testing.T) {
	assert.Panics(t, func() {
		New(nil)
	})
}

func TestWebSocketNonUpgradeRequestIsUpgradeRequired(t *testing.T) {
	app := fiber.New()
	app.Get("/ws", New(func(*Conn) {}))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/ws", nil))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusUpgradeRequired, resp.StatusCode)
}

func TestWebSocketRejectedHandshakeKeepsUpgraderResponse(t *testing.T) {
	app := fiber.New()
	app.Get("/ws", New(func(*Conn) {}))

	// A handshake that asks for a version the server does not speak must be
	// answered with the versions it does speak (RFC 6455 section 4.4), not
	// flattened into a bare 426.
	req := httptest.NewRequest(fiber.MethodGet, "/ws", nil)
	req.Header.Set(fiber.HeaderConnection, "Upgrade")
	req.Header.Set(fiber.HeaderUpgrade, "websocket")
	req.Header.Set(fiber.HeaderSecWebSocketVersion, "12")
	req.Header.Set(fiber.HeaderSecWebSocketKey, "dGhlIHNhbXBsZSBub25jZQ==")

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "13", resp.Header.Get(fiber.HeaderSecWebSocketVersion))
}

func TestWebSocketRejectedHandshakeReachesErrorHandler(t *testing.T) {
	var handled atomic.Int32
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, err error) error {
			handled.Add(1)
			var fe *fiber.Error
			require.ErrorAs(t, err, &fe)
			return c.Status(fe.Code).JSON(fiber.Map{"reason": fe.Message})
		},
	})
	// A header set by earlier middleware, as a request-id or CORS middleware
	// would. fasthttp's ctx.Error would have reset it away.
	app.Use(func(c fiber.Ctx) error {
		c.Set("X-Request-ID", "req-1")
		return c.Next()
	})
	app.Get("/ws", New(func(*Conn) {}, Config{Origins: []string{"http://allowed"}}))

	req := httptest.NewRequest(fiber.MethodGet, "/ws", nil)
	req.Header.Set(fiber.HeaderConnection, "Upgrade")
	req.Header.Set(fiber.HeaderUpgrade, "websocket")
	req.Header.Set(fiber.HeaderSecWebSocketVersion, "13")
	req.Header.Set(fiber.HeaderSecWebSocketKey, "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set(fiber.HeaderOrigin, "http://evil")

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// The rejection is a *fiber.Error with the upgrader's status, so the
	// app's own error formatting, logging and metrics all see it.
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
	assert.Equal(t, int32(1), handled.Load())
	assert.Equal(t, fiber.MIMEApplicationJSONCharsetUTF8, resp.Header.Get(fiber.HeaderContentType))
	assert.Equal(t, "req-1", resp.Header.Get("X-Request-ID"))
	assert.Equal(t, "13", resp.Header.Get(fiber.HeaderSecWebSocketVersion))
}

func TestConnCaptureAllocatesOnlyWhatTheRequestCarries(t *testing.T) {
	// A bare handshake has headers and nothing else, so only the header map
	// exists afterwards; the accessors for the rest answer without one.
	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.Set(fiber.HeaderHost, "localhost")

	conn := &Conn{}
	conn.capture(fctx, true)

	assert.NotNil(t, conn.headers)
	assert.Nil(t, conn.locals)
	assert.Nil(t, conn.queries)
	assert.Nil(t, conn.cookies)
	assert.Equal(t, "", conn.Query("missing"))
	assert.Equal(t, "", conn.Cookies("missing"))
	assert.Nil(t, conn.Locals("missing"))
}

func TestConnHeadersLookup(t *testing.T) {
	conn := &Conn{headers: map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer token",
	}}

	assert.Equal(t, "application/json", conn.Headers("Content-Type"))
	assert.Equal(t, "application/json", conn.Headers("content-type"))
	assert.Equal(t, "application/json", conn.Headers("CONTENT-TYPE"))
	assert.Equal(t, "Bearer token", conn.Headers("authorization"))
	assert.Equal(t, "", conn.Headers("X-Missing"))
	assert.Equal(t, "fallback", conn.Headers("X-Missing", "fallback"))

}

func TestConnCaptureCanonicalizesHeaderNames(t *testing.T) {
	// With header name normalizing disabled fasthttp hands names over as they
	// arrived; capture canonicalizes them so lookups stay case-insensitive
	// without a fallback scan.
	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.DisableNormalizing()
	fctx.Request.Header.Set("x-custom", "value")

	conn := &Conn{}
	conn.capture(fctx, false)

	assert.Equal(t, "value", conn.Headers("x-custom"))
	assert.Equal(t, "value", conn.Headers("X-Custom"))
	assert.Equal(t, "value", conn.Headers("X-CUSTOM"))
}

func TestConnAccessorsOnEmptyConn(t *testing.T) {
	conn := &Conn{}

	assert.Nil(t, conn.Locals("missing"))
	assert.Equal(t, "", conn.Params("missing"))
	assert.Equal(t, "fallback", conn.Params("missing", "fallback"))
	assert.Equal(t, "", conn.Query("missing"))
	assert.Equal(t, "fallback", conn.Query("missing", "fallback"))
	assert.Equal(t, "", conn.Cookies("missing"))
	assert.Equal(t, "fallback", conn.Cookies("missing", "fallback"))
	assert.Equal(t, "", conn.Headers("missing"))

	// Setting a local on a connection that carried none allocates on demand.
	assert.Equal(t, "value", conn.Locals("key", "value"))
	assert.Equal(t, "value", conn.Locals("key"))
}

func TestCloseCodeValues(t *testing.T) {
	// RFC 6455 section 11.7 plus the later IANA registrations.
	assert.Equal(t, 1000, CloseNormalClosure)
	assert.Equal(t, 1011, CloseInternalServerErr)
	assert.Equal(t, 1012, CloseServiceRestart)
	assert.Equal(t, 1013, CloseTryAgainLater)
	assert.Equal(t, 1015, CloseTLSHandshake)
}

func BenchmarkConnHeaders(b *testing.B) {
	conn := &Conn{headers: map[string]string{
		"Host":                  "localhost:3000",
		"Connection":            "Upgrade",
		"Upgrade":               "websocket",
		"Sec-Websocket-Version": "13",
		"Sec-Websocket-Key":     "dGhlIHNhbXBsZSBub25jZQ==",
		"User-Agent":            "Go-http-client/1.1",
		"Accept-Encoding":       "gzip",
		"Authorization":         "Bearer token",
	}}

	for _, key := range []string{"Authorization", "authorization", "X-Missing"} {
		b.Run(key, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = conn.Headers(key, "fallback")
			}
		})
	}
}

// handshakeRequest is a typical browser upgrade: nine headers, two cookies,
// two query arguments and one local set by earlier middleware.
func handshakeRequest() *fasthttp.RequestCtx {
	fctx := &fasthttp.RequestCtx{}
	fctx.Request.SetRequestURI("/ws?v=1&room=lobby")
	h := &fctx.Request.Header
	h.Set(fiber.HeaderHost, "localhost:3000")
	h.Set(fiber.HeaderConnection, "Upgrade")
	h.Set(fiber.HeaderUpgrade, "websocket")
	h.Set(fiber.HeaderSecWebSocketVersion, "13")
	h.Set(fiber.HeaderSecWebSocketKey, "dGhlIHNhbXBsZSBub25jZQ==")
	h.Set(fiber.HeaderUserAgent, "Mozilla/5.0")
	h.Set(fiber.HeaderAcceptEncoding, "gzip, deflate, br")
	h.Set(fiber.HeaderOrigin, "http://localhost:3000")
	h.Set(fiber.HeaderAuthorization, "Bearer token")
	h.SetCookie("session", "abc123")
	h.SetCookie("theme", "dark")
	fctx.SetUserValue("user", "alice")
	return fctx
}

// BenchmarkNewConn is the per-upgrade cost of building a Conn: the struct plus
// only the maps the request carries.
func BenchmarkNewConn(b *testing.B) {
	fctx := handshakeRequest()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn := &Conn{ip: "127.0.0.1"}
		conn.capture(fctx, true)
	}
}
