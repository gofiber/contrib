package pydanticai

import (
	"encoding/json"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/ui/vercel"
	"github.com/gofiber/fiber/v3"
)

// doneRecord is the record that ends an AI SDK UI message stream.
var doneRecord = []byte("[DONE]")

// VercelConfig defines the config for the Vercel handler. It is a StreamConfig
// whose Adapter is a vercel.Config.
type VercelConfig[Deps any] = StreamConfig[Deps, vercel.Config]

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
	limits := newRequestLimits(cfg.Adapter.AllowedContentTypes, cfg.Adapter.MaxRequestBytes, cfg.Adapter.DisableContentTypeCheck)

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

		c.Set("x-vercel-ai-ui-message-stream", "v1") // marks the response as an AI SDK UI message stream
		return stream(c, &cfg, request, vercelProtocol, adapter.RunStream)
	}
}

// encodeChunk encodes a chunk as the data of a server-sent event.
func encodeChunk(chunk *vercel.Chunk) ([]byte, error) {
	if chunk.Type == vercel.ChunkDone {
		return doneRecord, nil
	}
	return json.Marshal(chunk)
}

// vercelProtocol describes the Vercel AI chunks for relay.
var vercelProtocol = protocol[vercel.Chunk]{
	what:   "Vercel AI chunk",
	encode: encodeChunk,
	failed: func(chunk vercel.Chunk) bool { return chunk.Type == vercel.ChunkError },
	withMessage: func(chunk vercel.Chunk, message string) vercel.Chunk {
		chunk.ErrorText = message
		return chunk
	},
	// A canceled run ends with an abort chunk instead. Both are followed by [DONE].
	completed: func(chunk vercel.Chunk) bool { return chunk.Type == vercel.ChunkFinish },
	// The error chunk is followed by the record that ends the stream. The
	// adapter stops after the error chunk that it reports itself, so the handler
	// sends the record for it.
	fail: func(send func([]byte) error, message string) {
		if encoded, err := json.Marshal(vercel.Chunk{Type: vercel.ChunkError, ErrorText: message}); err == nil {
			_ = send(encoded)
			_ = send(doneRecord)
		}
	},
	end: func(send func([]byte) error) { _ = send(doneRecord) },
}
