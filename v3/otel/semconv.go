package otel

import (
	"encoding/base64"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

var (
	httpProtocolNameAttr = semconv.NetworkProtocolName("http")
	http11VersionAttr    = semconv.NetworkProtocolVersion("1.1")
	http10VersionAttr    = semconv.NetworkProtocolVersion("1.0")
	enduserIDKey         = attribute.Key("enduser.id")
)

func httpServerMetricAttributesFromRequest(c fiber.Ctx, cfg config) []attribute.KeyValue {
	protocolAttributes := httpNetworkProtocolAttributes(c)
	attrs := []attribute.KeyValue{
		semconv.URLScheme(requestScheme(c)),
		semconv.ServerAddress(utils.CopyString(c.Hostname())),
		semconv.HTTPRequestMethodKey.String(utils.CopyString(c.Method())),
	}
	attrs = append(attrs, protocolAttributes...)

	if cfg.Port != nil {
		attrs = append(attrs, semconv.ServerPort(*cfg.Port))
	}

	if cfg.CustomMetricAttributes != nil {
		attrs = append(attrs, cfg.CustomMetricAttributes(c)...)
	}

	return attrs
}

func httpServerTraceAttributesFromRequest(c fiber.Ctx, cfg config) []attribute.KeyValue {
	protocolAttributes := httpNetworkProtocolAttributes(c)
	attrs := []attribute.KeyValue{
		// utils.CopyString: we need to copy the string as fasthttp strings are by default
		// mutable so it will be unsafe to use in this middleware as it might be used after
		// the handler returns.
		semconv.HTTPRequestMethodKey.String(utils.CopyString(c.Method())),
		semconv.URLScheme(requestScheme(c)),
		semconv.HTTPRequestBodySize(c.Request().Header.ContentLength()),
		semconv.URLPath(string(utils.CopyBytes(c.Request().URI().Path()))),
		semconv.URLQuery(c.Request().URI().QueryArgs().String()),
		semconv.URLFull(utils.CopyString(c.OriginalURL())),
		semconv.UserAgentOriginal(string(utils.CopyBytes(c.Request().Header.UserAgent()))),
		semconv.ServerAddress(utils.CopyString(c.Hostname())),
		semconv.NetworkTransportTCP,
	}
	attrs = append(attrs, protocolAttributes...)

	if cfg.Port != nil {
		attrs = append(attrs, semconv.ServerPort(*cfg.Port))
	}

	if username, ok := HasBasicAuth(c.Get(fiber.HeaderAuthorization)); ok {
		attrs = append(attrs, enduserIDKey.String(utils.CopyString(username)))
	}

	if cfg.clientIP {
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

func httpNetworkProtocolAttributes(c fiber.Ctx) []attribute.KeyValue {
	httpProtocolAttributes := []attribute.KeyValue{httpProtocolNameAttr}
	if c.Request().Header.IsHTTP11() {
		return append(httpProtocolAttributes, http11VersionAttr)
	}
	return append(httpProtocolAttributes, http10VersionAttr)
}

func requestScheme(c fiber.Ctx) string {
	scheme := c.Request().URI().Scheme()
	if len(scheme) == 0 {
		return "http"
	}

	return utils.CopyString(string(scheme))
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
