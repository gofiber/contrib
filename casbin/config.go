package casbin

import (
	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/persist"
	fileadapter "github.com/casbin/casbin/v2/persist/file-adapter"
	"github.com/gofiber/fiber/v2"
)

// Config holds the configuration for the middleware
type Config struct {
	// ModelFilePath is path to model file for Casbin.
	// Optional. Default: "./model.conf".
	ModelFilePath string

	// PolicyAdapter is an interface for different persistent providers.
	// Optional. Default: fileadapter.NewAdapter("./policy.csv").
	PolicyAdapter persist.Adapter

	// Enforcer is an enforcer. If you want to use your own enforcer.
	// Optional. Default: nil
	Enforcer *casbin.Enforcer

	// Lookup is a function that is used to look up current subject.
	// An empty string is considered as unauthenticated user.
	// Optional. Default: func(c *fiber.Ctx) string { return "" }
	Lookup func(*fiber.Ctx) string

	// Unauthorized defines the response body for unauthorized responses.
	// Optional. Default: func(c *fiber.Ctx) error { return c.SendStatus(401) }
	Unauthorized fiber.Handler

	// Forbidden defines the response body for forbidden responses.
	// Optional. Default: func(c *fiber.Ctx) error { return c.SendStatus(403) }
	Forbidden fiber.Handler
}

var ConfigDefault = Config{
	ModelFilePath: "./model.conf",
	PolicyAdapter: fileadapter.NewAdapter("./policy.csv"),
	Lookup:        func(c *fiber.Ctx) string { return "" },
	Unauthorized:  func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusUnauthorized) },
	Forbidden:     func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusForbidden) },
}

// Helper function to set default values
func configDefault(config ...Config) (Config, error) {
	// Return default config if nothing provided
	if len(config) < 1 {
		return ConfigDefault, nil
	}

	// Override default config
	cfg := config[0]

	if cfg.Enforcer == nil {
		if cfg.ModelFilePath == "" {
			cfg.ModelFilePath = ConfigDefault.ModelFilePath
		}

		if cfg.PolicyAdapter == nil {
			cfg.PolicyAdapter = ConfigDefault.PolicyAdapter
		}

		enforcer, err := casbin.NewEnforcer(cfg.ModelFilePath, cfg.PolicyAdapter)
		if err != nil {
			return cfg, err
		}

		cfg.Enforcer = enforcer
	}

	if cfg.Lookup == nil {
		cfg.Lookup = ConfigDefault.Lookup
	}

	if cfg.Unauthorized == nil {
		cfg.Unauthorized = ConfigDefault.Unauthorized
	}

	if cfg.Forbidden == nil {
		cfg.Forbidden = ConfigDefault.Forbidden
	}

	return cfg, nil
}
