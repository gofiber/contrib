package uptime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/gofiber/contrib/v3/uptime/internal/storage"
)

// Snapshot is the current uptime status payload used by the JSON API.
type Snapshot = StatusResponse

// StatusResponse is the top-level JSON payload returned by the status API.
type StatusResponse struct {
	// GeneratedAt is the time when this response was built.
	GeneratedAt time.Time `json:"generated_at"`
	// SampleIntervalSeconds is the heartbeat interval used to compute uptime slots.
	SampleIntervalSeconds int64 `json:"sample_interval_seconds"`
	// Days is the number of local days included in each service history.
	Days int `json:"days"`
	// Storage reports the current backing store health.
	Storage StorageResponse `json:"storage"`
	// Services contains one entry per tracked service.
	Services []ServiceStatus `json:"services"`
}

// StorageResponse reports the current uptime store health.
type StorageResponse struct {
	// Driver is the storage backend name.
	Driver string `json:"driver"`
	// Status is "ok" when the store is currently healthy.
	Status string `json:"status"`
	// LastError is the latest runtime storage error, if any.
	LastError string `json:"last_error,omitempty"`
	// LastErrorAt is when LastError was recorded.
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
}

// ServiceStatus is the JSON status for one logical service.
type ServiceStatus struct {
	// ID is the stable service identifier.
	ID string `json:"id"`
	// Name is the display name shown in the dashboard.
	Name string `json:"name"`
	// Description is optional service detail text.
	Description string `json:"description,omitempty"`
	// LastSeenAt is the latest heartbeat time recorded for the service.
	LastSeenAt time.Time `json:"last_seen_at"`
	// CurrentStatus is "up" or "down" based on the latest heartbeat freshness.
	CurrentStatus string `json:"current_status"`
	// SampleIntervalSeconds is this service's heartbeat interval.
	SampleIntervalSeconds int64 `json:"sample_interval_seconds"`
	// Daily contains per-day uptime history.
	Daily []DayStatus `json:"daily"`
}

// DayStatus is the service uptime summary for one local day.
type DayStatus struct {
	// Day is the local date in YYYY-MM-DD format.
	Day string `json:"day"`
	// UptimeRate is UpSlots divided by ExpectedSlots.
	UptimeRate float64 `json:"uptime_rate"`
	// UpSlots is the number of heartbeat slots with at least one sample.
	UpSlots int `json:"up_slots"`
	// ExpectedSlots is the number of slots expected for the day.
	ExpectedSlots int `json:"expected_slots"`
	// EstimatedDowntimeSeconds is the missing-slot downtime estimate.
	EstimatedDowntimeSeconds int64 `json:"estimated_downtime_seconds"`
	// Finalized reports whether the day has been rolled up.
	Finalized bool `json:"finalized"`
	// HasData reports whether the day has enough data to calculate uptime.
	HasData bool `json:"has_data"`
	// Status is "green", "yellow", "red", or "gray" for dashboard rendering.
	Status string `json:"status"`
}

const (
	statusUp   = "up"
	statusDown = "down"
)

func (u *runtime) snapshot(ctx context.Context) (Snapshot, error) {
	if u == nil {
		return Snapshot{}, errors.New("uptime: nil runtime")
	}
	return u.buildStatus(ctx, time.Now())
}

