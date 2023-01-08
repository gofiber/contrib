package fiberzap

import (
	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Config defines the config for middleware.
type Config struct {
	// Next defines a function to skip this middleware when returned true.
	//
	// Optional. Default: nil
	Next func(c *fiber.Ctx) bool

	// SkipBody defines a function to skip log  "body" field when returned true.
	//
	// Optional. Default: nil
	SkipBody func(c *fiber.Ctx) bool

	// SkipResBody defines a function to skip log  "resBody" field when returned true.
	//
	// Optional. Default: nil
	SkipResBody func(c *fiber.Ctx) bool

	// GetResBody defines a function to get ResBody.
	//  eg: when use compress middleware, resBody is unreadable. you can set GetResBody func to get readable resBody.
	//
	// Optional. Default: nil
	GetResBody func(c *fiber.Ctx) []byte

	// Skip logging for these uri
	//
	// Optional. Default: nil
	SkipURIs []string

	// Add custom zap logger.
	//
	// Optional. Default: zap.NewProduction()\n
	Logger *zap.Logger

	// Add fields what you want see.
	//
	// Optional. Default: {"latency", "status", "method", "url"}
	Fields []string

	// Custom response messages.
	// Response codes >= 500 will be logged with Messages[0].
	// Response codes >= 400 will be logged with Messages[1].
	// Other response codes will be logged with Messages[2].
	// You can specify less, than 3 messages, but you must specify at least 1.
	// Specifying more than 3 messages is useless.
	//
	// Optional. Default: {"Server error", "Client error", "Success"}
	Messages []string

	// Custom response levels.
	// Response codes >= 500 will be logged with Levels[0].
	// Response codes >= 400 will be logged with Levels[1].
	// Other response codes will be logged with Levels[2].
	// You can specify less, than 3 levels, but you must specify at least 1.
	// Specifying more than 3 levels is useless.
	//
	// Optional. Default: {zapcore.ErrorLevel, zapcore.WarnLevel, zapcore.InfoLevel}
	Levels []zapcore.Level
}

// Use zap.NewProduction() as default logging instance.
var logger, _ = zap.NewProduction()

// ConfigDefault is the default config
var ConfigDefault = Config{
	Next:     nil,
	Logger:   logger,
	Fields:   []string{"latency", "status", "method", "url"},
	Messages: []string{"Server error", "Client error", "Success"},
	Levels:   []zapcore.Level{zapcore.ErrorLevel, zapcore.WarnLevel, zapcore.InfoLevel},
}

// Helper function to set default values
func configDefault(config ...Config) Config {
	// Return default config if nothing provided
	if len(config) < 1 {
		return ConfigDefault
	}

	// Override default config
	cfg := config[0]

	// Set default values
	if cfg.Next == nil {
		cfg.Next = ConfigDefault.Next
	}

	if cfg.Logger == nil {
		cfg.Logger = ConfigDefault.Logger
	}

	if cfg.Fields == nil {
		cfg.Fields = ConfigDefault.Fields
	}

	if cfg.Messages == nil {
		cfg.Messages = ConfigDefault.Messages
	}

	if cfg.Levels == nil {
		cfg.Levels = ConfigDefault.Levels
	}

	return cfg
}
