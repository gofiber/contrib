package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureSQLiteDirCreatesParentForFileURI(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "nested", "uptime.db")
	uri := "file:" + filepath.ToSlash(dbPath) + "?cache=shared"

	if err := ensureSQLiteDir(uri); err != nil {
		t.Fatalf("ensureSQLiteDir() error = %v", err)
	}
	if _, err := os.Stat(filepath.Dir(dbPath)); err != nil {
		t.Fatalf("parent dir was not created: %v", err)
	}
}

// TestSQLiteStoreRollupAndCleanupLifecycle drives the store across a day
// boundary: write samples for past days, roll them up into finalized daily
// rows, then verify retention deletes old samples and daily rows while keeping
// the most recent day.
func TestSQLiteStoreRollupAndCleanupLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewSQLiteStore(SQLiteConfig{Path: filepath.Join(t.TempDir(), "uptime.db")})
	mustNoErr(t, store.Init(ctx))
	t.Cleanup(func() { mustNoErr(t, store.Close()) })

	created := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	mustNoErr(t, store.UpsertService(ctx, Service{ID: "api", Name: "API", CreatedAt: created, LastSeenAt: created, SampleInterval: time.Minute}))
	mustNoErr(t, store.UpsertInstance(ctx, Instance{ID: 1, ServiceID: "api", StartedAt: created, LastSeenAt: created}))

	seenAt := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	writeHeartbeat := func(day string, slot int64) {
		mustNoErr(t, store.WriteHeartbeat(ctx, Heartbeat{ServiceID: "api", InstanceID: 1, Day: day, Slot: slot, SeenAt: seenAt}))
	}
	writeHeartbeat("2026-06-23", 5)
	writeHeartbeat("2026-06-25", 0)
	writeHeartbeat("2026-06-25", 1)
	writeHeartbeat("2026-06-25", 1) // duplicate slot must collapse to one up slot

	mustNoErr(t, store.RollupDaily(ctx, RollupOptions{
		BeforeDay:                  "2026-06-26",
		ExpectedSlotsForServiceDay: func(string, string) int { return 1440 },
	}))

	daily := queryDailyMap(t, store)
	if got := daily["2026-06-25"]; got.UpSlots != 2 || got.ExpectedSlots != 1440 || !got.Finalized {
		t.Fatalf("rolled-up day = %+v, want up=2 expected=1440 finalized=true", got)
	}
	if _, ok := daily["2026-06-23"]; !ok {
		t.Fatal("expected daily row for 2026-06-23 before cleanup")
	}

	mustNoErr(t, store.Cleanup(ctx, CleanupOptions{
		DailyBeforeDay:   "2026-06-24",
		SamplesBeforeDay: "2026-06-25",
	}))

	daily = queryDailyMap(t, store)
	if _, ok := daily["2026-06-23"]; ok {
		t.Fatal("daily row for 2026-06-23 should be removed by retention")
	}
	if _, ok := daily["2026-06-25"]; !ok {
		t.Fatal("daily row for 2026-06-25 should survive retention")
	}

	today, err := store.QueryTodaySamples(ctx, QueryTodaySamplesOptions{Day: "2026-06-25"})
	mustNoErr(t, err)
	if len(today) != 1 || today[0].UpSlots != 2 {
		t.Fatalf("samples for 2026-06-25 = %+v, want one row with up=2", today)
	}
	old, err := store.QueryTodaySamples(ctx, QueryTodaySamplesOptions{Day: "2026-06-23"})
	mustNoErr(t, err)
	if len(old) != 0 {
		t.Fatalf("samples for 2026-06-23 should be removed by retention, got %+v", old)
	}
}

func queryDailyMap(t *testing.T, store *SQLiteStore) map[string]DailyStatus {
	t.Helper()

	rows, err := store.QueryDaily(context.Background(), QueryDailyOptions{FromDay: "2026-06-01", ToDay: "2026-06-30"})
	mustNoErr(t, err)
	out := make(map[string]DailyStatus, len(rows))
	for _, row := range rows {
		out[row.Day] = row
	}
	return out
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
