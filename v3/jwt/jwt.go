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
	// extractor of a chain supplied the token, and whether it came from the URL
	// at all, is only known per request, so the parameters it could have come
	// from are collected here and compared then.
	urlKeys := urlParams(cfg.Extractor)

	reject := func(c fiber.Ctx, err error) error {
		// Read before the handler runs: whatever is on the response now came
		// from a middleware that ran earlier, and anything the handler adds is
		// its own answer to this rejection. The two are treated differently.
		inherited := inheritedChallenges(c)

		handlerErr := errorHandler(c, err)
		authChallenge.apply(c, handlerErr, err, inherited)
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

		if urlCarriesCredential(c, urlKeys) {
			return keepPrivate(c, successHandler)
		}
		return successHandler(c)
	}
}

// urlParam names a parameter whose value the extractor may find in the request
// URL, and how to read it back.
type urlParam struct {
	key    string
	inPath bool
}

// urlParams lists those parameters, in the order a chain tries them. It is
// empty for the extractors that never read the URL, which is the default.
//
// The list covers every built-in source that can put the token in the URL and
// no others: a query parameter obviously, a form parameter because Fiber's
// FormValue searches the query string before the request body, and a route
// parameter because it is part of the path. A header, an Authorization header
// and a cookie never reach the URL. A SourceCustom extractor reads wherever its
// author wrote it to, so only they can say.
//
// Whether a given request's URL actually carries one is settled per request, by
// urlCarriesCredential.
//
// The chain is walked with a visited set, as the extractors package walks its
// own: the metadata is the caller's to build, and a chain that refers back to
// itself would otherwise be followed along every path through it.
func urlParams(e extractors.Extractor) []urlParam {
	var params []urlParam
	walkExtractor(&e, func(candidate *extractors.Extractor) bool {
		switch candidate.Source {
		case extractors.SourceQuery, extractors.SourceForm:
			// The empty key is a real parameter: Fiber reads "/?=<token>" from it.
			params = append(params, urlParam{key: candidate.Key})
		case extractors.SourceParam:
			params = append(params, urlParam{key: candidate.Key, inPath: true})
		}
		return false
	})
	return params
}

// urlCarriesCredential reports whether the request URL holds a value in one of
// those parameters, which is what makes it sensitive.
//
// It asks whether the parameter is populated, not whether its value is the
// credential that authenticated: what a shared cache keys on is the URL, so a
// chain that preferred a cookie over the query still answered a request whose
// URL carries a token, and storing that response under it would hand the next
// caller of that URL someone else's. A chain that reads only headers, cookies
// or the request body leaves the URL clean and its response alone.
func urlCarriesCredential(c fiber.Ctx, params []urlParam) bool {
	for _, param := range params {
		if param.inPath {
			// A route parameter that matched is part of the path by
			// construction, so having a value is what there is to ask.
			if c.Params(param.key) != "" {
				return true
			}
			continue
		}
		// Only the query string: a form parameter reaches this list because
		// FormValue reads the query before the body, and a value in the body is
		// not in the URL. The value has to be there, not just the key: the
		// extractors read "?token=" as no credential, so nothing sensitive is
		// in a URL that only names the parameter. Note that this is the value,
		// so FromQuery("") reading "/?=<token>" still counts.
		if len(c.Request().URI().QueryArgs().Peek(param.key)) > 0 {
			return true
		}
	}
	return false
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

// keepPrivate keeps the response to a request whose URL carries a token out of
// shared caches, as RFC 6750 Section 2.3 asks of a resource server.
//
// The directive goes on twice. Once before the handler runs, because a cache
// registered after this middleware reads the response and decides whether to
// store it as its own stack unwinds - which happens before control comes back
// here, so a directive added only afterwards would reach the client but not the
// cache that already stored the body. Once after, because only then is the
// handler's own policy known and mergeable, and because the status the RFC
// scopes this to is only known then.
//
// A handler that replaces the header outright while a cache sits between it and
// this middleware is the case neither pass can cover, since that cache reads the
// replacement before this function runs again. Moving the cache outside this
// middleware would not help: it would then answer before any of this runs, so a
// hit would skip authentication altogether. The README says to keep the cache
// inside and key it on whatever identifies the user.
func keepPrivate(c fiber.Ctx, next fiber.Handler) error {
	setPrivate(c)

	err := next(c)

	// Only a response a cache would store needs the directive, and the early
	// pass already covered anything downstream that has stored one by now.
	status := c.Response().StatusCode()
	if status < fiber.StatusOK || status >= fiber.StatusMultipleChoices {
		return err
	}

	setPrivate(c)
	return err
}

// setPrivate merges the directive into whatever policy the response carries.
func setPrivate(c fiber.Ctx) {
	existing := string(c.Response().Header.Peek(fiber.HeaderCacheControl))
	c.Set(fiber.HeaderCacheControl, privateCacheControl(existing))
}

// privateCacheControl returns value with any directive that would let a shared
// cache store the response replaced by one that would not. A policy the handler
// set is otherwise kept: only "public" is dropped, and "private" is added only
// when nothing already keeps the response out of shared caches.
func privateCacheControl(value string) string {
	if value == "" {
		return "private"
	}

	directives, wellFormed := splitCacheControl(value)
	if !wellFormed {
		// An unterminated quoted string swallows everything after it, so
		// appending "private" to one would produce a policy that does not
		// contain the directive at all. There is nothing to merge into: a
		// policy no cache can parse is replaced rather than extended.
		return "private"
	}
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
// directives, leaving the ones inside a quoted field list alone. It also reports
// whether the value was well formed, which it is not when a quoted string is
// left open.
func splitCacheControl(value string) ([]string, bool) {
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
	return directives, !quoted
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
