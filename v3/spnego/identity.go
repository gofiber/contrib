package spnego

import "github.com/jcmturner/goidentity/v6"

type FiberContext interface {
	Locals(key any, value ...any) any
}

// SetAuthenticatedIdentityToContext stores the authenticated identity in the Fiber context.
// It takes a Fiber context and an identity, and sets it using the contextKeyOfIdentity key
// for later retrieval by other handlers in the request chain.
func SetAuthenticatedIdentityToContext[T FiberContext](ctx T, identity goidentity.Identity) {
	ctx.Locals(contextKeyOfIdentity, identity)
}

// GetAuthenticatedIdentityFromContext retrieves the authenticated identity from the Fiber context.
// It returns the identity and a boolean indicating if it was found.
// This function should be used by subsequent handlers to access the authenticated user's information.
// Example:
//
//	user, ok := GetAuthenticatedIdentityFromContext(ctx)
//	if ok {
//	    fmt.Printf("Authenticated user: %s\n", user.UserName())
//	}
func GetAuthenticatedIdentityFromContext[T FiberContext](ctx T) (goidentity.Identity, bool) {
	id, ok := ctx.Locals(contextKeyOfIdentity).(goidentity.Identity)
	return id, ok
}
