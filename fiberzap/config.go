package fiberzap

import (
	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
)

// Config defines the config for middleware.
type Config struct {
	// Next defines a function to skip this middleware when returned true.
	//
	// Optional. Default: nil
	Next func(c *fiber.Ctx) bool

	// Add custom zap logger.
	//
	// Optional. Default: zap.NewExample()\n
	Logger *zap.Logger

	// Add fields what you want see.
	//
	// Optional. Default: {"pid", "latency"}
	Fields []string

	// Custom response messages.
	//
	// Optional. Default: {"Server error", "Client error", "Success"}
	Messages []string
}

// ConfigDefault is the default config
var ConfigDefault = Config{
	Next:     nil,
	Logger:   zap.NewExample(),
	Fields:   []string{"latency", "status", "method", "url"},
	Messages: []string{"Server error", "Client error", "Success"},
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
		cfg.Logger = zap.NewExample()
	}

	if cfg.Fields == nil {
		cfg.Fields = ConfigDefault.Fields
	}

	if cfg.Messages == nil {
		cfg.Messages = ConfigDefault.Messages
	}

	return cfg
}