func (u *runtime) buildStatus(ctx context.Context, now time.Time) (StatusResponse, error) {
	services, err := u.store.ListServices(ctx)
	if err != nil {
		u.setLastError(err)
		return StatusResponse{}, err
	}

	days := dayRange(now, u.config.DaysToShow, u.config.Timezone)
	fromDay := days[0]
	toDay := days[len(days)-1]
	today := dayOf(now, u.config.Timezone)

	dailyRows, err := u.store.QueryDaily(ctx, storage.QueryDailyOptions{FromDay: fromDay, ToDay: toDay})
	if err != nil {
		u.setLastError(err)
		return StatusResponse{}, err
	}
	todayRows, err := u.store.QueryTodaySamples(ctx, storage.QueryTodaySamplesOptions{Day: today})
	if err != nil {
		u.setLastError(err)
		return StatusResponse{}, err
	}

	dailyByService := make(map[string]map[string]storage.DailyStatus)
	for _, row := range dailyRows {
		byDay := dailyByService[row.ServiceID]
		if byDay == nil {
			byDay = make(map[string]storage.DailyStatus)
			dailyByService[row.ServiceID] = byDay
		}
		byDay[row.Day] = row
	}
	todayByService := make(map[string]storage.TodaySampleStatus)
	for _, row := range todayRows {
		todayByService[row.ServiceID] = row
	}

	resp := StatusResponse{
		GeneratedAt:           now.UTC(),
		SampleIntervalSeconds: int64(u.config.SampleInterval / time.Second),
		Days:                  u.config.DaysToShow,
		Storage:               u.storageStatus(),
		Services:              make([]ServiceStatus, 0, len(services)),
	}

	for _, service := range services {
		interval := u.serviceSampleInterval(service)
		serviceStatus := ServiceStatus{
			ID:                    service.ID,
			Name:                  service.Name,
			Description:           service.Description,
			LastSeenAt:            service.LastSeenAt.UTC(),
			CurrentStatus:         currentStatus(now, service.LastSeenAt, interval),
			SampleIntervalSeconds: int64(interval / time.Second),
			Daily:                 make([]DayStatus, 0, len(days)),
		}
		createdDay := dayOf(service.CreatedAt, u.config.Timezone)
		for _, day := range days {
			serviceStatus.Daily = append(serviceStatus.Daily, u.dayStatus(service.ID, day, today, createdDay, service.CreatedAt, now, interval, dailyByService, todayByService))
		}
		resp.Services = append(resp.Services, serviceStatus)
	}

	return resp, nil
}

func (u *runtime) dayStatus(serviceID, day, today, createdDay string, createdAt, now time.Time, interval time.Duration, daily map[string]map[string]storage.DailyStatus, todayRows map[string]storage.TodaySampleStatus) DayStatus {
	if day < createdDay {
		return DayStatus{
			Day:     day,
			HasData: false,
			Status:  "gray",
		}
	}

	if day == today {
		row := todayRows[serviceID]
		expected := expectedSlotsSoFarSince(now, createdAt, interval, u.config.Timezone)
		return makeDayStatus(day, row.UpSlots, expected, false, true, interval, u.config.UI)
	}

	if byDay := daily[serviceID]; byDay != nil {
		if row, ok := byDay[day]; ok {
			return makeDayStatus(day, row.UpSlots, row.ExpectedSlots, row.Finalized, true, interval, u.config.UI)
		}
	}

	expected := expectedSlotsForServiceDay(day, createdAt, interval, u.config.Timezone)
	return makeDayStatus(day, 0, expected, true, true, interval, u.config.UI)
}

func (u *runtime) serviceSampleInterval(service storage.Service) time.Duration {
	if service.SampleInterval >= time.Second {
		return service.SampleInterval
	}
	return u.config.SampleInterval
}

func makeDayStatus(day string, upSlots, expectedSlots int, finalized, hasData bool, interval time.Duration, ui UIConfig) DayStatus {
	rate := uptimeRate(upSlots, expectedSlots)
	downSlots := expectedSlots - upSlots
	if downSlots < 0 {
		downSlots = 0
	}
	return DayStatus{
		Day:                      day,
		UptimeRate:               rate,
		UpSlots:                  upSlots,
		ExpectedSlots:            expectedSlots,
		EstimatedDowntimeSeconds: int64(time.Duration(downSlots) * interval / time.Second),
		Finalized:                finalized,
		HasData:                  hasData,
		Status:                   colorFor(rate, hasData, ui),
	}
}

func uptimeRate(upSlots, expectedSlots int) float64 {
	if expectedSlots <= 0 {
		return 0
	}
	rate := float64(upSlots) / float64(expectedSlots)
	if rate > 1 {
		return 1
	}
	if rate < 0 || math.IsNaN(rate) {
		return 0
	}
	return rate
}

func colorFor(rate float64, hasData bool, ui UIConfig) string {
	if !hasData {
		return "gray"
	}
	if rate >= ui.GreenThreshold {
		return "green"
	}
	if rate >= ui.YellowThreshold {
		return "yellow"
	}
	return "red"
}

func currentStatus(now, lastSeen time.Time, interval time.Duration) string {
	if lastSeen.IsZero() {
		return statusDown
	}
	if now.Sub(lastSeen) <= interval*2 {
		return statusUp
	}
	return statusDown
}

