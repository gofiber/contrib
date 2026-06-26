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

type StatusResponse struct {
	GeneratedAt           time.Time       `json:"generated_at"`
	SampleIntervalSeconds int64           `json:"sample_interval_seconds"`
	Days                  int             `json:"days"`
	Storage               StorageResponse `json:"storage"`
	Services              []ServiceStatus `json:"services"`
}

type StorageResponse struct {
	Driver      string     `json:"driver"`
	Status      string     `json:"status"`
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
}

type ServiceStatus struct {
	ID                    string      `json:"id"`
	Name                  string      `json:"name"`
	Description           string      `json:"description,omitempty"`
	LastSeenAt            time.Time   `json:"last_seen_at"`
	CurrentStatus         string      `json:"current_status"`
	SampleIntervalSeconds int64       `json:"sample_interval_seconds"`
	Daily                 []DayStatus `json:"daily"`
}

type DayStatus struct {
	Day                      string  `json:"day"`
	UptimeRate               float64 `json:"uptime_rate"`
	UpSlots                  int     `json:"up_slots"`
	ExpectedSlots            int     `json:"expected_slots"`
	EstimatedDowntimeSeconds int64   `json:"estimated_downtime_seconds"`
	Finalized                bool    `json:"finalized"`
	HasData                  bool    `json:"has_data"`
	Status                   string  `json:"status"`
}

const (
	statusUp   = "up"
	statusDown = "down"
)

// Snapshot queries the store and returns a fresh status snapshot.
func (u *Uptime) Snapshot(ctx context.Context) (Snapshot, error) {
	if u == nil {
		return Snapshot{}, errors.New("uptime: nil uptime")
	}
	return u.buildStatus(ctx, time.Now())
}

// CachedSnapshot returns a status snapshot through Uptime's in-memory cache.
func (u *Uptime) CachedSnapshot(ctx context.Context) (Snapshot, error) {
	if u == nil {
		return Snapshot{}, errors.New("uptime: nil uptime")
	}
	if u.config.Snapshot.DisableCache {
		return u.Snapshot(ctx)
	}

	now := time.Now()

	u.snapshotMu.Lock()
	defer u.snapshotMu.Unlock()

	if u.snapshotHasCache && now.Sub(u.snapshotCachedAt) < u.config.Snapshot.CacheTTL {
		return cloneSnapshot(u.snapshotCache), nil
	}

	snapshot, err := u.buildStatus(ctx, now)
	if err != nil {
		if u.snapshotHasCache && !u.config.Snapshot.DisableStaleIfError {
			stale := cloneSnapshot(u.snapshotCache)
			markSnapshotRefreshError(&stale, err, time.Now())
			return stale, nil
		}
		return Snapshot{}, err
	}

	u.snapshotCache = cloneSnapshot(snapshot)
	u.snapshotCachedAt = now
	u.snapshotHasCache = true

	return cloneSnapshot(snapshot), nil
}

func (u *Uptime) buildStatus(ctx context.Context, now time.Time) (StatusResponse, error) {
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

	u.clearLastError()
	return resp, nil
}

func (u *Uptime) dayStatus(serviceID, day, today, createdDay string, createdAt, now time.Time, interval time.Duration, daily map[string]map[string]storage.DailyStatus, todayRows map[string]storage.TodaySampleStatus) DayStatus {
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

func (u *Uptime) serviceSampleInterval(service storage.Service) time.Duration {
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

func cloneSnapshot(in Snapshot) Snapshot {
	out := in
	if in.Storage.LastErrorAt != nil {
		lastErrorAt := *in.Storage.LastErrorAt
		out.Storage.LastErrorAt = &lastErrorAt
	}
	out.Services = append([]ServiceStatus(nil), in.Services...)
	for i := range out.Services {
		out.Services[i].Daily = append([]DayStatus(nil), in.Services[i].Daily...)
	}
	return out
}

func markSnapshotRefreshError(snapshot *Snapshot, err error, at time.Time) {
	if snapshot == nil || err == nil {
		return
	}
	snapshot.Storage.Status = "degraded"
	snapshot.Storage.LastError = err.Error()
	errorAt := at.UTC()
	snapshot.Storage.LastErrorAt = &errorAt
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

func (u *Uptime) storageStatus() StorageResponse {
	err, at := u.LastError()
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

func expectedSlotsSoFar(now time.Time, interval time.Duration, loc *time.Location) int {
	return expectedSlotsSoFarSince(now, time.Time{}, interval, loc)
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
