package otel

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAppendRedactedQuery(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		query string
		extra []string
		want  string
	}{
		{name: "plain", query: "q=otel&page=2", want: "q=otel&page=2"},
		{name: "single sensitive", query: "sig=abc", want: "sig=REDACTED"},
		{
			name:  "pre-signed S3",
			query: "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIA%2F20260101&X-Amz-Signature=deadbeef&X-Amz-Security-Token=tok",
			want:  "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=REDACTED&X-Amz-Signature=REDACTED&X-Amz-Security-Token=REDACTED",
		},
		{name: "legacy S3", query: "AWSAccessKeyId=AKIA&Signature=c2ln&Expires=1", want: "AWSAccessKeyId=REDACTED&Signature=REDACTED&Expires=1"},
		{name: "GCS", query: "X-Goog-Signature=abc&alt=media", want: "X-Goog-Signature=REDACTED&alt=media"},
		{name: "repeated key", query: "sig=a&sig=b", want: "sig=REDACTED&sig=REDACTED"},
		// Matching is case-sensitive, as the semantic conventions specify.
		{name: "other case", query: "SIG=abc&signature=def", want: "SIG=abc&signature=def"},
		{name: "empty value kept", query: "sig=&q=1", want: "sig=&q=1"},
		{name: "key without value", query: "sig&q=1", want: "sig&q=1"},
		{name: "value holding equals", query: "sig=a=b&q=1", want: "sig=REDACTED&q=1"},
		{name: "empty pairs kept", query: "&&sig=a&", want: "&&sig=REDACTED&"},
		{name: "key as value", query: "q=sig", want: "q=sig"},
		{name: "encoded name", query: "s%69g=abc&q=1", want: "s%69g=REDACTED&q=1"},
		{name: "invalid escape kept", query: "s%zzg=abc", want: "s%zzg=abc"},
		{name: "extra name", query: "token=abc&q=1", extra: []string{"token"}, want: "token=REDACTED&q=1"},
		{name: "encoded extra name", query: "to%6Ben=abc", extra: []string{"token"}, want: "to%6Ben=REDACTED"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, string(appendRedactedQuery(nil, []byte(tc.query), tc.extra)))
			// Appending to a buffer keeps what was already there.
			require.Equal(t, "x"+tc.want, string(appendRedactedQuery([]byte("x"), []byte(tc.query), tc.extra)))
		})
	}
}
