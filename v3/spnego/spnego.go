package spnego

import (
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/gofiber/fiber/v3"
	flog "github.com/gofiber/fiber/v3/log"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/jcmturner/goidentity/v6"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/service"
	"github.com/jcmturner/gokrb5/v8/spnego"
)

// requestStateKey identifies the per-request state carried through the
// net/http adaptor. fasthttp's RequestCtx is itself the request's
// context.Context, so a user value set on it is readable as
// r.Context().Value(requestStateKey) inside the adapted handler.
type requestStateKey struct{}

// requestState carries the per-request data the adapted net/http handler needs.
// It cannot be closed over, because the handler is built once and shared by
// every request.
type requestState struct {
	ctx           fiber.Ctx
	handleErr     error
	authenticated bool
}

// handlerCache memoizes the SPNEGO handler built for a given keytab. Building
// it per request would re-wrap the net/http handler for the fasthttp adaptor on
// every call; keytabs change rarely, so a single-entry cache keyed on the
// keytab returned by the lookup is enough.
type handlerCache struct {
	build func(kt *keytab.Keytab) fiber.Handler

	mu      sync.Mutex
	keytab  *keytab.Keytab
	handler fiber.Handler
}

func (c *handlerCache) get(kt *keytab.Keytab) fiber.Handler {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handler != nil && c.keytab == kt {
		return c.handler
	}
	c.handler = c.build(kt)
	c.keytab = kt
	return c.handler
}

// New creates a new SPNEGO authentication middleware.
// It takes a Config struct and returns a Fiber handler or an error.
// The middleware handles Kerberos authentication for incoming requests using the
// SPNEGO protocol, verifying client credentials against the configured keytab.
func New(cfg Config) (fiber.Handler, error) {
	// Validate configuration
	keytabLookup := cfg.KeytabLookup
	if keytabLookup == nil {
		if !cfg.FallbackToSystemKeytab {
			return nil, ErrConfigInvalidOfKeytabLookupFunctionRequired
		}
		var err error
		if keytabLookup, err = NewSystemKeytabLookupFunc(); err != nil {
			return nil, err
		}
	}
	// Set default logger if fiber log using *log.Logger
	var opts = make([]func(settings *service.Settings), 0, 1)
	if cfg.Log != nil {
		opts = append(opts, service.Logger(cfg.Log))
	} else if l := flog.DefaultLogger[*log.Logger]().Logger(); l != nil {
		opts = append(opts, service.Logger(l))
	}
	// The inner handler runs only once SPNEGO has accepted the ticket. It reads
	// the request state from the request context rather than a closure, so that
	// the wrapped handler can be built once and reused.
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		state, ok := r.Context().Value(requestStateKey{}).(*requestState)
		if !ok {
			return
		}
		state.authenticated = true
		// Set the authenticated identity in the Fiber context
		SetAuthenticatedIdentityToContext(state.ctx, goidentity.FromHTTPRequestContext(r))
		// Call the next handler in the chain
		state.handleErr = state.ctx.Next()
	})
	handlers := &handlerCache{
		build: func(kt *keytab.Keytab) fiber.Handler {
			return adaptor.HTTPHandler(spnego.SPNEGOKRB5Authenticate(inner, kt, opts...))
		},
	}
	// Return the middleware handler
	return func(ctx fiber.Ctx) error {
		// Look up the keytab
		kt, err := keytabLookup()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrLookupKeytabFailed, err)
		}
		state := &requestState{ctx: ctx}
		ctx.RequestCtx().SetUserValue(requestStateKey{}, state)
		if err = handlers.get(kt)(ctx); err != nil {
			return err
		}
		if !state.authenticated && cfg.Unauthorized != nil {
			return cfg.Unauthorized(ctx)
		}
		return state.handleErr
	}, nil
}
