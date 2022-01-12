package fibersentry

import "time"

const hubKey = "sentry"

type Config struct {
	// Repanic configures whether Sentry should repanic after recovery, in most cases it should be set to false,
	// as fasthttp doesn't include it's own Recovery handler.
	// Optional. Default: false
	Repanic bool

	// WaitForDelivery configures whether you want to block the request before moving forward with the response.
	// Because fasthttp doesn't include it's own Recovery handler, it will restart the application,
	// and event won't be delivered otherwise.
	// Optional. Default: false
	WaitForDelivery bool

	// Timeout for the event delivery requests.
	// Optional. Default: 2 Seconds
	Timeout time.Duration
}
