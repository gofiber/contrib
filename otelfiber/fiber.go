package otelfiber

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	otelcontrib "go.opentelemetry.io/contrib"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	tracerKey          = "gofiber-contrib-tracer-fiber"
	tracerName         = "github.com/gofiber/contrib/otelfiber"
	defaultServiceName = "fiber-server"
)

// Middleware returns fiber handler which will trace incoming requests.
func Middleware(service string, opts ...Option) fiber.Handler {
	if service == "" {
		service = defaultServiceName
	}
	cfg := config{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.TracerProvider == nil {
		cfg.TracerProvider = otel.GetTracerProvider()
	}
	tracer := cfg.TracerProvider.Tracer(
		tracerName,
		oteltrace.WithInstrumentationVersion(otelcontrib.SemVersion()),
	)
	if cfg.Propagators == nil {
		cfg.Propagators = otel.GetTextMapPropagator()
	}

	if cfg.SpanNameFormatter == nil {
		cfg.SpanNameFormatter = defaultSpanNameFormatter
	}

	return func(c *fiber.Ctx) error {
		c.Locals(tracerKey, tracer)
		savedCtx, cancel := context.WithCancel(c.UserContext())

		defer func() {
			c.SetUserContext(savedCtx)
			cancel()
		}()

		reqHeader := make(http.Header)
		c.Request().Header.VisitAll(func(k, v []byte) {
			reqHeader.Add(string(k), string(v))
		})

		ctx := cfg.Propagators.Extract(savedCtx, propagation.HeaderCarrier(reqHeader))
		opts := []oteltrace.SpanStartOption{
			oteltrace.WithAttributes(
				// utils.CopyString: we need to copy the string as fasthttp strings are by default
				// mutable so it will be unsafe to use in this middleware as it might be used after
				// the handler returns.
				semconv.HTTPServerNameKey.String(service),
				semconv.HTTPMethodKey.String(utils.CopyString(c.Method())),
				semconv.HTTPTargetKey.String(string(utils.CopyBytes(c.Request().RequestURI()))),
				semconv.HTTPURLKey.String(utils.CopyString(c.OriginalURL())),
				semconv.NetHostIPKey.String(utils.CopyString(c.IP())),
				semconv.NetHostNameKey.String(utils.CopyString(c.Hostname())),
				semconv.HTTPUserAgentKey.String(string(utils.CopyBytes(c.Request().Header.UserAgent()))),
				semconv.HTTPRequestContentLengthKey.Int(c.Request().Header.ContentLength()),
				semconv.HTTPSchemeKey.String(utils.CopyString(c.Protocol())),
				semconv.NetTransportTCP),
			oteltrace.WithSpanKind(oteltrace.SpanKindServer),
		}
		if username, ok := hasBasicAuth(c.Get(fiber.HeaderAuthorization)); ok {
			opts = append(opts, oteltrace.WithAttributes(semconv.EnduserIDKey.String(utils.CopyString(username))))
		}
		if len(c.IPs()) > 0 {
			opts = append(opts, oteltrace.WithAttributes(semconv.HTTPClientIPKey.String(utils.CopyString(c.IPs()[0]))))
		}
		// temporary set to c.Path() first
		// update with c.Route().Path after c.Next() is called
		// to get pathRaw
		spanName := utils.CopyString(c.Path())
		ctx, span := tracer.Start(ctx, spanName, opts...)
		defer span.End()

		// pass the span through userContext
		c.SetUserContext(ctx)

		// serve the request to the next middleware
		err := c.Next()

		span.SetName(cfg.SpanNameFormatter(c))
		// no need to copy c.Route().Path: route strings should be immutable across app lifecycle
		span.SetAttributes(semconv.HTTPRouteKey.String(c.Route().Path))

		if err != nil {
			span.RecordError(err)
			// invokes the registered HTTP error handler
			// to get the correct response status code
			_ = c.App().Config().ErrorHandler(c, err)
		}

		attrs := semconv.HTTPAttributesFromHTTPStatusCode(c.Response().StatusCode())
		spanStatus, spanMessage := semconv.SpanStatusFromHTTPStatusCodeAndSpanKind(c.Response().StatusCode(), oteltrace.SpanKindServer)
		span.SetAttributes(attrs...)
		span.SetStatus(spanStatus, spanMessage)

		return nil
	}
}

// defaultSpanNameFormatter is the default formatter for spans created with the fiber
// integration. Returns the route pathRaw
func defaultSpanNameFormatter(ctx *fiber.Ctx) string {
	return ctx.Route().Path
}

func hasBasicAuth(auth string) (string, bool) {
	if auth == "" {
		return "", false
	}

	// Check if the Authorization header is Basic
	if !strings.HasPrefix(auth, "Basic ") {
		return "", false
	}

	// Decode the header contents
	raw, err := base64.StdEncoding.DecodeString(auth[6:])
	if err != nil {
		return "", false
	}

	// Get the credentials
	creds := utils.UnsafeString(raw)

	// Check if the credentials are in the correct form
	// which is "username:password".
	index := strings.Index(creds, ":")
	if index == -1 {
		return "", false
	}

	// Get the username
	return creds[:index], true
}
