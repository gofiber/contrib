// 🚀 Fiber is an Express inspired web framework written in Go with 💖
// 📌 API Documentation: https://fiber.wiki
// 📝 Github Repository: https://github.com/gofiber/fiber
// Special thanks to Echo: https://github.com/labstack/echo/blob/master/middleware/jwt.go

package jwtware

import (
	"reflect"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
)

// The contextKey type is unexported to prevent collisions with context keys defined in
// other packages.
type contextKey int

// The following contextKey values are defined to store values in context.
const (
	tokenKey contextKey = iota
)

// New ...
func New(config ...Config) fiber.Handler {
	cfg := makeCfg(config)

	// Return middleware handler
	return func(c fiber.Ctx) error {
		// Filter request to skip middleware
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}
		auth, err := cfg.Extractor.Extract(c)
		if err != nil {
			return cfg.ErrorHandler(c, err)
		}

		if cfg.TokenProcessorFunc != nil {
			auth, err = cfg.TokenProcessorFunc(auth)
			if err != nil {
				return cfg.ErrorHandler(c, err)
			}
		}

		var token *jwt.Token
		if _, ok := cfg.Claims.(jwt.MapClaims); ok {
			token, err = jwt.Parse(auth, cfg.KeyFunc)
		} else {
			t := reflect.ValueOf(cfg.Claims).Type().Elem()
			claims := reflect.New(t).Interface().(jwt.Claims)
			token, err = jwt.ParseWithClaims(auth, claims, cfg.KeyFunc)
		}
		if err == nil && token.Valid {
			// Store user information from token into context.
			c.Locals(tokenKey, token)
			return cfg.SuccessHandler(c)
		}
		return cfg.ErrorHandler(c, err)
	}
}

// FromContext returns the token from the context.
// If there is no token, nil is returned.
func FromContext(c fiber.Ctx) *jwt.Token {
	token, ok := c.Locals(tokenKey).(*jwt.Token)
	if !ok {
		return nil
	}
	return token
}
