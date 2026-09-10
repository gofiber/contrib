package uptime

import (
	"math"
	"slices"
	"testing"
)

func TestSummarizeService(t *testing.T) {
	t.Parallel()

	perfect := DayStatus{HasData: true, UpSlots: 100, ExpectedSlots: 100, Finalized: true}
	partial := DayStatus{HasData: true, UpSlots: 10, ExpectedSlots: 10}
	missed := DayStatus{HasData: true, UpSlots: 99, ExpectedSlots: 100, EstimatedDowntimeSeconds: 3, UptimeRate: 1}
	outage := DayStatus{HasData: true, ExpectedSlots: 100, EstimatedDowntimeSeconds: 300}
	unknown := DayStatus{UpSlots: 100, ExpectedSlots: 100, EstimatedDowntimeSeconds: 999}
	zeroExpected := DayStatus{HasData: true, EstimatedDowntimeSeconds: 999}
	tests := []struct {
		name      string
		days      []DayStatus
		truncated bool
		want      ServiceSummary
	}{
		{name: "empty"},
		{name: "all no data", days: []DayStatus{unknown, zeroExpected}, truncated: true},
		{name: "zero expected excluded", days: []DayStatus{zeroExpected}},
		{name: "all perfect", days: []DayStatus{perfect, perfect}, want: ServiceSummary{HasData: true, AvailabilityRate: 1, StableStreakDays: 2}},
		{name: "weighted unequal slots", days: []DayStatus{outage, partial}, want: ServiceSummary{HasData: true, AvailabilityRate: 10.0 / 110, EstimatedDowntimeSeconds: 300, AffectedDays: 1, StableStreakDays: 1}},
		{name: "partial creation day weighting", days: []DayStatus{partial, missed}, want: ServiceSummary{HasData: true, AvailabilityRate: 109.0 / 110, EstimatedDowntimeSeconds: 3, AffectedDays: 1}},
		{name: "no data excluded from aggregates", days: []DayStatus{unknown, zeroExpected, missed}, want: ServiceSummary{HasData: true, AvailabilityRate: .99, EstimatedDowntimeSeconds: 3, AffectedDays: 1}},
		{name: "downtime summed", days: []DayStatus{missed, outage}, want: ServiceSummary{HasData: true, AvailabilityRate: .495, EstimatedDowntimeSeconds: 303, AffectedDays: 2}},
		{name: "total outage is data", days: []DayStatus{outage}, want: ServiceSummary{HasData: true, EstimatedDowntimeSeconds: 300, AffectedDays: 1}},
		{name: "one missed slot ignores rounded rate", days: []DayStatus{missed}, want: ServiceSummary{HasData: true, AvailabilityRate: .99, EstimatedDowntimeSeconds: 3, AffectedDays: 1}},
		{name: "today perfect so far", days: []DayStatus{perfect, partial}, want: ServiceSummary{HasData: true, AvailabilityRate: 1, StableStreakDays: 2}},
		{name: "latest imperfect stops streak", days: []DayStatus{perfect, missed}, truncated: true, want: ServiceSummary{HasData: true, AvailabilityRate: .995, EstimatedDowntimeSeconds: 3, AffectedDays: 1}},
		{name: "older imperfect stops streak", days: []DayStatus{perfect, missed, perfect, partial}, truncated: true, want: ServiceSummary{HasData: true, AvailabilityRate: 309.0 / 310, EstimatedDowntimeSeconds: 3, AffectedDays: 1, StableStreakDays: 2}},
		{name: "skip newest no data", days: []DayStatus{perfect, partial, unknown, zeroExpected}, want: ServiceSummary{HasData: true, AvailabilityRate: 1, StableStreakDays: 2}},
		{name: "skip newest no data before imperfect", days: []DayStatus{perfect, missed, unknown}, truncated: true, want: ServiceSummary{HasData: true, AvailabilityRate: .995, EstimatedDowntimeSeconds: 3, AffectedDays: 1}},
		{name: "internal no data breaks streak", days: []DayStatus{perfect, unknown, partial}, truncated: true, want: ServiceSummary{HasData: true, AvailabilityRate: 1, StableStreakDays: 1}},
		{name: "internal zero expected breaks streak", days: []DayStatus{perfect, zeroExpected, partial}, truncated: true, want: ServiceSummary{HasData: true, AvailabilityRate: 1, StableStreakDays: 1}},
		{name: "history boundary capped", days: []DayStatus{perfect, partial}, truncated: true, want: ServiceSummary{HasData: true, AvailabilityRate: 1, StableStreakDays: 2, StableStreakCapped: true}},
		{name: "boundary capped after newest no data", days: []DayStatus{perfect, unknown}, truncated: true, want: ServiceSummary{HasData: true, AvailabilityRate: 1, StableStreakDays: 1, StableStreakCapped: true}},
		{name: "created within window exact", days: []DayStatus{unknown, perfect, partial}, want: ServiceSummary{HasData: true, AvailabilityRate: 1, StableStreakDays: 2}},
		{name: "created on left edge exact", days: []DayStatus{perfect}, want: ServiceSummary{HasData: true, AvailabilityRate: 1, StableStreakDays: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			before := slices.Clone(tt.days)
			got := summarizeService(tt.days, tt.truncated)
			if math.Abs(got.AvailabilityRate-tt.want.AvailabilityRate) > 1e-12 {
				t.Fatalf("availability = %v, want %v", got.AvailabilityRate, tt.want.AvailabilityRate)
			}
			got.AvailabilityRate = tt.want.AvailabilityRate
			requireEqual(t, tt.want, got)
			if !slices.Equal(before, tt.days) {
				t.Fatal("summary mutated daily history")
			}
		})
	}
}
