package jwtware

import (
	"errors"
	"fmt"
	"log"
	"net/url"
	"time"

	"github.com/MicahParks/keyfunc/v2"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/extractors"
	"github.com/golang-jwt/jwt/v5"
)

var (
	// ErrJWTAlg is returned when the JWT header did not contain the expected algorithm.
	ErrJWTAlg = errors.New("the JWT header did not contain the expected algorithm")

	// ErrMissingToken is returned when no JWT token is found in the request.
	ErrMissingToken = errors.New("missing or malformed JWT")
)

// Config defines the config for JWT middleware
type Config struct {
	// Next defines a function to skip this middleware when returned true.
	//
	// Optional. Default: nil
	Next func(fiber.Ctx) bool

	// SuccessHandler is executed when a token is successfully validated.
	// Optional. Default: nil
	SuccessHandler fiber.Handler

	// ErrorHandler is executed when token validation fails.
	// It allows customization of JWT error responses.
	// Optional. Default: 401 Invalid or expired JWT
	ErrorHandler fiber.ErrorHandler

	// SigningKey is the primary key used to validate tokens.
	// Used as a fallback if SigningKeys is empty.
	// At least one of the following is required: KeyFunc, JWKSetURLs, SigningKeys, or SigningKey.
	SigningKey SigningKey

	// SigningKeys is a map of keys used to validate tokens with the "kid" field.
	// At least one of the following is required: KeyFunc, JWKSetURLs, SigningKeys, or SigningKey.
	SigningKeys map[string]SigningKey

	// Claims are extendable claims data defining token content.
	// Optional. Default value jwt.MapClaims
	Claims jwt.Claims

	// Extractor defines a function to extract the token from the request.
	// Optional. Default: FromAuthHeader("Bearer").
	Extractor extractors.Extractor

	// TokenProcessorFunc processes the token extracted using the Extractor.
	// Optional. Default: nil
	TokenProcessorFunc func(token string) (string, error)

	// KeyFunc provides the public key for JWT verification.
	// It handles algorithm verification and key selection.
	// By default, the github.com/MicahParks/keyfunc/v2 package is used.
	// At least one of the following is required: KeyFunc, JWKSetURLs, SigningKeys, or SigningKey.
	KeyFunc jwt.Keyfunc

	// JWKSetURLs is a list of URLs containing JSON Web Key Sets (JWKS) for signature verification.
	// HTTPS is recommended. The "kid" field in the JWT header and JWKs is mandatory.
	// Default behavior:
	// - Refresh every hour.
	// - Auto-refresh on new "kid" in JWT.
	// - Rate limit refreshes to once every 5 minutes.
	// - Timeout refreshes after 10 seconds.
	// At least one of the following is required: KeyFunc, JWKSetURLs, SigningKeys, or SigningKey.
	JWKSetURLs []string
}

// SigningKey holds information about the recognized cryptographic keys used to sign JWTs by this program.
type SigningKey struct {
	// JWTAlg is the algorithm used to sign JWTs. If this value is a non-empty string, this will be checked against the
	// "alg" value in the JWT header.
	//
	// https://www.rfc-editor.org/rfc/rfc7518#section-3.1
	JWTAlg string
	// Key is the cryptographic key used to sign JWTs. For supported types, please see
	// https://github.com/golang-jwt/jwt.
	Key interface{}
}

