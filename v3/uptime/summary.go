package uptime

// summarizeService consumes normalized days in oldest-to-newest order.
// It reuses DayStatus slot and downtime semantics without consulting storage or time.
func summarizeService(days []DayStatus, historyTruncated bool) ServiceSummary {
	var summary ServiceSummary
	var upSlots, expectedSlots int64
	for _, day := range days {
		if !day.HasData || day.ExpectedSlots <= 0 {
			continue
		}
		upSlots += int64(day.UpSlots)
		expectedSlots += int64(day.ExpectedSlots)
		summary.EstimatedDowntimeSeconds += day.EstimatedDowntimeSeconds
		if day.UpSlots < day.ExpectedSlots {
			summary.AffectedDays++
		}
	}
	if expectedSlots == 0 {
		return summary
	}
	summary.HasData = true
	summary.AvailabilityRate = float64(upSlots) / float64(expectedSlots)

	for i := len(days) - 1; i >= 0; i-- {
		day := days[i]
		if !day.HasData || day.ExpectedSlots <= 0 {
			// Only skip unknown days at the newest edge (e.g. just after midnight).
			if summary.StableStreakDays == 0 {
				continue
			}
			break
		}
		if day.UpSlots != day.ExpectedSlots {
			break
		}
		summary.StableStreakDays++
		if i == 0 {
			summary.StableStreakCapped = historyTruncated
		}
	}
	return summary
}
