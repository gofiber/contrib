package spnego

import (
	"fmt"
	"log"
	"net/http"

	"github.com/gofiber/fiber/v3"
	flog "github.com/gofiber/fiber/v3/log"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/jcmturner/goidentity/v6"
	"github.com/jcmturner/gokrb5/v8/service"
	"github.com/jcmturner/gokrb5/v8/spnego"
)

// New creates a new SPNEGO authentication middleware.
// It takes a Config struct and returns a Fiber handler or an error.
// The middleware handles Kerberos authentication for incoming requests using the
// SPNEGO protocol, verifying client credentials against the configured keytab.
func New(cfg Config) (fiber.Handler, error) {
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
			return fmt.Errorf("%w: %v", ErrLookupKeytabFailed, err)
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
			return fmt.Errorf("%w: %v", ErrConvertRequestFailed, err)
		}
		// Serve the request using the SPNEGO handler
		handler.ServeHTTP(&wrapContext{ctx: ctx}, rawReq)
		return handleErr
	}, nil
}

// wrapContext adapts a Fiber context to the http.ResponseWriter interface
// This allows Fiber to work with libraries that expect the standard http.ResponseWriter
// T represents the type of the Fiber context (v2 or v3 compatible).
type wrapContext struct {
	ctx fiber.Ctx
}

// Header returns the response headers from the Fiber context
// in the standard http.Header format
// note: write header must using fiber context
func (f *wrapContext) Header() http.Header {
	headers := make(http.Header, f.ctx.Response().Header.Len())
	for k, v := range f.ctx.Response().Header.All() {
		headers.Set(string(k), string(v))
	}
	return headers
}

// Write writes bytes to the response body using the Fiber context's Write method
func (f *wrapContext) Write(bytes []byte) (int, error) {
	return f.ctx.Write(bytes)
}

// WriteHeader sets the HTTP status code using the Fiber context's Status method
func (f *wrapContext) WriteHeader(statusCode int) {
	f.ctx.Status(statusCode)
}
