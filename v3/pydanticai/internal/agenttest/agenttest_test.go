package agenttest

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// recorder is a testing.TB that keeps what Fatalf received, instead of ending
// the test.
type recorder struct {
	testing.TB
	message string
}

func (r *recorder) Helper() {}

func (r *recorder) Fatalf(format string, args ...any) { r.message = fmt.Sprintf(format, args...) }

func Test_Receive(t *testing.T) {
	t.Parallel()

	values := make(chan int, 1)
	values <- 7
	require.Equal(t, 7, Receive(t, values, "a value"))

	failing := new(recorder)
	require.Zero(t, receive(failing, make(chan int), "a value", time.Millisecond))
	require.Equal(t, "timed out waiting for a value", failing.message)
}

func Test_Deadline_Require(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		deadline Deadline
		want     time.Duration
		failure  string
	}{
		{name: "none wanted and none set"},
		{name: "wanted and set", deadline: Deadline{Left: 4*time.Minute + 59*time.Second, Set: true}, want: 5 * time.Minute},
		{
			name: "none wanted but set", deadline: Deadline{Left: time.Minute, Set: true},
			failure: "the context has a deadline in 1m0s, want none",
		},
		{
			name: "wanted but none set", want: time.Minute,
			failure: "the context has no deadline, want one in 1m0s",
		},
		{
			name: "further away than wanted", deadline: Deadline{Left: 6 * time.Minute, Set: true}, want: 5 * time.Minute,
			failure: "the deadline is in 6m0s, want about 5m0s",
		},
		{
			name: "nearer than wanted", deadline: Deadline{Left: time.Minute, Set: true}, want: 5 * time.Minute,
			failure: "the deadline is in 1m0s, want about 5m0s",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			failing := new(recorder)
			tc.deadline.Require(failing, tc.want)
			require.Equal(t, tc.failure, failing.message)
		})
	}
}
