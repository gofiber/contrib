package fibersentry

import "time"

const hubKey = "sentry"

type Config struct {
	// Repanic configures whether Sentry should repanic after recovery.
	// It should be set to true, if Recover middleware is used.
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
