package jwtware

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// Source represents the type of source from which a token is extracted.
// This is informational metadata that helps developers understand the extractor behavior.
type Source int

const (
	// SourceHeader indicates the token is extracted from an HTTP header.
	SourceHeader Source = iota

	// SourceAuthHeader indicates the token is extracted from the Authorization header.
	SourceAuthHeader

	// SourceForm indicates the token is extracted from form data.
	SourceForm

	// SourceQuery indicates the token is extracted from URL query parameters.
	SourceQuery

	// SourceParam indicates the token is extracted from URL path parameters.
	SourceParam

	// SourceCookie indicates the token is extracted from cookies.
	SourceCookie

	// SourceCustom indicates the token is extracted using a custom extractor function.
	SourceCustom
)

// ErrJWTMissingOrMalformed is returned when the token is missing or malformed.
var ErrJWTMissingOrMalformed = errors.New("missing or malformed JWT")

// Extractor defines a token extraction method with metadata.
type Extractor struct {
	Extract    func(fiber.Ctx) (string, error)
	Key        string      // The parameter/header name used for extraction
	AuthScheme string      // The auth scheme, e.g., "Bearer" for AuthHeader
	Chain      []Extractor // For chaining multiple extractors
	Source     Source      // The type of source being extracted from
}

// FromAuthHeader extracts a token from the specified header and authentication scheme.
// It's commonly used for the "Authorization" header with a "Bearer" scheme.
func FromAuthHeader(header, authScheme string) Extractor {
	return Extractor{
		Extract: func(c fiber.Ctx) (string, error) {
			authHeader := c.Get(header)
			if authHeader == "" {
				return "", ErrJWTMissingOrMalformed
			}

			// Check if the header starts with the specified auth scheme
			if authScheme != "" {
				schemeLen := len(authScheme)
				if len(authHeader) > schemeLen+1 && strings.EqualFold(authHeader[:schemeLen], authScheme) && authHeader[schemeLen] == ' ' {
					return strings.TrimSpace(authHeader[schemeLen+1:]), nil
				}
				return "", ErrJWTMissingOrMalformed
			}

			return strings.TrimSpace(authHeader), nil
		},
		Key:        header,
		Source:     SourceAuthHeader,
		AuthScheme: authScheme,
	}
}

// FromCookie creates an Extractor that retrieves a token from a specified cookie in the request.
//
// Parameters:
//   - key: The name of the cookie from which to extract the token.
//
// Returns:
//
//	An Extractor that attempts to retrieve the token from the specified cookie. If the cookie
//	is not present or does not contain a token, it returns ErrJWTMissingOrMalformed.
func FromCookie(key string) Extractor {
	return Extractor{
		Extract: func(c fiber.Ctx) (string, error) {
			token := c.Cookies(key)
			if token == "" {
				return "", ErrJWTMissingOrMalformed
			}
			return token, nil
		},
		Key:    key,
		Source: SourceCookie,
	}
}

// FromParam creates an Extractor that retrieves a token from a specified URL parameter in the request.
//
// Parameters:
//   - param: The name of the URL parameter from which to extract the token.
//
// Returns:
//
//	An Extractor that attempts to retrieve the token from the specified URL parameter. If the
//	parameter is not present or does not contain a token, it returns ErrJWTMissingOrMalformed.
func FromParam(param string) Extractor {
	return Extractor{
		Extract: func(c fiber.Ctx) (string, error) {
			token := c.Params(param)
			if token == "" {
				return "", ErrJWTMissingOrMalformed
			}
			return token, nil
		},
		Key:    param,
		Source: SourceParam,
	}
}

// FromForm creates an Extractor that retrieves a token from a specified form field in the request.
//
// Parameters:
//   - param: The name of the form field from which to extract the token.
//
// Returns:
//
//	An Extractor that attempts to retrieve the token from the specified form field. If the
//	field is not present or does not contain a token, it returns ErrJWTMissingOrMalformed.
func FromForm(param string) Extractor {
	return Extractor{
		Extract: func(c fiber.Ctx) (string, error) {
			token := c.FormValue(param)
			if token == "" {
				return "", ErrJWTMissingOrMalformed
			}
			return token, nil
		},
		Key:    param,
		Source: SourceForm,
	}
}

// FromHeader creates an Extractor that retrieves a token from a specified HTTP header in the request.
//
// Parameters:
//   - param: The name of the HTTP header from which to extract the token.
//
// Returns:
//
//	An Extractor that attempts to retrieve the token from the specified HTTP header. If the
//	header is not present or does not contain a token, it returns ErrJWTMissingOrMalformed.
func FromHeader(param string) Extractor {
	return Extractor{
		Extract: func(c fiber.Ctx) (string, error) {
			token := c.Get(param)
			if token == "" {
				return "", ErrJWTMissingOrMalformed
			}
			return token, nil
		},
		Key:    param,
		Source: SourceHeader,
	}
}

// FromQuery creates an Extractor that retrieves a token from a specified query parameter in the request.
//
// Parameters:
//   - param: The name of the query parameter from which to extract the token.
//
// Returns:
//
//	An Extractor that attempts to retrieve the token from the specified query parameter. If the
//	parameter is not present or does not contain a token, it returns ErrJWTMissingOrMalformed.
func FromQuery(param string) Extractor {
	return Extractor{
		Extract: func(c fiber.Ctx) (string, error) {
			token := fiber.Query[string](c, param)
			if token == "" {
				return "", ErrJWTMissingOrMalformed
			}
			return token, nil
		},
		Key:    param,
		Source: SourceQuery,
	}
}

// Chain creates an Extractor that tries multiple extractors in order until one succeeds.
//
// Parameters:
//   - extractors: A variadic list of Extractor instances to try in sequence.
//
// Returns:
//
//	An Extractor that attempts each provided extractor in order and returns the first successful
//	extraction. If all extractors fail, it returns the last error encountered, or ErrJWTMissingOrMalformed
//	if no errors were returned. If no extractors are provided, it always fails with ErrJWTMissingOrMalformed.
func Chain(extractors ...Extractor) Extractor {
	if len(extractors) == 0 {
		return Extractor{
			Extract: func(fiber.Ctx) (string, error) {
				return "", ErrJWTMissingOrMalformed
			},
			Source: SourceCustom,
			Key:    "",
			Chain:  []Extractor{},
		}
	}

	// Use the source and key from the first extractor as the primary
	primarySource := extractors[0].Source
	primaryKey := extractors[0].Key

	return Extractor{
		Extract: func(c fiber.Ctx) (string, error) {
			var lastErr error

			for _, extractor := range extractors {
				token, err := extractor.Extract(c)

				if err == nil && token != "" {
					return token, nil
				}

				// Only update lastErr if we got an actual error
				if err != nil {
					lastErr = err
				}
			}
			if lastErr != nil {
				return "", lastErr
			}
			return "", ErrJWTMissingOrMalformed
		},
		Source: primarySource,
		Key:    primaryKey,
		Chain:  extractors,
	}
}
