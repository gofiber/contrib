package uptime

import (
	"testing"
	"time"
)

func TestExpectedSlotsForWindow(t *testing.T) {
	t.Parallel()

	loc := time.UTC
	day := "2026-06-26"
	at := func(hour int) time.Time {
		return time.Date(2026, 6, 26, hour, 0, 0, 0, loc)
	}

	tests := []struct {
		name       string
		createdAt  time.Time
		endAt      time.Time
		includeEnd bool
		want       int
	}{
		{name: "full service day", includeEnd: false, want: 24},
		{name: "created mid-day", createdAt: at(6), includeEnd: false, want: 18},
		{name: "created before day", createdAt: time.Date(2026, 6, 25, 0, 0, 0, 0, loc), includeEnd: false, want: 24},
		{name: "created after day", createdAt: time.Date(2026, 6, 27, 0, 0, 0, 0, loc), includeEnd: false, want: 0},
		{name: "so far inclusive of current slot", endAt: at(6), includeEnd: true, want: 7},
		{name: "end on slot boundary exclusive", endAt: at(6), includeEnd: false, want: 6},
		{name: "end before created", createdAt: at(10), endAt: at(6), includeEnd: true, want: 0},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := expectedSlotsForWindow(day, tt.createdAt, tt.endAt, time.Hour, loc, tt.includeEnd)
			requireEqual(t, tt.want, got)
		})
	}
}

func TestExpectedSlotsForDayCountsWholeDay(t *testing.T) {
	t.Parallel()

	requireEqual(t, 24, expectedSlotsForDay("2026-06-26", time.Hour, time.UTC))
	requireEqual(t, 1440, expectedSlotsForDay("2026-06-26", time.Minute, time.UTC))
	requireEqual(t, 0, expectedSlotsForDay("not-a-day", time.Minute, time.UTC))
}
