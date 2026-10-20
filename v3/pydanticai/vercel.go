package pydanticai

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/ui/vercel"
	"github.com/gofiber/fiber/v3"
)

const (
	// vercelStreamHeader marks the response as an AI SDK UI message stream.
	vercelStreamHeader = "x-vercel-ai-ui-message-stream"
	vercelStreamValue  = "v1"
	// vercelDone is the record that ends an AI SDK UI message stream.
	vercelDone = "[DONE]"
)

// VercelConfig defines the config for the Vercel handler.
type VercelConfig[Deps any] struct {
	// Next defines a function to skip this handler when it returns true.
	//
	// Optional. Default: nil
	Next func(c fiber.Ctx) bool

	// Deps resolves the dependencies passed to the agent for one request, for
	// example from the user that an earlier middleware stored in c.Locals. An
	// error aborts the request before the stream starts and goes through the
	// Fiber error handler, so return fiber.ErrUnauthorized to turn away an
	// anonymous caller.
	//
	// The run outlives the handler, so do not keep c in the dependencies. The
	// strings that c returns, such as the ones of c.Get and c.Query, can point
	// into the buffer of the request, which fasthttp reuses once the client has
	// gone away. Copy them with strings.Clone, or enable Immutable in the
	// config of the app.
	//
	// Optional. Default: the zero value of Deps
	Deps func(c fiber.Ctx) (Deps, error)

	// RunOptions returns additional agent run options for one request, such as
	// ai.WithRunUsageLimits or ai.WithRunMetadata. The adapter sets the message
	// history and the conversation ID itself, whatever the options say.
	//
	// Optional. Default: nil
	RunOptions func(c fiber.Ctx) ([]ai.RunOption, error)

	// ErrorMessage maps the error that failed a run to the message that the
	// client receives in the error chunk. Use it to keep provider error
	// details, which can carry request IDs or parts of credentials, away from
	// the browser.
	//
	// Optional. Default: err.Error()
	ErrorMessage func(err error) string

	// OnClose is called once when the stream has ended, with the dependencies of
	// the run. err is nil when the run completed, the agent error when the run
	// failed, and the write error when the client went away.
	//
	// It does not receive the fiber.Ctx. OnClose runs when the response ends,
	// and fasthttp reuses the request of a client that went away while the run
	// is still winding down, so the context may belong to another request by
	// then. Read what the log line needs in Deps and keep it in the dependencies.
	//
	// Optional. Default: nil
	OnClose func(deps Deps, err error)

	// Adapter configures the Vercel AI adapter: the AI SDK version, how
	// untrusted client history is sanitized and the request limits
	// (MaxRequestBytes, AllowedContentTypes and DisableContentTypeCheck).
	//
	// Optional. Default: vercel.Config{}
	Adapter vercel.Config

	// Timeout bounds the duration of one run. When it elapses the run is
	// canceled and the stream ends, as it does when the client goes away.
	//
	// Optional. Default: 0 (no timeout)
	Timeout time.Duration

	// HeartbeatInterval is the interval of the comment lines that keep an idle
	// stream open and let the handler notice a client that went away while the
	// agent is busy. A value less than or equal to zero uses the default.
	//
	// Optional. Default: 15 * time.Second
	HeartbeatInterval time.Duration

	// DisableHeartbeat turns the heartbeat off. A client that goes away while
	// the agent is busy then goes unnoticed until a write to it fails, which
	// can be after the run has ended.
	//
	// Optional. Default: false
	DisableHeartbeat bool
}

// Vercel returns a handler that serves agent over the Vercel AI SDK UI message
// stream protocol, which is what the useChat hook of the AI SDK consumes.
//
// The handler accepts a POST request with the JSON body that useChat sends and
// answers with the chunks of the run as server-sent events.
//
//	app.Post("/chat", pydanticai.Vercel(agent))
//
// It panics when agent is nil or the adapter config is invalid.
func Vercel[Deps, Output any](agent *ai.Agent[Deps, Output], config ...VercelConfig[Deps]) fiber.Handler {
	var cfg VercelConfig[Deps]
	if len(config) > 0 {
		cfg = config[0]
	}
	adapter := vercel.NewAdapter(agent, cfg.Adapter)
	limits := newRequestLimits(
		cfg.Adapter.MaxRequestBytes, cfg.Adapter.AllowedContentTypes, cfg.Adapter.DisableContentTypeCheck,
	)
	stream := streamOptions{
		timeout:          cfg.Timeout,
		heartbeat:        cfg.HeartbeatInterval,
		disableHeartbeat: cfg.DisableHeartbeat,
	}

	return func(c fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		body, err := limits.body(c)
		if err != nil {
			return err
		}
		// Resolving the dependencies comes first, so that a caller that is
		// turned away does not get its payload parsed.
		deps, options, err := resolve(c, cfg.Deps, cfg.RunOptions)
		if err != nil {
			return err
		}
		var input vercel.RequestData
		if err := decodeJSON(body, &input, "Vercel AI"); err != nil {
			return err
		}

		c.Set(vercelStreamHeader, vercelStreamValue)
		return serveStream(c, stream, onClose(cfg.OnClose, deps), func(ctx context.Context, send func([]byte) error) error {
			for chunk, runErr := range adapter.RunStream(ctx, input, deps, options...) {
				if runErr != nil && cfg.ErrorMessage != nil && chunk.Type == vercel.ChunkError {
					chunk.ErrorText = cfg.ErrorMessage(runErr)
				}
				encoded := []byte(vercelDone)
				if chunk.Type != vercel.ChunkDone {
					var err error
					if encoded, err = json.Marshal(chunk); err != nil {
						return failVercel(send, cfg.ErrorMessage, fmt.Errorf("pydanticai: encode Vercel AI chunk: %w", err))
					}
				}
				if err := send(encoded); err != nil {
					return err
				}
				if runErr != nil {
					return runErr
				}
			}
			// A canceled run ends the stream without an error, so report why it
			// was canceled.
			return ctx.Err()
		})
	}
}

// failVercel ends the stream with an error chunk and the [DONE] record for a
// chunk that could not be encoded. It returns err for OnClose.
func failVercel(send func([]byte) error, message func(error) string, err error) error {
	text := err.Error()
	if message != nil {
		text = message(err)
	}
	// Best effort: the stream ends either way.
	if encoded, marshalErr := json.Marshal(vercel.Chunk{Type: vercel.ChunkError, ErrorText: text}); marshalErr == nil {
		_ = send(encoded)
		_ = send([]byte(vercelDone))
	}
	return err
}
