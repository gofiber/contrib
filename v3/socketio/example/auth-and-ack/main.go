// Example: Socket.IO server with handshake auth, listener-side
// payload.Ack, server-initiated EmitWithAckTimeout, and graceful
// Shutdown(ctx) on SIGINT/SIGTERM.
//
// Client (socket.io-client v4):
//
//	const s = io("http://localhost:3000", {
//	    path: "/ws",
//	    transports: ["websocket"],
//	    auth: { token: "secret" },
//	});
//	s.emit("hello", "world", (reply) => console.log("ack:", reply));
//	s.on("ping", (n, cb) => cb("pong-" + n));
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/contrib/v3/socketio"
	"github.com/gofiber/fiber/v3"
)

func main() {
	app := fiber.New()

	// EventConnect: validate handshake auth, then kick off a server-
	// initiated ack round-trip. Closing kws here triggers a clean
	// Socket.IO disconnect on the client.
	socketio.On(socketio.EventConnect, func(ep *socketio.EventPayload) {
		var auth struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(ep.HandshakeAuth, &auth); err != nil || auth.Token != "secret" {
			log.Printf("auth rejected for %s", ep.SocketUUID)
			ep.Kws.Close()
			return
		}
		log.Printf("connected uuid=%s", ep.SocketUUID)

		// Server-initiated ack with explicit per-call timeout. The
		// client's callback bytes arrive on cb; ErrAckTimeout signals
		// the client never replied; ErrAckDisconnected signals close.
		ep.Kws.EmitWithAckTimeout("ping", []byte("1"), 3*time.Second,
			func(ack []byte, err error) {
				switch {
				case errors.Is(err, socketio.ErrAckTimeout):
					log.Printf("client did not ack in time")
				case err != nil:
					log.Printf("ack error: %v", err)
				default:
					log.Printf("got ack: %s", ack)
				}
			})
	})

	// Listener-side ack: the JS client called s.emit("hello", "world", cb).
	// EventPayload.Ack(args...) replies exactly once even if multiple
	// listeners are registered for the same event.
	socketio.On("hello", func(ep *socketio.EventPayload) {
		if !ep.HasAck {
			return
		}
		if err := ep.Ack([]byte(`"hi from server"`)); err != nil {
			log.Printf("ack failed: %v", err)
		}
	})

	socketio.On(socketio.EventDisconnect, func(ep *socketio.EventPayload) {
		log.Printf("disconnected uuid=%s err=%v", ep.SocketUUID, ep.Error)
	})

	app.Get("/ws", socketio.New(func(kws *socketio.Websocket) {
		// New() callback runs after the EIO/SIO handshake completes,
		// before EventConnect listeners fire. Stash per-connection state
		// here; auth validation lives in the EventConnect listener
		// above (which can call kws.Close() to reject).
		kws.SetAttribute("connected_at", time.Now().UTC())
	}))

	// Graceful shutdown: drain Socket.IO connections, then stop Fiber.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := app.Listen(":3000"); err != nil {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := socketio.Shutdown(shutdownCtx); err != nil {
		log.Printf("socketio shutdown: %v", err)
	}
	if err := app.ShutdownWithContext(shutdownCtx); err != nil {
		log.Printf("fiber shutdown: %v", err)
	}
}
