package internal

import (
	"fmt"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"net/http"
)

// SpanStatusFromHTTPStatusCodeAndSpanKind generates a status code and a message
// as specified by the OpenTelemetry specification for a span.
// Exclude 4xx for SERVER to set the appropriate status.
func SpanStatusFromHTTPStatusCodeAndSpanKind(code int, spanKind trace.SpanKind) (codes.Code, string) {
	// This code block ignores the HTTP 306 status code. The 306 status code is no longer in use.
	if len(http.StatusText(code)) == 0 {
		return codes.Error, fmt.Sprintf("Invalid HTTP status code %d", code)
	}

	if (code >= http.StatusContinue && code < http.StatusBadRequest) ||
		(spanKind == trace.SpanKindServer && isCode4xx(code)) {
		return codes.Unset, ""
	}
	return codes.Error, ""
}

func isCode4xx(code int) bool {
	return code >= http.StatusBadRequest && code <= http.StatusUnavailableForLegalReasons
}