func dayRange(now time.Time, count int, loc *time.Location) []string {
	if count < 1 {
		count = 1
	}
	today := dayOf(now, loc)
	days := make([]string, count)
	for i := 0; i < count; i++ {
		days[count-1-i] = addDays(today, -i, loc)
	}
	return days
}

func (u *runtime) storageStatus() StorageResponse {
	at, err := u.lastError()
	storage := StorageResponse{
		Driver: storeDriver(u.store),
		Status: "ok",
	}
	if err != nil {
		storage.Status = "degraded"
		storage.LastError = err.Error()
		storage.LastErrorAt = &at
	}
	return storage
}

func storeDriver(store storage.Store) string {
	type named interface {
		Name() string
	}
	if namedStore, ok := store.(named); ok {
		return namedStore.Name()
	}
	return fmt.Sprintf("%T", store)
}

func dayOf(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("2006-01-02")
}

func parseDay(day string, loc *time.Location) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", day, loc)
}

func startOfDay(t time.Time, loc *time.Location) time.Time {
	local := t.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
}

func slotOf(t time.Time, interval time.Duration, loc *time.Location) int64 {
	if interval <= 0 {
		return 0
	}
	elapsed := t.In(loc).Sub(startOfDay(t, loc))
	if elapsed < 0 {
		return 0
	}
	return int64(elapsed / interval)
}

func expectedSlotsSoFarSince(now, createdAt time.Time, interval time.Duration, loc *time.Location) int {
	return expectedSlotsForWindow(dayOf(now, loc), createdAt, now, interval, loc, true)
}

func expectedSlotsForDay(day string, interval time.Duration, loc *time.Location) int {
	start, err := parseDay(day, loc)
	if err != nil || interval <= 0 {
		return 0
	}
	end := start.AddDate(0, 0, 1)
	return ceilDuration(end.Sub(start), interval)
}

func expectedSlotsForServiceDay(day string, createdAt time.Time, interval time.Duration, loc *time.Location) int {
	return expectedSlotsForWindow(day, createdAt, time.Time{}, interval, loc, false)
}

func expectedSlotsForWindow(day string, createdAt, endAt time.Time, interval time.Duration, loc *time.Location, includeEnd bool) int {
	dayStart, err := parseDay(day, loc)
	if err != nil || interval <= 0 {
		return 0
	}
	dayEnd := dayStart.AddDate(0, 0, 1)
	slotsInDay := ceilDuration(dayEnd.Sub(dayStart), interval)
	if slotsInDay <= 0 {
		return 0
	}

	start := dayStart
	if !createdAt.IsZero() && createdAt.After(start) {
		start = createdAt
	}
	if !start.Before(dayEnd) {
		return 0
	}

	end := dayEnd
	if !endAt.IsZero() && endAt.Before(end) {
		end = endAt
	}
	if includeEnd {
		if end.Before(start) {
			return 0
		}
	} else if !end.After(start) {
		return 0
	}

	firstSlot := int(slotOf(start, interval, loc))
	if firstSlot < 0 {
		firstSlot = 0
	}
	if firstSlot >= slotsInDay {
		return 0
	}

	lastSlot := slotsInDay - 1
	if end.Before(dayEnd) {
		lastSlot = int(slotOf(end, interval, loc))
		if !includeEnd && isSlotBoundary(end, interval, loc) {
			lastSlot--
		}
	}
	if lastSlot >= slotsInDay {
		lastSlot = slotsInDay - 1
	}
	if lastSlot < firstSlot {
		return 0
	}
	return lastSlot - firstSlot + 1
}

func isSlotBoundary(t time.Time, interval time.Duration, loc *time.Location) bool {
	if interval <= 0 {
		return false
	}
	elapsed := t.In(loc).Sub(startOfDay(t, loc))
	return elapsed%interval == 0
}

func ceilDuration(duration, interval time.Duration) int {
	if duration <= 0 || interval <= 0 {
		return 0
	}
	return int((duration + interval - 1) / interval)
}

func addDays(day string, days int, loc *time.Location) string {
	start, err := parseDay(day, loc)
	if err != nil {
		return day
	}
	return start.AddDate(0, 0, days).Format("2006-01-02")
}
