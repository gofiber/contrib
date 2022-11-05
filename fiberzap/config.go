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
	//
	// Optional. Default: {"Server error", "Client error", "Success"}
	Messages []string

	// Custom response levels.
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
