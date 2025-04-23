package internal

import (
	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
	"net/http"
	"testing"
)

func TestIsCode4xxIsNotValid(t *testing.T) {
	response := isCode4xx(http.StatusOK)

	assert.False(t, response)
}

func TestIsCode4xxIsValid(t *testing.T) {
	response := isCode4xx(http.StatusNotFound)

	assert.True(t, response)
}

func TestStatusErrorWithMessage(t *testing.T) {
	spanStatus, spanMessage := SpanStatusFromHTTPStatusCodeAndSpanKind(600, oteltrace.SpanKindClient)

	assert.Equal(t, codes.Error, spanStatus)
	assert.Equal(t, "Invalid HTTP status code 600", spanMessage)
}

func TestStatusErrorWithMessageForIgnoredHTTPCode(t *testing.T) {
	spanStatus, spanMessage := SpanStatusFromHTTPStatusCodeAndSpanKind(306, oteltrace.SpanKindClient)

	assert.Equal(t, codes.Error, spanStatus)
	assert.Equal(t, "Invalid HTTP status code 306", spanMessage)
}

func TestStatusErrorWhenHTTPCode5xx(t *testing.T) {
	spanStatus, spanMessage := SpanStatusFromHTTPStatusCodeAndSpanKind(http.StatusInternalServerError, oteltrace.SpanKindServer)

	assert.Equal(t, codes.Error, spanStatus)
	assert.Equal(t, "", spanMessage)
}

func TestStatusUnsetWhenServerSpanAndBadRequest(t *testing.T) {
	spanStatus, spanMessage := SpanStatusFromHTTPStatusCodeAndSpanKind(http.StatusBadRequest, oteltrace.SpanKindServer)

	assert.Equal(t, codes.Unset, spanStatus)
	assert.Equal(t, "", spanMessage)
}

func TestStatusUnset(t *testing.T) {
	spanStatus, spanMessage := SpanStatusFromHTTPStatusCodeAndSpanKind(http.StatusOK, oteltrace.SpanKindClient)

	assert.Equal(t, codes.Unset, spanStatus)
	assert.Equal(t, "", spanMessage)
}
