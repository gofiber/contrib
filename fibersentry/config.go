package fibersentry

import "time"

const hubKey = "sentry-hub"

type Config struct {
	// Repanic configures whether Sentry should repanic after recovery.
	// Set to true, if Recover middleware is used.
	// https://github.com/gofiber/fiber/tree/master/middleware/recover
	// Optional. Default: false
	Repanic bool

	// WaitForDelivery configures whether you want to block the request before moving forward with the response.
	// If Recover middleware is used, it's safe to either skip this option or set it to false.
	// https://github.com/gofiber/fiber/tree/master/middleware/recover
	// Optional. Default: false
	WaitForDelivery bool

	// Timeout for the event delivery requests.
	// Optional. Default: 2 Seconds
	Timeout time.Duration
}

// ConfigDefault is the default config
var ConfigDefault = Config{
	Repanic:         false,
	WaitForDelivery: false,
	Timeout:         time.Second * 2,
}

// Helper function to set default values
func configDefault(config ...Config) Config {
	// Return default config if nothing provided
	if len(config) < 1 {
		return ConfigDefault
	}

	// Override default config
	cfg := config[0]

	return cfg
}
