package jwtware

import (
	"errors"
	"fmt"
	"slices"
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

// newChallenge renders the challenges for the configured credential sources.
func newChallenge(cfg Config) *challenge {
	schemes := authSchemes(cfg.Extractor)
	if len(schemes) == 0 {
		// The token does not travel in an Authorization header, but a 401 still
		// has to name a scheme the client can retry with, and for a JWT that is
		// the bearer scheme of RFC 6750.
		schemes = []string{"Bearer"}
	}

	ch := &challenge{
		badRequest:   make([]string, len(challengeDescriptions)),
		unauthorized: make([]string, len(challengeDescriptions)),
	}
	for reason, description := range challengeDescriptions {
		ch.badRequest[reason] = render(schemes, cfg.Realm, errorInvalidRequest, description, challengeReason(reason))
		ch.unauthorized[reason] = render(schemes, cfg.Realm, errorInvalidToken, description, challengeReason(reason))
	}
	return ch
}

// render builds the header value for one failure: one challenge per scheme the
// extractor accepts, in the order a chain tries them. RFC 9110 Section 11.6.1
// carries them as a comma separated list, so a client that cannot use the first
// still sees the others.
func render(schemes []string, realm, code, description string, reason challengeReason) string {
	challenges := make([]string, 0, len(schemes))
	for _, scheme := range schemes {
		prefix := fmt.Sprintf("%s realm=%q", scheme, realm)

		// The error parameters are defined for the bearer scheme only, and
		// RFC 6750 Section 3.1 asks that a request which presented no usable
		// credential be answered without naming an error about one.
		if !strings.EqualFold(scheme, "Bearer") || reason == reasonMissing {
			challenges = append(challenges, prefix)
			continue
		}
		challenges = append(challenges, withError(prefix, code, description))
	}
	return strings.Join(challenges, ", ")
}

// withError appends the RFC 6750 Section 3 error parameters to a challenge.
func withError(prefix, code, description string) string {
	if description == "" {
		return prefix + `, error="` + code + `"`
	}
	return prefix + `, error="` + code + `", error_description="` + description + `"`
}

// apply adds the challenge to a response that rejected the request, unless the
// error handler already wrote one of its own. A challenge inherited from an
// earlier middleware is kept and this one added beside it.
//
// handlerErr is what the error handler returned, so that a handler which only
// returns a *fiber.Error - leaving the status to Fiber's own error handler - is
// still recognised as a rejection. cause is the failure the handler was called
// with, and picks the error_description.
//
// Those two are the whole of what can be read here. A handler that returns an
// error of its own for fiber.Config.ErrorHandler to map to a status later is
// answered without a challenge, because the status does not exist yet and the
// same handler maps other errors to 403 and 500, where a challenge would be
// wrong. The README tells such a handler to name the status itself.
func (ch *challenge) apply(c fiber.Ctx, handlerErr, cause error, inherited challengeHeaders) {
	// A handler that returns a *fiber.Error leaves the status to Fiber's own
	// error handler, and that code is the one the client will see; whatever is
	// on the response right now may belong to a middleware that ran earlier.
	// A handler that wrote its response returns nil, so the common path never
	// reaches errors.As.
	status := c.Response().StatusCode()
	if handlerErr != nil {
		// A typed-nil *fiber.Error satisfies errors.As without being something
		// to dereference, and Fiber's own error handler tolerates one.
		var fiberErr *fiber.Error
		if errors.As(handlerErr, &fiberErr) && fiberErr != nil {
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

	// Who put a challenge there decides what happens to it.
	switch existing := string(c.Response().Header.Peek(header)); {
	case existing == "":
		c.Set(header, values[describe(cause)])
	case existing == inherited.get(header):
		// Another authentication middleware ran before this one and its scheme
		// may still be one the client can use. RFC 9110 Section 11.6.1 lets the
		// field carry more than one challenge, and a client refused for a bad
		// token is owed the reason, so this one goes beside it rather than
		// instead of it.
		c.Set(header, existing+", "+values[describe(cause)])
	default:
		// The error handler wrote this for this rejection. It is the answer.
	}
}

// challengeHeaders is the pair of challenge headers as they stood before an
// error handler ran.
type challengeHeaders struct {
	wwwAuthenticate   string
	proxyAuthenticate string
}

// inheritedChallenges reads them off the response. Peeking an absent header
// yields an empty slice, which converts to "" without allocating, so the usual
// rejection - nothing there to inherit - stays allocation free.
func inheritedChallenges(c fiber.Ctx) challengeHeaders {
	return challengeHeaders{
		wwwAuthenticate:   string(c.Response().Header.Peek(fiber.HeaderWWWAuthenticate)),
		proxyAuthenticate: string(c.Response().Header.Peek(fiber.HeaderProxyAuthenticate)),
	}
}

// get returns the one belonging to the named header.
func (h challengeHeaders) get(header string) string {
	if header == fiber.HeaderProxyAuthenticate {
		return h.proxyAuthenticate
	}
	return h.wwwAuthenticate
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

// authSchemes lists the Authorization header schemes the extractor accepts, in
// the order a chain tries them and without repeats. It is empty when the token
// never comes from an Authorization header.
func authSchemes(e extractors.Extractor) []string {
	var schemes []string
	walkExtractor(&e, func(candidate *extractors.Extractor) bool {
		if candidate.Source != extractors.SourceAuthHeader || candidate.AuthScheme == "" {
			return false
		}
		if !slices.ContainsFunc(schemes, func(known string) bool {
			return strings.EqualFold(known, candidate.AuthScheme)
		}) {
			schemes = append(schemes, candidate.AuthScheme)
		}
		return false
	})
	return schemes
}
