// Package v3 provides SPNEGO authentication middleware for Fiber v3.
// This middleware enables Kerberos authentication for incoming requests
// using the SPNEGO protocol, allowing seamless integration with Active Directory
// and other Kerberos-based authentication systems.
package v3

import (
	"fmt"
	"log"
	"net/http"

	spnego2 "github.com/gofiber/contrib/spnego"
	"github.com/gofiber/contrib/spnego/utils"
	"github.com/gofiber/fiber/v3"
	flog "github.com/gofiber/fiber/v3/log"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/jcmturner/goidentity/v6"
	"github.com/jcmturner/gokrb5/v8/service"
	"github.com/jcmturner/gokrb5/v8/spnego"
)

// NewSpnegoKrb5AuthenticateMiddleware creates a new SPNEGO authentication middleware.
// It takes a Config struct and returns a Fiber handler or an error.
// The middleware handles Kerberos authentication for incoming requests using the
// SPNEGO protocol, verifying client credentials against the configured keytab.
func NewSpnegoKrb5AuthenticateMiddleware(cfg spnego2.Config) (fiber.Handler, error) {
	// Validate configuration
	if cfg.KeytabLookup == nil {
		return nil, spnego2.ErrConfigInvalidOfKeytabLookupFunctionRequired
	}
	// Set default logger if not provided
	if cfg.Log == nil {
		cfg.Log = flog.DefaultLogger().Logger().(*log.Logger)
	}
	// Return the middleware handler
	return func(ctx fiber.Ctx) error {
		// Look up the keytab
		kt, err := cfg.KeytabLookup()
		if err != nil {
			return fmt.Errorf("%w: %w", spnego2.ErrLookupKeytabFailed, err)
		}
		// Create the SPNEGO handler using the keytab
		var handleErr error
		handler := spnego.SPNEGOKRB5Authenticate(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			// Set the authenticated identity in the Fiber context
			spnego2.SetAuthenticatedIdentityToContext(ctx, goidentity.FromHTTPRequestContext(r))
			// Call the next handler in the chain
			handleErr = ctx.Next()
		}), kt, service.Logger(cfg.Log))
		// Convert Fiber context to HTTP request
		rawReq, err := adaptor.ConvertRequest(ctx, true)
		if err != nil {
			return fmt.Errorf("%w: %w", spnego2.ErrConvertRequestFailed, err)
		}
		// Serve the request using the SPNEGO handler
		handler.ServeHTTP(utils.NewWrapFiberContext(ctx), rawReq)
		return handleErr
	}, nil
}
