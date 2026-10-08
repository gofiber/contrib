package internal

import (
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// SpanStatusFromHTTPStatusCodeAndSpanKind generates a status code and a message
// as specified by the OpenTelemetry specification for a span.
// Any code in [100, 600) is valid; 4xx is not an error on SERVER spans.
func SpanStatusFromHTTPStatusCodeAndSpanKind(code int, spanKind trace.SpanKind) (codes.Code, string) {
	if code < http.StatusContinue || code >= 600 {
		return codes.Error, fmt.Sprintf("Invalid HTTP status code %d", code)
	}

	if code >= http.StatusInternalServerError || (code >= http.StatusBadRequest && spanKind != trace.SpanKindServer) {
		return codes.Error, ""
	}

	return codes.Unset, ""
}
