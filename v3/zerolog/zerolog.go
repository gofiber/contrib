package zerolog

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

// New creates a new middleware handler
func New(config ...Config) fiber.Handler {
	// Set default config
	cfg := configDefault(config...)

	// Return new handler
	return func(c fiber.Ctx) error {
		// Don't execute middleware if Next returns true
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		start := time.Now()
		// What hooks get if timeout.New answered: its handler may still be setting the context then.
		reqCtx := c.Context()

		// Handle request, store err for logging
		chainErr := c.Next()
		if chainErr != nil {
			// Manually call error handler
			if err := c.App().ErrorHandler(c, chainErr); err != nil {
				_ = c.SendStatus(fiber.StatusInternalServerError)
			}
		}

		response := sentResponse(c)
		// As in Fiber's logger: the timeout error keeps middleware before us off the handler's response.
		var result error
		if response != c.Response() {
			result = chainErr
		}

		// New: Skip check AFTER c.Next() but BEFORE log assembly
		if cfg.Skip != nil && cfg.Skip(c) {
			return result
		}

		latency := time.Since(start)

		status := response.StatusCode()

		index := 0
		switch {
		case status >= 500:
			// error index is zero
		case status >= 400:
			index = 1
		default:
			index = 2
		}

		levelIndex := index
		if levelIndex >= len(cfg.Levels) {
			levelIndex = len(cfg.Levels) - 1
		}
		level := cfg.Levels[levelIndex]

		// no log
		if level == zerolog.NoLevel || level == zerolog.Disabled {
			return result
		}

		messageIndex := index
		if messageIndex >= len(cfg.Messages) {
			messageIndex = len(cfg.Messages) - 1
		}
		message := cfg.Messages[messageIndex]

		logger := cfg.logger(c, response, latency, chainErr)
		ctx := reqCtx
		if response == c.Response() {
			ctx = logContext{Context: c.Context(), c: c}
		}

		switch level {
		case zerolog.DebugLevel:
			logger.Debug().Ctx(ctx).Msg(message)
		case zerolog.InfoLevel:
			logger.Info().Ctx(ctx).Msg(message)
		case zerolog.WarnLevel:
			logger.Warn().Ctx(ctx).Msg(message)
		case zerolog.ErrorLevel:
			logger.Error().Ctx(ctx).Msg(message)
		case zerolog.FatalLevel:
			logger.Fatal().Ctx(ctx).Msg(message)
		case zerolog.PanicLevel:
			logger.Panic().Ctx(ctx).Msg(message)
		case zerolog.TraceLevel:
			logger.Trace().Ctx(ctx).Msg(message)
		}

		return result
	}
}

// logContext is c.Context() for zerolog hooks, falling back to c.Value so
// values kept only in c.Locals (e.g. the request ID) stay visible.
type logContext struct {
	context.Context
	c fiber.Ctx
}

func (l logContext) Value(key any) any {
	if v := l.Context.Value(key); v != nil {
		return v
	}
	return l.c.Value(key)
}

// sentResponse is what the client gets: timeout.New parks its own while the handler keeps c's.
func sentResponse(c fiber.Ctx) *fasthttp.Response {
	// IsAbandoned goes first: its atomic load orders the read after the timeout middleware's write.
	if c.IsAbandoned() {
		if resp := c.RequestCtx().LastTimeoutErrorResponse(); resp != nil {
			return resp
		}
	}

	return c.Response()
}
