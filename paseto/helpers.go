package pasetoware

import (
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/o1egl/paseto"
)

const (
	LookupHeader = "header"
	LookupCookie = "cookie"
	LookupQuery  = "query"
	LookupParam  = "param"

	// DefaultContextKey is the Default key used by this middleware to store decrypted token
	DefaultContextKey = "auth-token"
)

type TokenPurpose int

const (
	PurposeLocal TokenPurpose = iota
	PurposePublic
)

var (
	ErrExpiredToken         = errors.New("token has expired")
	ErrMissingToken         = errors.New("missing PASETO token")
	ErrIncorrectTokenPrefix = errors.New("missing prefix for PASETO token")
	ErrDataUnmarshal        = errors.New("can't unmarshal token data to Payload type")
	pasetoObject            = paseto.NewV2()
)

type acquireToken func(c fiber.Ctx, key string) string

// PayloadValidator Function that receives the decrypted payload and returns an interface and an error
// that's a result of validation logic
type PayloadValidator func(decrypted []byte) (interface{}, error)

// PayloadCreator Signature of a function that generates a payload token
type PayloadCreator func(key []byte, dataInfo string, duration time.Duration, purpose TokenPurpose) (string, error)

// Acquire Token methods
func acquireFromHeader(c fiber.Ctx, key string) string {
	return c.Get(key)
}

func acquireFromQuery(c fiber.Ctx, key string) string {
	return c.Query(key)
}

func acquireFromParams(c fiber.Ctx, key string) string {
	return c.Params(key)
}

func acquireFromCookie(c fiber.Ctx, key string) string {
	return c.Cookies(key)
}

func getExtractor(lookupOrigin string) (extractor acquireToken) {
	switch lookupOrigin {
	case LookupHeader:
		extractor = acquireFromHeader
	case LookupQuery:
		extractor = acquireFromQuery
	case LookupParam:
		extractor = acquireFromParams
	case LookupCookie:
		extractor = acquireFromCookie
	default:
		extractor = acquireFromHeader
	}

	return
}

// Public helper functions

// CreateToken Create a new Token Payload that will be stored in PASETO
func CreateToken(key []byte, dataInfo string, duration time.Duration, purpose TokenPurpose) (string, error) {
	payload, err := NewPayload(dataInfo, duration)
	if err != nil {
		return "", err
	}

	switch purpose {
	case PurposeLocal:
		return pasetoObject.Encrypt(key, payload, nil)
	case PurposePublic:
		return pasetoObject.Sign(ed25519.PrivateKey(key), payload, nil)
	default:
		return pasetoObject.Encrypt(key, payload, nil)
	}
}
