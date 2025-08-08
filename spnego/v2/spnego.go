// Package v2 provides SPNEGO authentication middleware for Fiber v2.
// This middleware enables Kerberos authentication for incoming requests
// using the SPNEGO protocol, allowing seamless integration with Active Directory
// and other Kerberos-based authentication systems.
package v2

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/gofiber/contrib/spnego/config"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"github.com/jcmturner/goidentity/v6"
	"github.com/jcmturner/gokrb5/v8/service"
	"github.com/jcmturner/gokrb5/v8/spnego"
)

// NewSpnegoKrb5AuthenticateMiddleware creates a new SPNEGO authentication middleware.
// It takes a Config struct and returns a Fiber handler or an error.
// The middleware handles Kerberos authentication for incoming requests using the
// SPNEGO protocol, verifying client credentials against the configured keytab.
func NewSpnegoKrb5AuthenticateMiddleware(cfg *config.Config) (fiber.Handler, error) {
	// Validate configuration
	if cfg == nil {
		cfg = &config.Config{}
	}
	if cfg.KeytabLookup == nil {
		return nil, config.ErrConfigInvalidOfKeytabLookupFunctionRequired
	}
	// Set default logger if not provided
	if cfg.Log == nil {
		// Due to differences between Fiber v2 and v3 versions, internal log.Log cannot be obtained, so a new one is created in the same way
		cfg.Log = log.New(os.Stderr, "", log.LstdFlags|log.Lshortfile|log.Lmicroseconds)
	}
	// Return the middleware handler
	return func(ctx *fiber.Ctx) error {
		// Look up the keytab
		kt, err := cfg.KeytabLookup()
		if err != nil {
			return fmt.Errorf("%w: %w", config.ErrLookupKeytabFailed, err)
		}
		// Create the SPNEGO handler using the keytab
		var handleErr error
		handler := spnego.SPNEGOKRB5Authenticate(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			// Set the authenticated identity in the Fiber context
			setAuthenticatedIdentityToContext(ctx, goidentity.FromHTTPRequestContext(r))
			// Call the next handler in the chain
			handleErr = ctx.Next()
		}), kt, service.Logger(cfg.Log))
		// Convert Fiber context to HTTP request
		rawReq, err := adaptor.ConvertRequest(ctx, true)
		if err != nil {
			return fmt.Errorf("%w: %w", config.ErrConvertRequestFailed, err)
		}
		// Serve the request using the SPNEGO handler
		handler.ServeHTTP(wrapCtx{ctx}, rawReq)
		return handleErr
	}, nil
}

// setAuthenticatedIdentityToContext stores the authenticated identity in the Fiber context.
// It takes a Fiber context pointer and an identity, and sets it using the ContextKeyOfIdentity key
// for later retrieval by other handlers in the request chain.
func setAuthenticatedIdentityToContext(ctx *fiber.Ctx, identity goidentity.Identity) {
	ctx.Locals(config.ContextKeyOfIdentity, identity)
}

// GetAuthenticatedIdentityFromContext retrieves the authenticated identity from the Fiber context.
// It returns the identity and a boolean indicating if it was found.
// This function should be used by subsequent handlers to access the authenticated user's information.
//
// Example:
//
//	user, ok := GetAuthenticatedIdentityFromContext(ctx)
//	if ok {
//	    fmt.Printf("Authenticated user: %s\n", user.UserName())
//	}
func GetAuthenticatedIdentityFromContext(ctx *fiber.Ctx) (goidentity.Identity, bool) {
	id, ok := ctx.Locals(config.ContextKeyOfIdentity).(goidentity.Identity)
	return id, ok
}

// wrapCtx wraps a Fiber context pointer to implement the http.ResponseWriter interface.
// This adapter allows the Fiber context to be used with standard HTTP handlers
// that expect an http.ResponseWriter, bridging the gap between Fiber's context
// model and the standard library's HTTP interfaces.

type wrapCtx struct {
	*fiber.Ctx
}

// Header returns the request headers from the wrapped Fiber context.
// This method implements the http.ResponseWriter interface.
func (w wrapCtx) Header() http.Header {
	return w.Ctx.GetReqHeaders()
}

// WriteHeader sets the HTTP status code on the wrapped Fiber context.
// This method implements the http.ResponseWriter interface.
func (w wrapCtx) WriteHeader(statusCode int) {
	w.Ctx.Status(statusCode)
}
