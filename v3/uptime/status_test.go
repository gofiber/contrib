package uptime

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/uptime/internal/storage"
	"github.com/gofiber/fiber/v3"
)

func TestServiceSummaryStatusAPI(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		createdAt time.Time
		daily     []storage.DailyStatus
		todayUp   int
		wantDays  []DayStatus
		want      ServiceSummary
	}{
		{
			name:      "weighted partial creation day and healthy today",
			createdAt: now.Add(-24 * time.Hour),
			daily:     []storage.DailyStatus{{ServiceID: "api", Day: "2026-09-08", UpSlots: 6, ExpectedSlots: 12, Finalized: true}},
			todayUp:   12,
			wantDays: []DayStatus{
				{Day: "2026-09-07", Status: "gray"},
				{Day: "2026-09-08", UptimeRate: .5, UpSlots: 6, ExpectedSlots: 12, EstimatedDowntimeSeconds: 21600, Finalized: true, HasData: true, Status: "red"},
				{Day: "2026-09-09", UptimeRate: 1, UpSlots: 12, ExpectedSlots: 12, HasData: true, Status: "green"},
			},
			want: ServiceSummary{HasData: true, AvailabilityRate: .75, EstimatedDowntimeSeconds: 21600, AffectedDays: 1, StableStreakDays: 1},
		},
		{
			name:      "older service capped at returned window",
			createdAt: now.AddDate(0, 0, -10),
			daily: []storage.DailyStatus{
				{ServiceID: "api", Day: "2026-09-07", UpSlots: 24, ExpectedSlots: 24, Finalized: true},
				{ServiceID: "api", Day: "2026-09-08", UpSlots: 24, ExpectedSlots: 24, Finalized: true},
			},
			todayUp: 12,
			wantDays: []DayStatus{
				{Day: "2026-09-07", UptimeRate: 1, UpSlots: 24, ExpectedSlots: 24, Finalized: true, HasData: true, Status: "green"},
				{Day: "2026-09-08", UptimeRate: 1, UpSlots: 24, ExpectedSlots: 24, Finalized: true, HasData: true, Status: "green"},
				{Day: "2026-09-09", UptimeRate: 1, UpSlots: 12, ExpectedSlots: 12, HasData: true, Status: "green"},
			},
			want: ServiceSummary{HasData: true, AvailabilityRate: 1, StableStreakDays: 3, StableStreakCapped: true},
		},
		{
			name:      "new service without a completed slot",
			createdAt: now,
			todayUp:   1,
			wantDays: []DayStatus{
				{Day: "2026-09-07", Status: "gray"},
				{Day: "2026-09-08", Status: "gray"},
				{Day: "2026-09-09", Status: "gray"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &summaryReadStore{snapshotStore: newSnapshotStore()}
			store.services = []storage.Service{{ID: "api", Name: "API", CreatedAt: tt.createdAt, LastSeenAt: now, SampleInterval: time.Hour}}
			store.daily = tt.daily
			store.today = []storage.TodaySampleStatus{{ServiceID: "api", Day: "2026-09-09", UpSlots: tt.todayUp}}
			u := newSnapshotUptimeWithConfig(store.snapshotStore, Config{
				ServiceID: "api", SampleInterval: time.Hour, DaysToShow: 3, RetentionDays: 3, Timezone: time.UTC,
			})
			u.store = store
			status, err := u.buildStatus(context.Background(), now)
			requireNoError(t, err)
			requireLen(t, status.Services, 1)
			requireEqual(t, tt.want, status.Services[0].Summary)
			requireEqual(t, statusUp, status.Services[0].CurrentStatus)
			if !slices.Equal(tt.wantDays, status.Services[0].Daily) {
				t.Fatalf("daily = %+v, want %+v", status.Services[0].Daily, tt.wantDays)
			}

			// Exercise the real JSON handler using this deterministic snapshot.
			u.snapshotCache = cloneSnapshot(status)
			u.snapshotCachedAt = time.Now()
			u.snapshotTTL = time.Hour
			u.snapshotHasCache = true
			app := newSnapshotApp(u)
			resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/uptime/api/status", nil))
			requireNoError(t, err)
			t.Cleanup(func() { requireNoError(t, resp.Body.Close()) })
			requireEqual(t, fiber.StatusOK, resp.StatusCode)
			var payload struct {
				Services []struct {
					Summary       *ServiceSummary `json:"summary"`
					Daily         []DayStatus     `json:"daily"`
					CurrentStatus string          `json:"current_status"`
				}
			}
			requireNoError(t, json.NewDecoder(resp.Body).Decode(&payload))
			requireLen(t, payload.Services, 1)
			if payload.Services[0].Summary == nil {
				t.Fatal("JSON service is missing the additive summary object")
			}
			requireEqual(t, tt.want, *payload.Services[0].Summary)
			requireEqual(t, statusUp, payload.Services[0].CurrentStatus)
			if !slices.Equal(tt.wantDays, payload.Services[0].Daily) {
				t.Fatal("JSON changed daily history")
			}
			requireEqual(t, 1, store.listServicesCalls)
			requireEqual(t, 1, store.dailyReads)
			requireEqual(t, 1, store.todayReads)
			requireEqual(t, 0, store.rollupCalls)
			requireEqual(t, 0, store.cleanupCalls)

			status.Services[0].Summary = ServiceSummary{}
			requireEqual(t, tt.want, u.snapshotCache.Services[0].Summary)
		})
	}
}

