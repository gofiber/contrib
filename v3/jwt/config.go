package jwtware

import (
	"errors"
	"fmt"
	"log"
	"net/url"
	"slices"
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

	// Realm names the protected area in the "WWW-Authenticate" challenge that
	// RFC 9110 Section 15.5.2 and RFC 6750 Section 3 require on a rejected
	// request. The challenge is only added when the response does not already
	// carry one, so an ErrorHandler may write its own instead.
	//
	// Optional. Default: "Restricted"
	Realm string

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

	// ParserOptions provides additional options for JWT parsing.
	//
	// This is where per-application claim validation belongs, such as
	// jwt.WithAudience or jwt.WithIssuer: RFC 7519 Section 4.1.3 requires a JWT
	// whose "aud" claim does not identify this application to be rejected, and
	// only the application knows which value that is.
	//
	// Optional. Default: nil
	ParserOptions []jwt.ParserOption

	// KnownCriticalHeaders lists the JWS "crit" header parameters this
	// application understands and processes itself, for example by reading them
	// off the token returned by FromContext.
	//
	// RFC 7515 Section 4.1.11 requires a token that marks a header parameter
	// critical to be rejected unless the recipient understands that parameter,
	// so by default every such token is rejected.
	//
	// An extension that changes how the JWS is parsed or verified cannot be
	// listed here, because the parser has already decoded and verified the
	// token by the time the application sees it. "b64" (RFC 7797) is the one
	// such extension defined for JWS, and naming it panics at New.
	//
	// Optional. Default: nil
	KnownCriticalHeaders []string
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
			// A typed-nil *fiber.Error satisfies errors.As without being
			// something to dereference; leave it to Fiber, as its own error
			// handler does.
			var fiberErr *fiber.Error
			if errors.As(err, &fiberErr) && fiberErr != nil {
				return c.Status(fiberErr.Code).SendString(fiberErr.Message)
			}
			return c.Status(fiber.StatusUnauthorized).SendString("Invalid or expired JWT")
		}
	}
	if cfg.Realm == "" {
		cfg.Realm = "Restricted"
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

	// An extension that changes how the JWS is parsed cannot be handled by the
	// application, whatever it declares: the parser has already decoded the
	// token by the time anything of yours sees it.
	for _, name := range cfg.KnownCriticalHeaders {
		if _, unprocessable := unprocessableCriticalHeaders[name]; unprocessable {
			panic(fmt.Sprintf("Fiber: JWT middleware configuration: KnownCriticalHeaders cannot contain %q, which changes how the JWS is parsed; tokens carrying it are always rejected", name))
		}
	}

	// RFC 7515 Section 4.1.11 requires critical header parameters to be
	// understood before the JWS is accepted, and github.com/golang-jwt/jwt does
	// not check them. The guard wraps a caller supplied key function too, so the
	// rule holds however the keys are resolved.
	cfg.KeyFunc = criticalHeaderGuard(cfg.KeyFunc, knownCriticalHeaders(cfg.KnownCriticalHeaders))

	return cfg
}

// validAlgorithms reports the "alg" header values the configuration accepts, so
// that the parser can reject every other algorithm before a key is looked up.
//
// RFC 8725 Section 3.1 asks a recipient to decide which algorithms are
// acceptable from its own configuration rather than from the token. The key
// functions below already refuse a key whose algorithm does not match, and
// hoisting the check into the parser makes it happen earlier and uniformly.
// Only algorithms the caller pinned are returned: a configuration that does not
// name one keeps whatever the key material allows.
func validAlgorithms(config []Config) []string {
	if len(config) == 0 {
		return nil
	}
	cfg := config[0]

	switch {
	case cfg.KeyFunc != nil, len(cfg.JWKSetURLs) > 0:
		// The caller's key function, or the remote key set, decides which
		// algorithms are acceptable. The configuration does not know them.
		return nil
	case len(cfg.SigningKeys) > 0:
		algorithms := make([]string, 0, len(cfg.SigningKeys))
		for _, key := range cfg.SigningKeys {
			if key.JWTAlg == "" {
				// A single unrestricted key leaves the whole set unrestricted.
				return nil
			}
			if !slices.Contains(algorithms, key.JWTAlg) {
				algorithms = append(algorithms, key.JWTAlg)
			}
		}
		// Map iteration order is random; keep the parser's view stable.
		slices.Sort(algorithms)
		return algorithms
	case cfg.SigningKey.JWTAlg != "":
		return []string{cfg.SigningKey.JWTAlg}
	default:
		return nil
	}
}

// multiKeyfunc resolves keys against several JWK Set URLs at once, giving each
// of them the same refresh policy and taking the first key that matches.
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

// keyfuncOptions is the refresh policy every JWK Set in a configuration is
// fetched under: hourly in the background, rate limited, and re-fetched on a
// key ID the cache has not seen.
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

// signingKeyFunc returns the key function for a single configured key. A key
// that names an algorithm refuses a token signed with any other, wrapping
// ErrJWTAlg so callers can match on it; this is the check RFC 8725 Section 3.1
// asks for, and validAlgorithms hoists it into the parser where it can.
func signingKeyFunc(key SigningKey) jwt.Keyfunc {
	return func(token *jwt.Token) (interface{}, error) {
		if key.JWTAlg != "" {
			alg, ok := token.Header["alg"].(string)
			if !ok {
				return nil, fmt.Errorf("%w: unexpected jwt signing method: expected: %q: got: missing or unexpected JSON type", ErrJWTAlg, key.JWTAlg)
			}
			if alg != key.JWTAlg {
				return nil, fmt.Errorf("%w: unexpected jwt signing method: expected: %q: got: %q", ErrJWTAlg, key.JWTAlg, alg)
			}
		}
		return key.Key, nil
	}
}
