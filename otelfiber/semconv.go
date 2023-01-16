package otelfiber

import (
	"encoding/base64"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.12.0"
)

func httpServerMetricAttributesFromRequest(c *fiber.Ctx, cfg config) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		httpFlavorAttribute(c),
		semconv.HTTPMethodKey.String(utils.CopyString(c.Method())),
		semconv.HTTPSchemeKey.String(utils.CopyString(c.Protocol())),
		semconv.NetHostNameKey.String(utils.CopyString(c.Hostname())),
	}

	if cfg.Port != nil {
		attrs = append(attrs, semconv.NetHostPortKey.Int(*cfg.Port))
	}

	if cfg.ServerName != nil {
		attrs = append(attrs, semconv.HTTPServerNameKey.String(*cfg.ServerName))
	}

	return attrs
}

func httpServerTraceAttributesFromRequest(c *fiber.Ctx, cfg config) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		httpFlavorAttribute(c),
		// utils.CopyString: we need to copy the string as fasthttp strings are by default
		// mutable so it will be unsafe to use in this middleware as it might be used after
		// the handler returns.
		semconv.HTTPMethodKey.String(utils.CopyString(c.Method())),
		semconv.HTTPRequestContentLengthKey.Int(c.Request().Header.ContentLength()),
		semconv.HTTPSchemeKey.String(utils.CopyString(c.Protocol())),
		semconv.HTTPTargetKey.String(string(utils.CopyBytes(c.Request().RequestURI()))),
		semconv.HTTPURLKey.String(utils.CopyString(c.OriginalURL())),
		semconv.HTTPUserAgentKey.String(string(utils.CopyBytes(c.Request().Header.UserAgent()))),
		semconv.NetHostNameKey.String(utils.CopyString(c.Hostname())),
		semconv.NetTransportTCP,
	}

	if cfg.Port != nil {
		attrs = append(attrs, semconv.NetHostPortKey.Int(*cfg.Port))
	}

	if cfg.ServerName != nil {
		attrs = append(attrs, semconv.HTTPServerNameKey.String(*cfg.ServerName))
	}

	if username, ok := hasBasicAuth(c.Get(fiber.HeaderAuthorization)); ok {
		attrs = append(attrs, semconv.EnduserIDKey.String(utils.CopyString(username)))
	}
	clientIP := c.IP()
	if len(clientIP) > 0 {
		attrs = append(attrs, semconv.HTTPClientIPKey.String(utils.CopyString(clientIP)))
	}

	return attrs
}

func httpFlavorAttribute(c *fiber.Ctx) attribute.KeyValue {
	if c.Request().Header.IsHTTP11() {
		return semconv.HTTPFlavorHTTP11
	}

	return semconv.HTTPFlavorHTTP10
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