// Count the existing logical reads without adding a separate mock backend.
type summaryReadStore struct {
	*snapshotStore
	dailyReads int
	todayReads int
}

func (s *summaryReadStore) QueryDaily(ctx context.Context, options storage.QueryDailyOptions) ([]storage.DailyStatus, error) {
	s.dailyReads++
	return s.snapshotStore.QueryDaily(ctx, options)
}

func (s *summaryReadStore) QueryTodaySamples(ctx context.Context, options storage.QueryTodaySamplesOptions) ([]storage.TodaySampleStatus, error) {
	s.todayReads++
	return s.snapshotStore.QueryTodaySamples(ctx, options)
}

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

func TestExpectedSlotsSoFarSinceExcludesInProgressSlot(t *testing.T) {
	t.Parallel()

	loc := time.UTC
	createdAt := time.Date(2026, 6, 26, 0, 0, 0, 0, loc)

	tests := []struct {
		name     string
		now      time.Time
		interval time.Duration
		want     int
	}{
		{
			name:     "one second after midnight",
			now:      time.Date(2026, 6, 26, 0, 0, 1, 0, loc),
			interval: 3 * time.Second,
			want:     0,
		},
		{
			name:     "first slot boundary",
			now:      time.Date(2026, 6, 26, 0, 0, 3, 0, loc),
			interval: 3 * time.Second,
			want:     1,
		},
		{
			name:     "inside second slot",
			now:      time.Date(2026, 6, 26, 0, 0, 4, 0, loc),
			interval: 3 * time.Second,
			want:     1,
		},
		{
			name:     "thirty seconds after midnight",
			now:      time.Date(2026, 6, 26, 0, 0, 30, 0, loc),
			interval: 3 * time.Second,
			want:     10,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := expectedSlotsSoFarSince(tt.now, createdAt, tt.interval, loc)
			requireEqual(t, tt.want, got)
		})
	}
}

func TestExpectedSlotsForServiceDayCountsWholeDay(t *testing.T) {
	t.Parallel()

	requireEqual(t, 24, expectedSlotsForServiceDay("2026-06-26", time.Time{}, time.Hour, time.UTC))
	requireEqual(t, 1440, expectedSlotsForServiceDay("2026-06-26", time.Time{}, time.Minute, time.UTC))
	requireEqual(t, 0, expectedSlotsForServiceDay("not-a-day", time.Time{}, time.Minute, time.UTC))
}

func TestExpectedSlotsForServiceDayHandlesDSTTransitions(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("America/New_York")
	requireNoError(t, err)

	requireEqual(t, 23, expectedSlotsForServiceDay("2026-03-08", time.Time{}, time.Hour, loc))
	requireEqual(t, 25, expectedSlotsForServiceDay("2026-11-01", time.Time{}, time.Hour, loc))
	requireEqual(t, 24, expectedSlotsForServiceDay("2026-06-26", time.Time{}, time.Hour, loc))
}
