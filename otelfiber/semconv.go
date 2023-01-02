package otelfiber

import (
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


func httpFlavorAttribute(c *fiber.Ctx) attribute.KeyValue {
	if c.Request().Header.IsHTTP11() {
		return semconv.HTTPFlavorHTTP11
	}

	return semconv.HTTPFlavorHTTP10
}