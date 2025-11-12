package spnego

import (
	"fmt"
	"log"
	"net/http"

	"github.com/gofiber/contrib/v3/spnego/utils"
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
func NewSpnegoKrb5AuthenticateMiddleware(cfg Config) (fiber.Handler, error) {
	// Validate configuration
	if cfg.KeytabLookup == nil {
		return nil, ErrConfigInvalidOfKeytabLookupFunctionRequired
	}
	// Set default logger if fiber log using *log.Logger
	var opts = make([]func(settings *service.Settings), 0, 1)
	if cfg.Log != nil {
		opts = append(opts, service.Logger(cfg.Log))
	} else if l := flog.DefaultLogger[*log.Logger]().Logger(); l != nil {
		opts = append(opts, service.Logger(l))
	}
	// Return the middleware handler
	return func(ctx fiber.Ctx) error {
		// Look up the keytab
		kt, err := cfg.KeytabLookup()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrLookupKeytabFailed, err)
		}
		// Create the SPNEGO handler using the keytab
		var handleErr error
		handler := spnego.SPNEGOKRB5Authenticate(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			// Set the authenticated identity in the Fiber context
			SetAuthenticatedIdentityToContext(ctx, goidentity.FromHTTPRequestContext(r))
			// Call the next handler in the chain
			handleErr = ctx.Next()
		}), kt, opts...)
		// Convert Fiber context to HTTP request
		rawReq, err := adaptor.ConvertRequest(ctx, true)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrConvertRequestFailed, err)
		}
		// Serve the request using the SPNEGO handler
		handler.ServeHTTP(utils.NewWrapFiberContext(ctx), rawReq)
		return handleErr
	}, nil
}
