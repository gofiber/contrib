package pasetoware

import (
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

// Extractor defines a token extraction method with metadata.
type Extractor struct {
	Extract    func(fiber.Ctx) (string, error)
	Key        string      // The parameter/header name used for extraction
	Source     Source      // The type of source being extracted from
	AuthScheme string      // The authentication scheme (e.g., "Bearer")
	Chain      []Extractor // For chained extractors, stores all extractors in the chain
}

// FromHeader creates an Extractor that retrieves a token from a specified HTTP header in the request.
//
// Parameters:
//   - header: The name of the HTTP header from which to extract the token.
//
// Returns:
//
//	An Extractor that attempts to retrieve the token from the specified HTTP header. If the
//	header is not present or does not contain a token, it returns ErrMissingToken.
func FromHeader(header string) Extractor {
	return Extractor{
		Extract: func(c fiber.Ctx) (string, error) {
			token := strings.TrimSpace(c.Get(header))
			if token == "" {
				return "", ErrMissingToken
			}
			return token, nil
		},
		Key:    header,
		Source: SourceHeader,
	}
}

// FromQuery creates an Extractor that retrieves a token from a specified query parameter in the request.
//
// SECURITY WARNING: Extracting tokens from URL query parameters can leak sensitive information through:
// - Server logs and access logs
// - Browser referrer headers
// - Proxy and intermediary logs
// - Browser history and bookmarks
// - Network monitoring tools
// Consider using FromAuthHeader or FromCookie for better security.
//
// Parameters:
//   - param: The name of the query parameter from which to extract the token.
//
// Returns:
//
//	An Extractor that attempts to retrieve the token from the specified query parameter. If the
//	parameter is not present or does not contain a token, it returns ErrMissingToken.
func FromQuery(param string) Extractor {
	return Extractor{
		Extract: func(c fiber.Ctx) (string, error) {
			token := strings.TrimSpace(c.Query(param))
			if token == "" {
				return "", ErrMissingToken
			}
			return token, nil
		},
		Key:    param,
		Source: SourceQuery,
	}
}

// FromParam creates an Extractor that retrieves a token from a specified URL parameter in the request.
//
// SECURITY WARNING: Extracting tokens from URL parameters can leak sensitive information through:
// - Server logs and access logs
// - Browser referrer headers
// - Proxy and intermediary logs
// - Browser history
// Consider using FromAuthHeader or FromCookie for better security.
//
// Parameters:
//   - param: The name of the URL parameter from which to extract the token.
//
// Returns:
//
//	An Extractor that attempts to retrieve the token from the specified URL parameter. If the
//	parameter is not present or does not contain a token, it returns ErrMissingToken.
func FromParam(param string) Extractor {
	return Extractor{
		Extract: func(c fiber.Ctx) (string, error) {
			token := strings.TrimSpace(c.Params(param))
			if token == "" {
				return "", ErrMissingToken
			}
			return token, nil
		},
		Key:    param,
		Source: SourceParam,
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
//	is not present or does not contain a token, it returns ErrMissingToken.
func FromCookie(key string) Extractor {
	return Extractor{
		Extract: func(c fiber.Ctx) (string, error) {
			token := strings.TrimSpace(c.Cookies(key))
			if token == "" {
				return "", ErrMissingToken
			}
			return token, nil
		},
		Key:    key,
		Source: SourceCookie,
	}
}

// FromForm creates an Extractor that retrieves a token from a specified form field in the request.
//
// SECURITY WARNING: Extracting tokens from form data can leak sensitive information through:
// - Server logs and access logs
// - Browser referrer headers (if form is submitted via GET)
// - Proxy and intermediary logs
// Consider using FromAuthHeader or FromCookie for better security.
//
// Parameters:
//   - param: The name of the form field from which to extract the token.
//
// Returns:
//
//	An Extractor that attempts to retrieve the token from the specified form field. If the
//	field is not present or does not contain a token, it returns ErrMissingToken.
func FromForm(param string) Extractor {
	return Extractor{
		Extract: func(c fiber.Ctx) (string, error) {
			token := strings.TrimSpace(c.FormValue(param))
			if token == "" {
				return "", ErrMissingToken
			}
			return token, nil
		},
		Key:    param,
		Source: SourceForm,
	}
}

// FromAuthHeader extracts a token from the Authorization header with an optional prefix.
// This is a convenience function for the common case of extracting from the Authorization header.
//
// Parameters:
//   - authScheme: The auth scheme to strip from the header value (e.g., "Bearer"). If empty, no prefix is stripped.
//
// Returns:
//
//	An Extractor that attempts to retrieve the token from the Authorization header.
func FromAuthHeader(authScheme string) Extractor {
	return Extractor{
		Extract: func(c fiber.Ctx) (string, error) {
			authHeader := c.Get(fiber.HeaderAuthorization)
			if authHeader == "" {
				return "", ErrMissingToken
			}

			if authScheme != "" {
				parts := strings.Fields(authHeader)
				if len(parts) >= 2 && strings.EqualFold(parts[0], authScheme) {
					return parts[1], nil
				}
				return "", ErrMissingToken
			}

			return strings.TrimSpace(authHeader), nil
		},
		Key:        fiber.HeaderAuthorization,
		Source:     SourceAuthHeader,
		AuthScheme: authScheme,
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
//	extraction. If all extractors fail, it returns the last error encountered, or ErrMissingToken
//	if no errors were returned. If no extractors are provided, it always fails with ErrMissingToken.
func Chain(extractors ...Extractor) Extractor {
	if len(extractors) == 0 {
		return Extractor{
			Extract: func(fiber.Ctx) (string, error) {
				return "", ErrMissingToken
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
			return "", ErrMissingToken
		},
		Source: primarySource,
		Key:    primaryKey,
		Chain:  append([]Extractor(nil), extractors...), // Defensive copy for introspection
	}
}
