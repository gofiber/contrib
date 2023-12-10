package fgprof

import "github.com/gofiber/fiber/v2"

type Config struct {
	// Next defines a function to skip this middleware when returned true.
	//
	// Optional. Default: nil
	Next func(c *fiber.Ctx) bool

	// Prefix is the path where the fprof endpoints will be mounted.
	// Default Path is "/debug/fgprof"
	//
	// Optional. Default: ""
	Prefix string
}

// ConfigDefault is the default config
var ConfigDefault = Config{
	Next: nil,
}

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

	return cfg
}
