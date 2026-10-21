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
)

// doneRecord is the record that ends an AI SDK UI message stream.
var doneRecord = []byte("[DONE]")

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
	// anonymous caller. Deps runs before the body of the request is read, so a
	// caller that is turned away costs next to nothing.
	//
	// The request stays alive until the run has ended, so the strings that c
	// returns remain valid for the whole run. Do not keep c, or those strings,
	// for work that outlives the run.
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
	// the browser. It also applies when the run panics or a chunk cannot be
	// encoded.
	//
	// Optional. Default: err.Error()
	ErrorMessage func(err error) string

	// OnClose is called once when the stream is over, with the dependencies of
	// the run. err is nil when the stream ended normally, which includes a run
	// that a tool canceled. Otherwise it is the agent error when the run
	// failed, the context error when Timeout elapsed, and the write error when
	// the client went away.
	//
	// It runs once the run has ended and the events are written, while the
	// request is still alive. The client has every event by then, but the end of
	// the response and the next request on the connection wait for OnClose, so
	// keep it short, or hand the work to a goroutine and copy what that needs. A
	// panic in OnClose is logged and recovered.
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
	// Neither the cancellation nor the deadline of the context of the request
	// apply to the run, as the handler has returned by the time the run
	// starts. Timeout does.
	//
	// Optional. Default: 0 (no timeout)
	Timeout time.Duration

	// HeartbeatInterval is the interval of the comment lines that keep an idle
	// stream open and let the handler notice a client that went away while the
	// agent is busy. A client that left is noticed by the second or third
	// heartbeat after it did, so a run that nobody waits for is canceled after
	// a few intervals. A value less than or equal to zero uses the default.
	//
	// Optional. Default: 5 * time.Second
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
// answers with the chunks of the run as server-sent events. The body must carry
// the id of the chat.
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
	limits := requestLimits{
		contentTypes:         cfg.Adapter.AllowedContentTypes,
		maxBytes:             cfg.Adapter.MaxRequestBytes,
		skipContentTypeCheck: cfg.Adapter.DisableContentTypeCheck,
	}
	stream := streamOptions[Deps]{
		onClose:          cfg.OnClose,
		timeout:          cfg.Timeout,
		heartbeat:        cfg.HeartbeatInterval,
		disableHeartbeat: cfg.DisableHeartbeat,
	}
	fail := func(send func([]byte) error, err error) {
		failVercel(send, cfg.ErrorMessage, err)
	}

	return func(c fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		request, err := admit[Deps, vercel.RequestData](c, limits, "Vercel AI", cfg.Deps, cfg.RunOptions)
		if err != nil {
			return err
		}
		// The adapter takes the id as the conversation ID of the run, which
		// panics when it is empty.
		if request.input.ID == "" {
			return fiber.NewError(fiber.StatusBadRequest, "invalid Vercel AI request")
		}

		c.Set(vercelStreamHeader, vercelStreamValue)
		return serveStream(c, stream, request.deps, fail, func(ctx context.Context, send func([]byte) error) error {
			for chunk, runErr := range adapter.RunStream(ctx, request.input, request.deps, request.options...) {
				if runErr != nil && cfg.ErrorMessage != nil && chunk.Type == vercel.ChunkError {
					chunk.ErrorText = cfg.ErrorMessage(runErr)
				}
				encoded, err := encodeChunk(chunk)
				if err != nil {
					err = fmt.Errorf("pydanticai: encode Vercel AI chunk: %w", err)
					fail(send, err)
					return err
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

// encodeChunk encodes a chunk as the data of a server-sent event.
func encodeChunk(chunk vercel.Chunk) ([]byte, error) {
	if chunk.Type == vercel.ChunkDone {
		return doneRecord, nil
	}
	return json.Marshal(chunk)
}

// failVercel ends the stream with an error chunk and the [DONE] record for a
// failure that the adapter did not report itself.
func failVercel(send func([]byte) error, message func(error) string, err error) {
	// Best effort: the stream ends either way.
	if encoded, marshalErr := json.Marshal(vercel.Chunk{Type: vercel.ChunkError, ErrorText: errorText(message, err)}); marshalErr == nil {
		_ = send(encoded)
		_ = send(doneRecord)
	}
}
