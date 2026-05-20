package casbin

import (
	casbinv3 "github.com/casbin/casbin/v3"
	casbinv3persist "github.com/casbin/casbin/v3/persist"
	casbinv3fileadapter "github.com/casbin/casbin/v3/persist/file-adapter"
	"github.com/gofiber/fiber/v3"
)

// ConfigV3 holds the configuration for the middleware when using Casbin v3.
type ConfigV3 struct {
	// ModelFilePath is path to model file for Casbin.
	// Optional. Default: "./model.conf".
	ModelFilePath string

	// PolicyAdapter is an interface for different persistent providers (v3).
	// Optional. Default: fileadapter.NewAdapter("./policy.csv").
	PolicyAdapter casbinv3persist.Adapter

	// Enforcer is a Casbin v3 enforcer. If you want to use your own enforcer.
	// Optional. Default: nil (one is created from ModelFilePath and PolicyAdapter).
	Enforcer *casbinv3.Enforcer

	// Lookup is a function that is used to look up current subject.
	// An empty string is considered as unauthenticated user.
	// Optional. Default: func(c fiber.Ctx) string { return "" }
	Lookup func(fiber.Ctx) string

	// Unauthorized defines the response body for unauthorized responses.
	// Optional. Default: func(c fiber.Ctx) error { return c.SendStatus(401) }
	Unauthorized fiber.Handler

	// Forbidden defines the response body for forbidden responses.
	// Optional. Default: func(c fiber.Ctx) error { return c.SendStatus(403) }
	Forbidden fiber.Handler
}

var ConfigV3Default = ConfigV3{
	ModelFilePath: "./model.conf",
	PolicyAdapter: casbinv3fileadapter.NewAdapter("./policy.csv"),
	Lookup:        func(c fiber.Ctx) string { return "" },
	Unauthorized:  func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusUnauthorized) },
	Forbidden:     func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusForbidden) },
}

// Helper function to set default values
func configDefaultV3(config ...ConfigV3) (ConfigV3, error) {
	// Return default config if nothing provided
	if len(config) < 1 {
		return ConfigV3Default, nil
	}

	// Override default config
	cfg := config[0]

	if cfg.Enforcer == nil {
		if cfg.ModelFilePath == "" {
			cfg.ModelFilePath = ConfigV3Default.ModelFilePath
		}

		if cfg.PolicyAdapter == nil {
			cfg.PolicyAdapter = ConfigV3Default.PolicyAdapter
		}

		enforcer, err := casbinv3.NewEnforcer(cfg.ModelFilePath, cfg.PolicyAdapter)
		if err != nil {
			return cfg, err
		}

		cfg.Enforcer = enforcer
	}

	if cfg.Lookup == nil {
		cfg.Lookup = ConfigV3Default.Lookup
	}

	if cfg.Unauthorized == nil {
		cfg.Unauthorized = ConfigV3Default.Unauthorized
	}

	if cfg.Forbidden == nil {
		cfg.Forbidden = ConfigV3Default.Forbidden
	}

	return cfg, nil
}
