package sentry

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/getsentry/sentry-go"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/utils/v2"
)

// New creates a new middleware handler
func New(config ...Config) fiber.Handler {
	// Set default config
	cfg := configDefault(config...)

	// Return new handler
	return func(c fiber.Ctx) error {
		// Convert fiber request to http request
		r, err := adaptor.ConvertRequest(c, true)
		if err != nil {
			return err
		}

		// The converted request aliases fasthttp's reusable buffers and must
		// not be referenced once the handler has returned. Sentry serializes
		// events on background goroutines, so store a deep copy in the scope.
		body := utils.CopyBytes(c.Body())
		r = cloneRequest(r, body)

		// Init sentry hub
		hub := sentry.CurrentHub().Clone()
		scope := hub.Scope()
		scope.SetRequest(r)
		scope.SetRequestBody(body)
		fiber.StoreInContext(c, hubKey, hub)

		// Catch panics
		defer func() {
			if err := recover(); err != nil {
				eventID := hub.RecoverWithContext(
					context.WithValue(context.Background(), sentry.RequestContextKey, c),
					err,
				)

				if eventID != nil && cfg.WaitForDelivery {
					hub.Flush(cfg.Timeout)
				}

				if cfg.Repanic {
					panic(err)
				}
			}
		}()

		// Return err if exist, else move to next handler
		return c.Next()
	}
}

// cloneRequest returns a deep copy of r whose strings no longer alias
// fasthttp's reusable buffers, so it can safely outlive the request handler.
// body must already be a copy that does not alias fasthttp memory.
func cloneRequest(r *http.Request, body []byte) *http.Request {
	rc := new(http.Request)
	*rc = *r

	rc.Method = strings.Clone(r.Method)
	rc.Proto = strings.Clone(r.Proto)
	rc.Host = strings.Clone(r.Host)
	rc.RequestURI = strings.Clone(r.RequestURI)
	rc.RemoteAddr = strings.Clone(r.RemoteAddr)
	rc.URL = cloneURL(r.URL)
	rc.Body = io.NopCloser(bytes.NewReader(body))

	if r.Header != nil {
		h := make(http.Header, len(r.Header))
		for k, vv := range r.Header {
			cvv := make([]string, len(vv))
			for i, v := range vv {
				cvv[i] = strings.Clone(v)
			}
			h[strings.Clone(k)] = cvv
		}
		rc.Header = h
	}

	if r.TransferEncoding != nil {
		te := make([]string, len(r.TransferEncoding))
		for i, v := range r.TransferEncoding {
			te[i] = strings.Clone(v)
		}
		rc.TransferEncoding = te
	}

	return rc
}

// cloneURL returns a deep copy of u; the parsed URL fields are sub-strings of
// the request URI buffer and alias fasthttp memory as well.
func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	uc := new(url.URL)
	*uc = *u

	uc.Scheme = strings.Clone(u.Scheme)
	uc.Opaque = strings.Clone(u.Opaque)
	uc.Host = strings.Clone(u.Host)
	uc.Path = strings.Clone(u.Path)
	uc.RawPath = strings.Clone(u.RawPath)
	uc.RawQuery = strings.Clone(u.RawQuery)
	uc.Fragment = strings.Clone(u.Fragment)
	uc.RawFragment = strings.Clone(u.RawFragment)
	if u.User != nil {
		if pw, ok := u.User.Password(); ok {
			uc.User = url.UserPassword(strings.Clone(u.User.Username()), strings.Clone(pw))
		} else {
			uc.User = url.User(strings.Clone(u.User.Username()))
		}
	}

	return uc
}

// MustGetHubFromContext returns the Sentry hub from context.
// It accepts fiber.CustomCtx, fiber.Ctx, *fasthttp.RequestCtx, and context.Context.
// Panics if the hub is not found or has an unexpected type.
func MustGetHubFromContext(ctx any) *sentry.Hub {
	hub := GetHubFromContext(ctx)
	if hub == nil {
		panic("sentry: hub not found in context or has unexpected type")
	}

	return hub
}

// GetHubFromContext returns the Sentry hub from context.
// It accepts fiber.CustomCtx, fiber.Ctx, *fasthttp.RequestCtx, and context.Context.
func GetHubFromContext(ctx any) *sentry.Hub {
	hub, ok := fiber.ValueFromContext[*sentry.Hub](ctx, hubKey)
	if !ok {
		return nil
	}
	return hub
}
