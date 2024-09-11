package otelfiber

import (
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

func httpServerMetricAttributesFromRequest(c *fiber.Ctx, cfg config) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		networkProtocolVersion(c),
		semconv.HTTPRequestMethodKey.String(utils.CopyString(c.Method())),
		semconv.URLScheme(utils.CopyString(c.Protocol())),
		semconv.ServerAddress(utils.CopyString(c.Hostname())),
	}

	if cfg.Port != nil {
		attrs = append(attrs, semconv.ServerPort(*cfg.Port))
	}

	if cfg.ServiceName != nil {
		attrs = append(attrs, semconv.ServiceName(*cfg.ServiceName))
	}

	if cfg.CustomMetricAttributes != nil {
		attrs = append(attrs, cfg.CustomMetricAttributes(c)...)
	}

	return attrs
}

func httpServerTraceAttributesFromRequest(c *fiber.Ctx, cfg config) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		networkProtocolVersion(c),
		// utils.CopyString: we need to copy the string as fasthttp strings are by default
		// mutable so it will be unsafe to use in this middleware as it might be used after
		// the handler returns.
		semconv.HTTPRequestMethodKey.String(utils.CopyString(c.Method())),
		attribute.StringSlice(AttributeNameHTTPRequestHeaderContentLength, []string{strconv.Itoa(c.Request().Header.ContentLength())}),
		semconv.URLScheme(utils.CopyString(c.Protocol())),
		semconv.URLOriginal(utils.CopyString(c.OriginalURL())),
		semconv.URLPath(string(utils.CopyBytes(c.Request().URI().Path()))),
		semconv.URLQuery(string(utils.CopyBytes(c.Request().URI().QueryString()))),
		semconv.UserAgentOriginal(string(utils.CopyBytes(c.Request().Header.UserAgent()))),
		semconv.ServerAddress(utils.CopyString(c.Hostname())),
		semconv.NetworkTransportTCP,
	}

	if cfg.Port != nil {
		attrs = append(attrs, semconv.ServerPort(*cfg.Port))
	}

	if cfg.ServiceName != nil {
		attrs = append(attrs, semconv.ServiceName(*cfg.ServiceName))
	}

	if username, ok := HasBasicAuth(c.Get(fiber.HeaderAuthorization)); ok {
		attrs = append(attrs, semconv.EnduserIDKey.String(utils.CopyString(username)))
	}
	if cfg.collectClientIP {
		clientIP := c.IP()
		if len(clientIP) > 0 {
			attrs = append(attrs, semconv.ClientAddress(utils.CopyString(clientIP)))
		}
	}

	if cfg.CustomAttributes != nil {
		attrs = append(attrs, cfg.CustomAttributes(c)...)
	}

	return attrs
}

func networkProtocolVersion(c *fiber.Ctx) attribute.KeyValue {
	if c.Request().Header.IsHTTP11() {
		return semconv.NetworkProtocolVersion("1.1")
	}

	return semconv.NetworkProtocolVersion("1.0")
}

func HasBasicAuth(auth string) (string, bool) {
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
