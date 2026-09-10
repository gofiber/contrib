package jwtware

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/extractors"
	"github.com/golang-jwt/jwt/v5"
)

// The RFC 6750 Section 3.1 error codes this middleware can report.
const (
	errorInvalidRequest = "invalid_request"
	errorInvalidToken   = "invalid_token"
)

// challengeReason enumerates the failures a challenge can describe. RFC 6750
// Section 3 allows a human readable error_description, and keeping the texts in
// a table lets every finished header value be assembled once, at startup.
type challengeReason int

const (
	reasonUnknown challengeReason = iota
	reasonMissing
	reasonCritical
	reasonAlgorithm
	reasonExpired
	reasonNotYetValid
	reasonUsedBeforeIssued
	reasonAudience
	reasonIssuer
	reasonMissingClaim
	reasonClaims
	reasonSignature
	reasonMalformed
	reasonUnverifiable
)

// challengeDescriptions holds the error_description text for every reason. The
// underlying error strings are deliberately not forwarded: they can name
// configuration details, and the grammar in RFC 6750 Section 3 admits neither
// quotes nor backslashes.
var challengeDescriptions = [...]string{
	reasonUnknown:          "",
	reasonMissing:          "The access token is missing or malformed",
	reasonCritical:         "The access token uses an unsupported critical extension",
	reasonAlgorithm:        "The access token is signed with an unexpected algorithm",
	reasonExpired:          "The access token expired",
	reasonNotYetValid:      "The access token is not valid yet",
	reasonUsedBeforeIssued: "The access token was used before it was issued",
	reasonAudience:         "The access token is meant for a different audience",
	reasonIssuer:           "The access token was issued by an unexpected issuer",
	reasonMissingClaim:     "The access token is missing a required claim",
	reasonClaims:           "The access token claims are invalid",
	reasonSignature:        "The access token signature is invalid",
	reasonMalformed:        "The access token is malformed",
	reasonUnverifiable:     "The access token could not be verified",
}

// challenge holds the authentication challenge that has to accompany a rejected
// request.
//
// RFC 9110 Section 15.5.2 requires every 401 response to carry a
// "WWW-Authenticate" header, and RFC 6750 Section 3 requires a resource server
// that refuses a bearer token to describe the failure in that header. The header
// values depend only on the configuration and the kind of failure, so they are
// all built when the middleware is, and a rejected request just looks one up.
type challenge struct {
	badRequest   []string
	unauthorized []string
}

// newChallenge renders the challenges for the configured credential source.
func newChallenge(cfg Config) *challenge {
	scheme := firstAuthScheme(cfg.Extractor)
	if scheme == "" {
		// The token does not travel in an Authorization header, but a 401 still
		// has to name a scheme the client can retry with, and for a JWT that is
		// the bearer scheme of RFC 6750.
		scheme = "Bearer"
	}
	prefix := fmt.Sprintf("%s realm=%q", scheme, cfg.Realm)

	ch := &challenge{
		badRequest:   make([]string, len(challengeDescriptions)),
		unauthorized: make([]string, len(challengeDescriptions)),
	}
	bearer := strings.EqualFold(scheme, "Bearer")
	for reason, description := range challengeDescriptions {
		// The error parameters are defined for the bearer scheme only, and
		// RFC 6750 Section 3.1 asks that a request which presented no usable
		// credential be answered without naming an error about one.
		if !bearer || challengeReason(reason) == reasonMissing {
			ch.badRequest[reason] = prefix
			ch.unauthorized[reason] = prefix
			continue
		}
		ch.badRequest[reason] = withError(prefix, errorInvalidRequest, description)
		ch.unauthorized[reason] = withError(prefix, errorInvalidToken, description)
	}
	return ch
}

// withError appends the RFC 6750 Section 3 error parameters to a challenge.
func withError(prefix, code, description string) string {
	if description == "" {
		return prefix + `, error="` + code + `"`
	}
	return prefix + `, error="` + code + `", error_description="` + description + `"`
}

// apply adds the challenge to a response that rejected the request, unless the
// error handler already wrote one of its own.
//
// handlerErr is what the error handler returned, so that a handler which only
// returns a *fiber.Error - leaving the status to Fiber's own error handler - is
// still recognised as a rejection. cause is the failure the handler was called
// with, and picks the error_description.
func (ch *challenge) apply(c fiber.Ctx, handlerErr, cause error) {
	status := c.Response().StatusCode()
	switch status {
	case fiber.StatusBadRequest, fiber.StatusUnauthorized, fiber.StatusProxyAuthRequired:
		// The handler wrote the status itself, so it needs no interpreting.
	default:
		var fiberErr *fiber.Error
		if errors.As(handlerErr, &fiberErr) {
			status = fiberErr.Code
		}
	}

	// RFC 6750 Section 3.1: credentials the resource server could not parse are
	// a bad request, anything that got as far as being verified is a bad token.
	header, values := fiber.HeaderWWWAuthenticate, ch.unauthorized
	switch status {
	case fiber.StatusBadRequest:
		values = ch.badRequest
	case fiber.StatusUnauthorized:
	case fiber.StatusProxyAuthRequired:
		header = fiber.HeaderProxyAuthenticate
	default:
		// Anything else is not an authentication challenge.
		return
	}

	// A challenge already on the response belongs to whoever put it there: an
	// error handler of the caller's, or another authentication middleware that
	// ran before this one and whose scheme the client may still be able to use.
	if len(c.Response().Header.Peek(header)) > 0 {
		return
	}

	c.Set(header, values[describe(cause)])
}

// describe classifies a failure so that the challenge can say what was wrong
// with the token without repeating the error itself.
func describe(err error) challengeReason {
	switch {
	case err == nil:
		return reasonUnknown
	case errors.Is(err, extractors.ErrNotFound):
		return reasonMissing
	case errors.Is(err, ErrCriticalHeader):
		return reasonCritical
	case errors.Is(err, ErrJWTAlg):
		return reasonAlgorithm
	case errors.Is(err, jwt.ErrTokenExpired):
		return reasonExpired
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return reasonNotYetValid
	case errors.Is(err, jwt.ErrTokenUsedBeforeIssued):
		return reasonUsedBeforeIssued
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return reasonAudience
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return reasonIssuer
	case errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		return reasonMissingClaim
	case errors.Is(err, jwt.ErrTokenInvalidClaims):
		return reasonClaims
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return reasonSignature
	case errors.Is(err, jwt.ErrTokenMalformed):
		return reasonMalformed
	case errors.Is(err, jwt.ErrTokenUnverifiable):
		return reasonUnverifiable
	default:
		return reasonUnknown
	}
}

// firstAuthScheme reports the Authorization header scheme the extractor accepts,
// walking a chain in the order it is tried. It returns an empty string when the
// token never comes from an Authorization header.
func firstAuthScheme(e extractors.Extractor) string {
	if e.Source == extractors.SourceAuthHeader && e.AuthScheme != "" {
		return e.AuthScheme
	}
	for _, chained := range e.Chain {
		if scheme := firstAuthScheme(chained); scheme != "" {
			return scheme
		}
	}
	return ""
}
