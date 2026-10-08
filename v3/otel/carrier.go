package otel

import (
	"github.com/gofiber/utils/v2"
	"github.com/valyala/fasthttp"
	"go.opentelemetry.io/otel/propagation"
)

// requestCarrier reads propagation fields from fasthttp's headers without building an http.Header.
type requestCarrier fasthttp.RequestHeader

var (
	_ propagation.TextMapCarrier = (*requestCarrier)(nil)
	_ propagation.ValuesGetter   = (*requestCarrier)(nil)
)

// Get copies the value: propagators may keep it, and fasthttp reuses the buffer.
func (c *requestCarrier) Get(key string) string {
	return string((*fasthttp.RequestHeader)(c).Peek(key))
}

func (c *requestCarrier) Values(key string) []string {
	return copyValues((*fasthttp.RequestHeader)(c).PeekAll(key))
}

// Set is a no-op: a request carrier is only extracted from.
func (*requestCarrier) Set(string, string) {}

func (c *requestCarrier) Keys() []string {
	return headerKeys((*fasthttp.RequestHeader)(c))
}

// foldingRequestCarrier matches names case-insensitively, for apps with DisableHeaderNormalizing.
type foldingRequestCarrier fasthttp.RequestHeader

var (
	_ propagation.TextMapCarrier = (*foldingRequestCarrier)(nil)
	_ propagation.ValuesGetter   = (*foldingRequestCarrier)(nil)
)

func (c *foldingRequestCarrier) Get(key string) string {
	header := (*fasthttp.RequestHeader)(c)
	if value := header.Peek(key); len(value) > 0 {
		return string(value)
	}

	for name, value := range header.All() {
		if utils.EqualFold(utils.UnsafeString(name), key) {
			return string(value)
		}
	}

	return ""
}

func (c *foldingRequestCarrier) Values(key string) []string {
	var values []string
	for name, value := range (*fasthttp.RequestHeader)(c).All() {
		if utils.EqualFold(utils.UnsafeString(name), key) {
			values = append(values, string(value))
		}
	}

	return values
}

func (*foldingRequestCarrier) Set(string, string) {}

func (c *foldingRequestCarrier) Keys() []string {
	return headerKeys((*fasthttp.RequestHeader)(c))
}

// responseCarrier writes propagation fields into fasthttp's response headers.
type responseCarrier fasthttp.ResponseHeader

var _ propagation.TextMapCarrier = (*responseCarrier)(nil)

func (c *responseCarrier) Get(key string) string {
	return string((*fasthttp.ResponseHeader)(c).Peek(key))
}

func (c *responseCarrier) Set(key, value string) {
	(*fasthttp.ResponseHeader)(c).Set(key, value)
}

func (c *responseCarrier) Keys() []string {
	header := (*fasthttp.ResponseHeader)(c)
	keys := make([]string, 0, header.Len())
	for name := range header.All() {
		keys = append(keys, string(name))
	}

	return keys
}

func headerKeys(header *fasthttp.RequestHeader) []string {
	keys := make([]string, 0, header.Len())
	for name := range header.All() {
		keys = append(keys, string(name))
	}

	return keys
}

// copyValues detaches header values from fasthttp's buffers.
func copyValues(raw [][]byte) []string {
	if len(raw) == 0 {
		return nil
	}

	values := make([]string, len(raw))
	for i, value := range raw {
		values[i] = string(value)
	}

	return values
}
