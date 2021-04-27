package pasetoware

import (
	"errors"
	"github.com/gofiber/fiber/v2"
	"github.com/o1egl/paseto"
	"time"
)

const (
	LookupHeader = "header"
	LookupCookie = "cookie"
	LookupQuery  = "query"
	LookupParam  = "param"

	// DefaultContextKey is the Default key used by this middleware to store decrypted token
	DefaultContextKey = "auth-token"
)

var (
	ErrExpiredToken  = errors.New("token has expired")
	ErrDataUnmarshal = errors.New("can't unmarshal token data to Payload type")
	pasetoObject     = paseto.NewV2()
)

// Acquire Token methods

type acquireToken func(c *fiber.Ctx, key string) string

func acquireFromHeader(c *fiber.Ctx, key string) string {
	return c.Get(key)
}

func acquireFromQuery(c *fiber.Ctx, key string) string {
	return c.Query(key)
}

func acquireFromParams(c *fiber.Ctx, key string) string {
	return c.Params(key)
}

func acquireFromCookie(c *fiber.Ctx, key string) string {
	return c.Cookies(key)
}

// Public helper functions

// CreateToken Create a new Token Payload that will be stored in PASETO
func CreateToken(key []byte, dataInfo string, duration time.Duration) (string, error) {
	payload, err := NewPayload(dataInfo, duration)
	if err != nil {
		return "", err
	}
	return pasetoObject.Encrypt(key, payload, nil)
}