// makeCfg function will check correctness of supplied configuration
// and will complement it with default values instead of missing ones
func makeCfg(config []Config) (cfg Config) {
	if len(config) > 0 {
		cfg = config[0]
	}
	if cfg.SuccessHandler == nil {
		cfg.SuccessHandler = func(c fiber.Ctx) error {
			return c.Next()
		}
	}
	if cfg.ErrorHandler == nil {
		cfg.ErrorHandler = func(c fiber.Ctx, err error) error {
			if errors.Is(err, extractors.ErrNotFound) {
				return c.Status(fiber.StatusBadRequest).SendString(ErrMissingToken.Error())
			}
			if e, ok := err.(*fiber.Error); ok {
				return c.Status(e.Code).SendString(e.Message)
			}
			return c.Status(fiber.StatusUnauthorized).SendString("Invalid or expired JWT")
		}
	}
	if cfg.SigningKey.Key == nil && len(cfg.SigningKeys) == 0 && len(cfg.JWKSetURLs) == 0 && cfg.KeyFunc == nil {
		panic("Fiber: JWT middleware configuration: At least one of the following is required: KeyFunc, JWKSetURLs, SigningKeys, or SigningKey.")
	}
	if len(cfg.SigningKeys) > 0 {
		for _, key := range cfg.SigningKeys {
			if key.Key == nil {
				panic("Fiber: JWT middleware configuration: SigningKey.Key cannot be nil")
			}
		}
	}
	if len(cfg.JWKSetURLs) > 0 {
		for _, u := range cfg.JWKSetURLs {
			parsed, err := url.Parse(u)
			if err != nil || parsed.Scheme == "" || parsed.Host == "" {
				panic("Fiber: JWT middleware configuration: Invalid JWK Set URL (must be absolute http/https): " + u)
			}
			if parsed.Scheme != "https" && parsed.Scheme != "http" {
				panic("Fiber: JWT middleware configuration: Unsupported JWK Set URL scheme: " + parsed.Scheme)
			}
		}
	}
	if cfg.Claims == nil {
		cfg.Claims = jwt.MapClaims{}
	}
	if cfg.Extractor.Extract == nil {
		cfg.Extractor = extractors.FromAuthHeader("Bearer")
	}

	if cfg.KeyFunc == nil {
		if len(cfg.SigningKeys) > 0 || len(cfg.JWKSetURLs) > 0 {
			var givenKeys map[string]keyfunc.GivenKey
			if cfg.SigningKeys != nil {
				givenKeys = make(map[string]keyfunc.GivenKey, len(cfg.SigningKeys))
				for kid, key := range cfg.SigningKeys {
					givenKeys[kid] = keyfunc.NewGivenCustom(key.Key, keyfunc.GivenKeyOptions{
						Algorithm: key.JWTAlg,
					})
				}
			}
			if len(cfg.JWKSetURLs) > 0 {
				var err error
				cfg.KeyFunc, err = multiKeyfunc(givenKeys, cfg.JWKSetURLs)
				if err != nil {
					panic("Failed to create keyfunc from JWK Set URL: " + err.Error())
				}
			} else {
				cfg.KeyFunc = keyfunc.NewGiven(givenKeys).Keyfunc
			}
		} else {
			cfg.KeyFunc = signingKeyFunc(cfg.SigningKey)
		}
	}

	return cfg
}

func multiKeyfunc(givenKeys map[string]keyfunc.GivenKey, jwkSetURLs []string) (jwt.Keyfunc, error) {
	opts := keyfuncOptions(givenKeys)
	multiple := make(map[string]keyfunc.Options, len(jwkSetURLs))
	for _, url := range jwkSetURLs {
		multiple[url] = opts
	}
	multiOpts := keyfunc.MultipleOptions{
		KeySelector: keyfunc.KeySelectorFirst,
	}
	multi, err := keyfunc.GetMultiple(multiple, multiOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to get multiple JWK Set URLs: %w", err)
	}
	return multi.Keyfunc, nil
}

func keyfuncOptions(givenKeys map[string]keyfunc.GivenKey) keyfunc.Options {
	return keyfunc.Options{
		GivenKeys: givenKeys,
		RefreshErrorHandler: func(err error) {
			log.Printf("Failed to perform background refresh of JWK Set: %s.", err)
		},
		RefreshInterval:   time.Hour,
		RefreshRateLimit:  time.Minute * 5,
		RefreshTimeout:    time.Second * 10,
		RefreshUnknownKID: true,
	}
}

func signingKeyFunc(key SigningKey) jwt.Keyfunc {
	return func(token *jwt.Token) (interface{}, error) {
		if key.JWTAlg != "" {
			alg, ok := token.Header["alg"].(string)
			if !ok {
				return nil, fmt.Errorf("unexpected jwt signing method: expected: %q: got: missing or unexpected JSON type", key.JWTAlg)
			}
			if alg != key.JWTAlg {
				return nil, fmt.Errorf("unexpected jwt signing method: expected: %q: got: %q", key.JWTAlg, alg)
			}
		}
		return key.Key, nil
	}
}
