package uptime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/gofiber/contrib/v3/uptime/internal/storage"
	fiberlog "github.com/gofiber/fiber/v3/log"
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
	// LastError is a non-sensitive indicator of the latest runtime storage error.
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
	statusUp                = "up"
	statusDown              = "down"
	storageErrorPublicLabel = "storage operation failed"
)

// cachedSnapshot limits status requests to one store refresh per cache
// lifetime. The previous value remains available when a refresh fails,
// preventing a temporary store outage from taking down the public status
// endpoints.
func (u *runtime) cachedSnapshot(ctx context.Context) (Snapshot, error) {
	if u == nil {
		return Snapshot{}, errors.New("uptime: nil runtime")
	}

	now := time.Now()
	u.snapshotMu.Lock()
	defer u.snapshotMu.Unlock()

	if u.snapshotHasCache && now.Sub(u.snapshotCachedAt) < u.snapshotTTL {
		return cloneSnapshot(u.snapshotCache), nil
	}

	snapshot, err := u.buildStatus(ctx, now)
	if err != nil {
		if !u.snapshotHasCache {
			return Snapshot{}, err
		}

		// A failed attempt restarts the lifetime exactly like a successful one.
		// Leaving snapshotCachedAt behind would send every later request back
		// into buildStatus - each one holding snapshotMu across a call to the
		// store that is already failing - and queue the public endpoints behind
		// the outage this fallback exists to survive.
		failedAt := time.Now()
		fiberlog.Errorf("uptime: snapshot refresh failed, serving cached status: %v", err)
		markSnapshotRefreshError(&u.snapshotCache, err, failedAt)
		u.snapshotCachedAt = failedAt
		return cloneSnapshot(u.snapshotCache), nil
	}

	u.snapshotCache = cloneSnapshot(snapshot)
	u.snapshotCachedAt = now
	u.snapshotTTL = snapshotLifetime(u.config.SampleInterval, snapshot)
	u.snapshotHasCache = true
	return cloneSnapshot(snapshot), nil
}

// snapshotLifetime reports how long a snapshot may be served. Endpoints carry
// their own interval, which Config.SampleInterval neither bounds nor defaults
// to, so the fastest service in the payload sets the pace: currentStatus turns
// a service down two intervals after its last heartbeat, and caching past that
// would keep reporting an endpoint up long after the snapshot itself stopped
// agreeing.
func snapshotLifetime(sampleInterval time.Duration, snapshot Snapshot) time.Duration {
	lifetime := sampleInterval
	for _, service := range snapshot.Services {
		// Seconds are what the payload carries, and the normalized config floor
		// is one second, so this only ever rounds an interval down.
		interval := time.Duration(service.SampleIntervalSeconds) * time.Second
		if interval > 0 && interval < lifetime {
			lifetime = interval
		}
	}
	return lifetime
}

func (u *runtime) buildStatus(ctx context.Context, now time.Time) (StatusResponse, error) {
	if err := u.ensureStoreReady(ctx); err != nil {
		u.setLastError(err)
		return StatusResponse{}, err
	}

	services, err := u.store.ListServices(ctx)
	if err != nil {
		u.setLastError(err)
		return StatusResponse{}, err
	}
	services = append([]storage.Service(nil), services...)
	sort.Slice(services, func(i, j int) bool {
		return services[i].ID < services[j].ID
	})

	days := dayRange(now, u.config.DaysToShow, u.config.Timezone)
	fromDay := days[0]
	toDay := days[len(days)-1]
	today := dayOf(now, u.config.Timezone)
	serviceIDs := serviceIDsFromServices(services)

	dailyRows, err := u.store.QueryDaily(ctx, storage.QueryDailyOptions{ServiceIDs: serviceIDs, FromDay: fromDay, ToDay: toDay})
	if err != nil {
		u.setLastError(err)
		return StatusResponse{}, err
	}
	todayRows, err := u.store.QueryTodaySamples(ctx, storage.QueryTodaySamplesOptions{ServiceIDs: serviceIDs, Day: today})
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

func serviceIDsFromServices(services []storage.Service) []string {
	serviceIDs := make([]string, 0, len(services))
	for _, service := range services {
		serviceIDs = append(serviceIDs, service.ID)
	}
	return serviceIDs
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

// markSnapshotRefreshError flags a served-stale snapshot as degraded. The store's
// own message stays in the log: this payload is public, so it carries the same
// fixed label storageStatus uses rather than whatever the backend reported.
func markSnapshotRefreshError(snapshot *Snapshot, err error, at time.Time) {
	if snapshot == nil || err == nil {
		return
	}
	snapshot.Storage.Status = "degraded"
	snapshot.Storage.LastError = storageErrorPublicLabel
	errorAt := at.UTC()
	snapshot.Storage.LastErrorAt = &errorAt
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
	if expectedSlots <= 0 {
		expectedSlots = 0
		upSlots = 0
		hasData = false
	}
	if upSlots > expectedSlots {
		upSlots = expectedSlots
	}
	if upSlots < 0 {
		upSlots = 0
	}

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
		// Keep backend error details in server-side logs and expose only a stable,
		// non-sensitive diagnostic through the public status payload.
		storage.LastError = storageErrorPublicLabel
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
	return expectedSlotsForWindow(dayOf(now, loc), createdAt, currentSlotStart(now, interval, loc), interval, loc, false)
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

func currentSlotStart(t time.Time, interval time.Duration, loc *time.Location) time.Time {
	if interval <= 0 {
		return t
	}
	start := startOfDay(t, loc)
	elapsed := t.In(loc).Sub(start)
	if elapsed <= 0 {
		return start
	}
	return start.Add((elapsed / interval) * interval)
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
