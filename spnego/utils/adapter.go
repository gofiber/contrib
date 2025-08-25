package utils

import (
	"net/http"

	"github.com/valyala/fasthttp"
)

// FiberContext defines the minimal interface required from a Fiber context
// for the adapter to function properly.
// T represents the type of the Fiber context (v2 or v3 compatible).
type FiberContext[T any] interface {
	// Response returns the underlying fasthttp.Response
	Response() *fasthttp.Response
	// Write writes bytes to the response body
	Write(bytes []byte) (int, error)
	// Status sets the HTTP status code and returns the context
	Status(status int) T
}

// WrapFiberContextAdaptHttpResponseWriter adapts a Fiber context to the http.ResponseWriter interface
// This allows Fiber to work with libraries that expect the standard http.ResponseWriter
// T represents the type of the Fiber context (v2 or v3 compatible).
type WrapFiberContextAdaptHttpResponseWriter[T FiberContext[T]] struct {
	ctx T
}

// Header returns the response headers from the Fiber context
// in the standard http.Header format
// note: write header must using fiber context
func (f *WrapFiberContextAdaptHttpResponseWriter[T]) Header() http.Header {
	headers := http.Header{}
	for k, v := range f.ctx.Response().Header.All() {
		headers.Set(string(k), string(v))
	}
	return headers
}

// Write writes bytes to the response body using the Fiber context's Write method
func (f *WrapFiberContextAdaptHttpResponseWriter[T]) Write(bytes []byte) (int, error) {
	return f.ctx.Write(bytes)
}

// WriteHeader sets the HTTP status code using the Fiber context's Status method
func (f *WrapFiberContextAdaptHttpResponseWriter[T]) WriteHeader(statusCode int) {
	f.ctx.Status(statusCode)
}

// NewWrapFiberContext creates a new adapter instance that wraps a Fiber context
// to implement the http.ResponseWriter interface
// ctx: The Fiber context to wrap
// Returns: A new adapter instance
func NewWrapFiberContext[T FiberContext[T]](ctx T) *WrapFiberContextAdaptHttpResponseWriter[T] {
	return &WrapFiberContextAdaptHttpResponseWriter[T]{
		ctx: ctx,
	}
}
