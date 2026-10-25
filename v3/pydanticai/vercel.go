package pydanticai

import (
	"encoding/json"

	ai "github.com/Kludex/pydantic-ai-go/ai"
	"github.com/Kludex/pydantic-ai-go/ai/ui/vercel"
	"github.com/gofiber/fiber/v3"
)

var doneRecord = []byte("[DONE]")

// VercelConfig is the StreamConfig of Vercel, with a vercel.Config as Adapter.
type VercelConfig[Deps any] = StreamConfig[Deps, vercel.Config]

// Vercel returns a handler that serves agent over the Vercel AI SDK UI message
// stream protocol, which the useChat hook consumes. The body must carry the chat
// id. It panics when agent is nil or the adapter config is invalid.
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
		// The adapter takes the id as the conversation ID and panics when it is empty.
		if request.input.ID == "" {
			return fiber.NewError(fiber.StatusBadRequest, "invalid Vercel AI request")
		}

		c.Set("x-vercel-ai-ui-message-stream", "v1")
		return stream(c, &cfg, request, &vercelProtocol, adapter.RunStream)
	}
}

func encodeChunk(chunk *vercel.Chunk) ([]byte, error) {
	if chunk.Type == vercel.ChunkDone {
		return doneRecord, nil
	}
	return json.Marshal(chunk)
}

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
	failure:   vercel.Chunk{Type: vercel.ChunkError},
	// The adapter stops after the error chunk it reports itself, so the handler sends
	// the [DONE] record for it.
	closing: []vercel.Chunk{{Type: vercel.ChunkDone}},
}
