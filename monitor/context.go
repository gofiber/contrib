package monitor

import "github.com/gofiber/fiber/v3"

// Context is an interface that abstracts the Fiber context.
type Context interface {
	Method(override ...string) string
	Get(key string, defaultValue ...string) string
	// TODO: return self is a problem
	Status(status int) fiber.Ctx
	JSON(data any, ctype ...string) error
	Next() error
	Set(key, val string)
	SendString(body string) error
}

// Handler is a function type that takes a Context and returns an error.
type Handler[T Context] func(T) error
