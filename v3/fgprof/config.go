package fgprof

import "github.com/gofiber/fiber/v3"

type Config struct {
	// Next defines a function to skip this middleware when returned true.
	//
	// Optional. Default: nil
	Next func(c fiber.Ctx) bool

	// Prefix is the path where the fprof endpoints will be mounted.
	// Default Path is "/debug/fgprof"
	//
	// Optional. Default: ""
	Prefix string

	// MaxConcurrent is the number of profiles that may run at the same time.
	// A request beyond it is answered with 503 Service Unavailable.
	//
	// Optional. Default: 4
	MaxConcurrent int

	// MaxSeconds is the longest profile a request may ask for with the seconds
	// parameter (30 when absent). A longer one is answered with 400 Bad Request.
	//
	// Optional. Default: 60
	MaxSeconds int
}

// ConfigDefault is the default config
var ConfigDefault = Config{
	Next:          nil,
	MaxConcurrent: 4,
	MaxSeconds:    60,
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
	if cfg.Prefix == "" {
		cfg.Prefix = ConfigDefault.Prefix
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = ConfigDefault.MaxConcurrent
	}
	if cfg.MaxSeconds <= 0 {
		cfg.MaxSeconds = ConfigDefault.MaxSeconds
	}

	return cfg
}
