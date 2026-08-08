package spnego

import "github.com/jcmturner/goidentity/v6"

// FiberContext is the subset of fiber.Ctx used to carry the identity.
type FiberContext interface {
	Locals(key any, value ...any) any
}

// SetAuthenticatedIdentityToContext stores the identity for later handlers.
func SetAuthenticatedIdentityToContext(ctx FiberContext, identity goidentity.Identity) {
	ctx.Locals(contextKeyOfIdentity, identity)
}

// GetAuthenticatedIdentityFromContext returns the authenticated identity and
// whether one was found.
func GetAuthenticatedIdentityFromContext(ctx FiberContext) (goidentity.Identity, bool) {
	id, ok := ctx.Locals(contextKeyOfIdentity).(goidentity.Identity)
	return id, ok
}
