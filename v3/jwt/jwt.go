// 🚀 Fiber is an Express inspired web framework written in Go with 💖
// 📌 API Documentation: https://fiber.wiki
// 📝 Github Repository: https://github.com/gofiber/fiber
// Special thanks to Echo: https://github.com/labstack/echo/blob/master/middleware/jwt.go

package jwtware

import (
	"reflect"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/extractors"
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
	// The accepted algorithms have to be read from the configuration as the
	// caller wrote it: makeCfg installs a key function of its own, which
	// afterwards is indistinguishable from one the caller supplied.
	validMethods := validAlgorithms(config)
	cfg := makeCfg(config)

	// Everything that depends on the configuration alone is resolved here, once,
	// so that a request pays for nothing but its own token. A parser is
	// read-only once built, so a single one serves every request.
	options := make([]jwt.ParserOption, 0, len(cfg.ParserOptions)+1)
	if len(validMethods) > 0 {
		options = append(options, jwt.WithValidMethods(validMethods))
	}
	// The caller's options come last, so an explicit jwt.WithValidMethods wins.
	options = append(options, cfg.ParserOptions...)
	parser := jwt.NewParser(options...)

	newClaims := claimsFactory(cfg.Claims)
	authChallenge := newChallenge(cfg, customErrorHandler(config))

	// Hoisting the fields out of the config keeps the request path from chasing
	// the same pointers on every call.
	extract := cfg.Extractor.Extract
	keyFunc := cfg.KeyFunc
	next := cfg.Next
	processToken := cfg.TokenProcessorFunc
	successHandler := cfg.SuccessHandler
	errorHandler := cfg.ErrorHandler

	if readsQuery(cfg.Extractor) {
		successHandler = markPrivate(successHandler)
	}

	reject := func(c fiber.Ctx, err error) error {
		handlerErr := errorHandler(c, err)
		authChallenge.apply(c, handlerErr, err)
		return handlerErr
	}

	// Return middleware handler
	return func(c fiber.Ctx) error {
		// Filter request to skip middleware
		if next != nil && next(c) {
			return c.Next()
		}

		auth, err := extract(c)
		if err != nil {
			return reject(c, err)
		}

		if processToken != nil {
			if auth, err = processToken(auth); err != nil {
				return reject(c, err)
			}
		}

		token, err := parser.ParseWithClaims(auth, newClaims(), keyFunc)
		if err != nil {
			return reject(c, err)
		}
		if !token.Valid {
			// The parser only reports success for a valid token, but a token is
			// not trusted on the strength of a nil error alone.
			return reject(c, jwt.ErrTokenUnverifiable)
		}

		// Store user information from token into context.
		fiber.StoreInContext(c, tokenKey, token)
		return successHandler(c)
	}
}

// readsQuery reports whether the extractor can take the token from the query
// string, where it ends up in URLs that caches and logs keep.
func readsQuery(e extractors.Extractor) bool {
	return e.Contains(func(candidate extractors.Extractor) bool {
		return candidate.Source == extractors.SourceQuery
	})
}

// markPrivate wraps a handler so that a successful response to a request whose
// token travelled in the query string is kept out of shared caches, which
// RFC 6750 Section 2.3 asks of a resource server. A handler that states its own
// caching policy keeps it.
func markPrivate(next fiber.Handler) fiber.Handler {
	return func(c fiber.Ctx) error {
		err := next(c)

		status := c.Response().StatusCode()
		if status < fiber.StatusOK || status >= fiber.StatusMultipleChoices {
			return err
		}
		if len(c.Response().Header.Peek(fiber.HeaderCacheControl)) == 0 {
			c.Set(fiber.HeaderCacheControl, "private")
		}
		return err
	}
}

// claimsFactory returns a function that allocates an empty claims value of the
// configured type. Resolving the type once keeps reflection off the request
// path, where only the allocation itself is unavoidable.
//
// The configured value supplies the type, never the contents: claims parsed from
// one request must not be visible to the next.
func claimsFactory(configured jwt.Claims) func() jwt.Claims {
	if _, ok := configured.(jwt.MapClaims); ok {
		return func() jwt.Claims { return jwt.MapClaims{} }
	}

	claimsType := reflect.TypeOf(configured)
	if claimsType == nil {
		panic("Fiber: JWT middleware configuration: Claims type cannot be nil")
	}
	if claimsType.Kind() == reflect.Pointer {
		claimsType = claimsType.Elem()
	}
	if _, ok := reflect.New(claimsType).Interface().(jwt.Claims); !ok {
		panic("Fiber: JWT middleware configuration: Claims type does not implement jwt.Claims")
	}

	return func() jwt.Claims {
		// The assertion held for this type when the middleware was built.
		claims, _ := reflect.New(claimsType).Interface().(jwt.Claims)
		return claims
	}
}

// FromContext returns the token from the context.
// It accepts fiber.CustomCtx, fiber.Ctx, *fasthttp.RequestCtx, and context.Context.
// If there is no token, nil is returned.
func FromContext(ctx any) *jwt.Token {
	token, ok := fiber.ValueFromContext[*jwt.Token](ctx, tokenKey)
	if !ok {
		return nil
	}
	return token
}
