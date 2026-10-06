package internal

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestSpanStatusFromHTTPStatusCodeAndSpanKind(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		code    int
		kind    oteltrace.SpanKind
		status  codes.Code
		message string
	}{
		{name: "below range", code: 99, kind: oteltrace.SpanKindServer, status: codes.Error, message: "Invalid HTTP status code 99"},
		{name: "zero", code: 0, kind: oteltrace.SpanKindServer, status: codes.Error, message: "Invalid HTTP status code 0"},
		{name: "above range", code: 600, kind: oteltrace.SpanKindClient, status: codes.Error, message: "Invalid HTTP status code 600"},
		{name: "informational", code: http.StatusSwitchingProtocols, kind: oteltrace.SpanKindServer, status: codes.Unset},
		{name: "ok", code: http.StatusOK, kind: oteltrace.SpanKindClient, status: codes.Unset},
		// Valid codes without a registered reason phrase are statuses all the same.
		{name: "unnamed 2xx", code: 299, kind: oteltrace.SpanKindServer, status: codes.Unset},
		{name: "unused 306", code: 306, kind: oteltrace.SpanKindClient, status: codes.Unset},
		{name: "server 4xx", code: http.StatusBadRequest, kind: oteltrace.SpanKindServer, status: codes.Unset},
		{name: "server unnamed 4xx", code: 499, kind: oteltrace.SpanKindServer, status: codes.Unset},
		{name: "client 4xx", code: http.StatusNotFound, kind: oteltrace.SpanKindClient, status: codes.Error},
		{name: "server 5xx", code: http.StatusInternalServerError, kind: oteltrace.SpanKindServer, status: codes.Error},
		{name: "server unnamed 5xx", code: 520, kind: oteltrace.SpanKindServer, status: codes.Error},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			status, message := SpanStatusFromHTTPStatusCodeAndSpanKind(tc.code, tc.kind)
			assert.Equal(t, tc.status, status)
			assert.Equal(t, tc.message, message)
		})
	}
}
