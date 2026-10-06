package otel

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"go.opentelemetry.io/otel/propagation"
)

func newRequestHeader(t *testing.T, normalize bool, headers ...[2]string) *fasthttp.RequestHeader {
	t.Helper()

	header := &fasthttp.RequestHeader{}
	if !normalize {
		header.DisableNormalizing()
	}
	for _, kv := range headers {
		header.Add(kv[0], kv[1])
	}

	return header
}

func TestRequestCarrier(t *testing.T) {
	t.Parallel()

	header := newRequestHeader(t, true,
		[2]string{"traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		[2]string{"Baggage", "a=1"},
		[2]string{"baggage", "b=2"},
	)
	carrier := (*requestCarrier)(header)

	// Names are normalized on both sides, so any spelling finds the header.
	require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", carrier.Get("traceparent"))
	require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", carrier.Get("TraceParent"))
	require.Empty(t, carrier.Get("tracestate"))
	require.Equal(t, []string{"a=1", "b=2"}, carrier.Values("baggage"))
	require.Nil(t, carrier.Values("tracestate"))
	require.ElementsMatch(t, []string{"Traceparent", "Baggage", "Baggage"}, carrier.Keys())

	carrier.Set("traceparent", "overwritten")
	require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", carrier.Get("traceparent"))
}

// Propagators keep values, so they must not alias fasthttp's buffers.
func TestRequestCarrierCopiesValues(t *testing.T) {
	t.Parallel()

	header := newRequestHeader(t, true, [2]string{"tracestate", "vendor=value"})
	carrier := (*requestCarrier)(header)

	value := carrier.Get("tracestate")
	values := carrier.Values("tracestate")
	header.Set("tracestate", "XXXXXX=XXXXX")

	require.Equal(t, "vendor=value", value)
	require.Equal(t, []string{"vendor=value"}, values)
}

// With DisableHeaderNormalizing, names must still match case-insensitively.
func TestFoldingRequestCarrier(t *testing.T) {
	t.Parallel()

	header := newRequestHeader(t, false,
		[2]string{"Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		[2]string{"BAGGAGE", "a=1"},
		[2]string{"baggage", "b=2"},
	)
	require.Empty(t, header.Peek("traceparent"), "the exact match the carrier falls back from")

	carrier := (*foldingRequestCarrier)(header)
	require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", carrier.Get("traceparent"))
	require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", carrier.Get("Traceparent"))
	require.Empty(t, carrier.Get("tracestate"))
	require.Equal(t, []string{"a=1", "b=2"}, carrier.Values("baggage"))
	require.Nil(t, carrier.Values("tracestate"))
	require.ElementsMatch(t, []string{"Traceparent", "BAGGAGE", "baggage"}, carrier.Keys())

	carrier.Set("traceparent", "overwritten")
	require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", carrier.Get("traceparent"))
}

func TestResponseCarrier(t *testing.T) {
	t.Parallel()

	header := &fasthttp.ResponseHeader{}
	header.Set("Traceparent", "stale")
	carrier := (*responseCarrier)(header)

	propagation.TraceContext{}.Inject(t.Context(), carrier)
	require.Equal(t, "stale", carrier.Get("traceparent"), "an invalid span context injects nothing")

	carrier.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	carrier.Set("tracestate", "vendor=value")
	require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", string(header.Peek("Traceparent")))
	require.Equal(t, "vendor=value", carrier.Get("Tracestate"))
	require.Subset(t, carrier.Keys(), []string{"Traceparent", "Tracestate"})
}
