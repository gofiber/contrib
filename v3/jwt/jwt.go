// 🚀 Fiber is an Express inspired web framework written in Go with 💖
// 📌 API Documentation: https://fiber.wiki
// 📝 Github Repository: https://github.com/gofiber/fiber
// Special thanks to Echo: https://github.com/labstack/echo/blob/master/middleware/jwt.go

package jwtware

import (
	"reflect"
	"strings"

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
	authChallenge := newChallenge(cfg)

	// Hoisting the fields out of the config keeps the request path from chasing
	// the same pointers on every call.
	extract := cfg.Extractor.Extract
	keyFunc := cfg.KeyFunc
	next := cfg.Next
	processToken := cfg.TokenProcessorFunc
	successHandler := cfg.SuccessHandler
	errorHandler := cfg.ErrorHandler

	// A token read from the query string ends up in the URL a shared cache keys
	// on, so those responses need the RFC 6750 Section 2.3 directive. Which
	// extractor of a chain supplied the token is only known per request, so the
	// parameters it could have come from are collected here and compared then.
	queryKeys := queryParams(cfg.Extractor)

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

		// The extractor's own answer is what the URL held; a processor may turn
		// it into something else entirely.
		extracted := auth
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

		if fromQuery(c, queryKeys, extracted) {
			return keepPrivate(c, successHandler)
		}
		return successHandler(c)
	}
}

// queryParams lists the query string parameters the extractor may read the
// token from, in the order a chain tries them. It is empty for the extractors
// that never look at the query, which is the default.
//
// The chain is walked with a visited set, as the extractors package walks its
// own: the metadata is the caller's to build, and a chain that refers back to
// itself would otherwise be followed along every path through it.
func queryParams(e extractors.Extractor) []string {
	var params []string
	walkExtractor(&e, func(candidate *extractors.Extractor) bool {
		if candidate.Source == extractors.SourceQuery {
			// The empty key is a real parameter: Fiber reads "/?=<token>" from it.
			params = append(params, candidate.Key)
		}
		return false
	})
	return params
}

// walkExtractor visits an extractor and its chain in the order a chain is
// tried, stopping early when visit returns true. Each node is visited once.
func walkExtractor(e *extractors.Extractor, visit func(*extractors.Extractor) bool) bool {
	visited := make(map[*extractors.Extractor]struct{})

	var walk func(*extractors.Extractor) bool
	walk = func(candidate *extractors.Extractor) bool {
		if _, seen := visited[candidate]; seen {
			return false
		}
		visited[candidate] = struct{}{}

		if visit(candidate) {
			return true
		}
		for i := range candidate.Chain {
			if walk(&candidate.Chain[i]) {
				return true
			}
		}
		return false
	}
	return walk(e)
}

// fromQuery reports whether the token the extractor returned is the value of
// one of those parameters, which is what makes the URL sensitive. A chain that
// answered from a header or a cookie leaves the URL clean, and its response is
// none of this function's business.
func fromQuery(c fiber.Ctx, params []string, token string) bool {
	for _, param := range params {
		if c.Query(param) == token {
			return true
		}
	}
	return false
}

// keepPrivate runs the handler and then keeps its response out of shared
// caches, as RFC 6750 Section 2.3 asks of a resource server answering a request
// whose token travelled in the URL.
func keepPrivate(c fiber.Ctx, next fiber.Handler) error {
	err := next(c)

	// Only a response a cache would store needs the directive.
	status := c.Response().StatusCode()
	if status < fiber.StatusOK || status >= fiber.StatusMultipleChoices {
		return err
	}

	existing := string(c.Response().Header.Peek(fiber.HeaderCacheControl))
	c.Set(fiber.HeaderCacheControl, privateCacheControl(existing))
	return err
}

// privateCacheControl returns value with any directive that would let a shared
// cache store the response replaced by one that would not. A policy the handler
// set is otherwise kept: only "public" is dropped, and "private" is added only
// when nothing already keeps the response out of shared caches.
func privateCacheControl(value string) string {
	if value == "" {
		return "private"
	}

	directives := splitCacheControl(value)
	kept := directives[:0]
	private := false
	for _, directive := range directives {
		name := directive
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "public":
			// Contradicts what this response needs; drop it.
			continue
		case "no-store", "private":
			// RFC 9111 Section 5.2.3: a recipient ignores a directive carrying
			// an argument the directive is not defined to take, and Section
			// 5.2.2.7's field-scoped "private" leaves the body storable by a
			// shared cache either way. Only the bare forms settle it.
			private = private || !strings.Contains(directive, "=")
		}
		kept = append(kept, directive)
	}
	if !private {
		kept = append(kept, "private")
	}

	return strings.Join(kept, ", ")
}

// splitCacheControl splits a Cache-Control value on the commas that separate
// directives, leaving the ones inside a quoted field list alone.
func splitCacheControl(value string) []string {
	var (
		directives []string
		start      int
		quoted     bool
	)
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\':
			// RFC 9110 Section 5.6.4: inside a quoted string a backslash quotes
			// the next character, so neither of them ends anything.
			if quoted {
				i++
			}
		case '"':
			quoted = !quoted
		case ',':
			if quoted {
				continue
			}
			if directive := strings.TrimSpace(value[start:i]); directive != "" {
				directives = append(directives, directive)
			}
			start = i + 1
		}
	}
	if directive := strings.TrimSpace(value[start:]); directive != "" {
		directives = append(directives, directive)
	}
	return directives
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
