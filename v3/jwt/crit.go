package jwtware

import (
	"errors"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

// ErrCriticalHeader is returned when a token carries a "crit" (critical) header
// parameter that this middleware is not allowed to ignore.
//
// RFC 7515 Section 4.1.11 requires a recipient to reject a JWS whose "crit" list
// names an extension it does not understand, and lets it reject a "crit" value
// that violates the rules producers have to follow.
var ErrCriticalHeader = errors.New("the JWT header contains an unsupported critical extension")

// joseHeaderParameters holds the Header Parameter names the core JOSE
// specifications already define: RFC 7515 for JWS, RFC 7516 and RFC 7518 for
// JWE. RFC 7515 Section 4.1.11 forbids a producer from listing any of them in
// "crit", which only ever names extensions, so finding one there invalidates
// the token.
//
// The JWE names are here because a name JOSE has already defined is not an
// extension whatever the message is, and one of them in the "crit" list of a
// JWS is a producer with a bug rather than an extension a recipient could
// understand. "b64" (RFC 7797) is a real extension and belongs in "crit", so it
// is not listed here; unprocessableCriticalHeaders refuses it for its own
// reason.
var joseHeaderParameters = map[string]struct{}{
	// RFC 7515 Section 4.1, for JWS.
	"alg":      {},
	"jku":      {},
	"jwk":      {},
	"kid":      {},
	"x5u":      {},
	"x5c":      {},
	"x5t":      {},
	"x5t#S256": {},
	"typ":      {},
	"cty":      {},
	"crit":     {},
	// RFC 7516 Section 4.1, for JWE.
	"enc": {},
	"zip": {},
	// RFC 7518 Sections 4.6.1, 4.7.1 and 4.8.1, for JWE key management.
	"epk": {},
	"apu": {},
	"apv": {},
	"iv":  {},
	"tag": {},
	"p2s": {},
	"p2c": {},
}

// unprocessableCriticalHeaders names the critical extensions that no
// configuration can declare understood, because they change how the JWS itself
// is parsed or verified and this middleware only sees a token after the parser
// has done both.
//
// "b64" (RFC 7797) is the one such extension defined for JWS. With "b64": false
// the payload travels unencoded and the signature covers it verbatim;
// github.com/golang-jwt/jwt does not implement that and always base64url-decodes
// the second segment. Honouring a declaration would therefore let it read the
// decoding of a signed message as the claims of a token nobody issued.
var unprocessableCriticalHeaders = map[string]struct{}{
	"b64": {},
}

// criticalHeaderGuard wraps a key function with the "crit" check of RFC 7515
// Section 4.1.11, which github.com/golang-jwt/jwt does not perform.
//
// Key lookup is the last hook the parser offers before it verifies the
// signature, which is where step 5 of the validation procedure in RFC 7515
// Section 5.2 belongs. Tokens without a "crit" header - which is every token in
// practice - pay one map lookup for the check.
func criticalHeaderGuard(next jwt.Keyfunc, known map[string]struct{}) jwt.Keyfunc {
	return func(token *jwt.Token) (any, error) {
		if err := checkCriticalHeaders(token.Header, known); err != nil {
			return nil, err
		}
		return next(token)
	}
}

// checkCriticalHeaders reports whether the JOSE header may be processed, given
// the set of critical extensions the application declared it understands.
func checkCriticalHeaders(header map[string]any, known map[string]struct{}) error {
	// These are refused on sight, whatever their value and whether or not
	// "crit" names them: RFC 7797 Section 6 requires the "crit" entry, but a
	// producer that leaves it out still hands the parser a payload it may read
	// the wrong way round. Running before "crit" is looked at is also what keeps
	// a declaration from getting past the check.
	for name := range unprocessableCriticalHeaders {
		if _, present := header[name]; present {
			return fmt.Errorf(`%w: the %q header parameter changes how the JWS is parsed and cannot be honoured here`, ErrCriticalHeader, name)
		}
	}

	value, ok := header["crit"]
	if !ok {
		return nil
	}

	// RFC 7515 Section 4.1.11: the value is an array of strings, and the empty
	// array is not a legal value.
	names, ok := value.([]any)
	if !ok || len(names) == 0 {
		return fmt.Errorf(`%w: "crit" must be a non-empty array of header parameter names`, ErrCriticalHeader)
	}

	seen := make(map[string]struct{}, len(names))
	for _, entry := range names {
		name, ok := entry.(string)
		if !ok {
			return fmt.Errorf(`%w: "crit" must only contain header parameter names`, ErrCriticalHeader)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf(`%w: "crit" lists %q more than once`, ErrCriticalHeader, name)
		}
		seen[name] = struct{}{}

		if _, reserved := joseHeaderParameters[name]; reserved {
			return fmt.Errorf(`%w: "crit" must not list the registered header parameter %q`, ErrCriticalHeader, name)
		}
		if _, present := header[name]; !present {
			return fmt.Errorf(`%w: "crit" lists %q, which the header does not contain`, ErrCriticalHeader, name)
		}
		if _, understood := known[name]; !understood {
			return fmt.Errorf(`%w: the %q header parameter is not understood`, ErrCriticalHeader, name)
		}
	}

	return nil
}

// knownCriticalHeaders turns the configured extension names into the set the
// guard looks them up in. It returns nil for an empty configuration, which
// rejects every token that marks a header parameter critical.
func knownCriticalHeaders(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	known := make(map[string]struct{}, len(names))
	for _, name := range names {
		known[name] = struct{}{}
	}
	return known
}
