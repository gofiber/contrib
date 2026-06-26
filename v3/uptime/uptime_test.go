package uptime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/uptime/internal/storage"
	"github.com/gofiber/fiber/v3"
)

func TestConfigDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Config{ServiceID: "api"}.normalized()
	requireNoError(t, err)

	requireEqual(t, "api", cfg.ServiceName)
	requireEqual(t, defaultSampleInterval, cfg.SampleInterval)
	requireEqual(t, defaultRetentionDays, cfg.RetentionDays)
	requireEqual(t, defaultDaysToShow, cfg.DaysToShow)
	requireEqual(t, defaultSQLitePath, cfg.SQLite.Path)
	requireEqual(t, defaultUIPath, cfg.UI.Path)
	requireEqual(t, defaultUITitle, cfg.UI.Title)
	requireEqual(t, defaultUIDescription, cfg.UI.Description)
	requireEqual(t, defaultUIFooter, cfg.UI.Footer)
	requireEqual(t, defaultGreenThreshold, cfg.UI.GreenThreshold)
	requireEqual(t, defaultYellowThreshold, cfg.UI.YellowThreshold)
	requireEqual(t, cfg.SampleInterval, cfg.Snapshot.CacheTTL)
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config Config
	}{
		{name: "missing service id", config: Config{}},
		{
			name: "sample interval too small",
			config: Config{
				ServiceID:      "api",
				SampleInterval: time.Millisecond,
			},
		},
		{
			name: "retention days invalid",
			config: Config{
				ServiceID:     "api",
				RetentionDays: -1,
			},
		},
		{
			name: "days to show invalid",
			config: Config{
				ServiceID:  "api",
				DaysToShow: -1,
			},
		},
		{
			name: "snapshot ttl too small",
			config: Config{
				ServiceID: "api",
				Snapshot:  SnapshotConfig{CacheTTL: time.Millisecond},
			},
		},
		{
			name: "threshold order invalid",
			config: Config{
				ServiceID: "api",
				UI: UIConfig{
					GreenThreshold:  0.90,
					YellowThreshold: 0.95,
				},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := tt.config.normalized()
			requireError(t, err)
		})
	}
}

func TestConfigClampsDaysToShow(t *testing.T) {
	t.Parallel()

	cfg, err := Config{
		ServiceID:     "api",
		RetentionDays: 7,
		DaysToShow:    30,
	}.normalized()
	requireNoError(t, err)
	requireEqual(t, 7, cfg.DaysToShow)
}

func TestHandlerDashboardHTML(t *testing.T) {
	t.Parallel()

	up, app := newTestApp(t)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/uptime", nil))
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, resp.Body.Close()) })

	body, err := io.ReadAll(resp.Body)
	requireNoError(t, err)
	bodyText := string(body)

	requireEqual(t, fiber.StatusOK, resp.StatusCode)
	requireEqual(t, fiber.MIMETextHTMLCharsetUTF8, resp.Header.Get(fiber.HeaderContentType))
	requireContains(t, bodyText, "<title>Fiber Uptime</title>")
	requireContains(t, bodyText, `const apiPath = "/uptime/api/status";`)
	requireContains(t, bodyText, up.config.ServiceID)
}

func TestHandlerHeadDashboard(t *testing.T) {
	t.Parallel()

	_, app := newTestApp(t)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodHead, "/uptime", nil))
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, resp.Body.Close()) })

	body, err := io.ReadAll(resp.Body)
	requireNoError(t, err)

	requireEqual(t, fiber.StatusOK, resp.StatusCode)
	requireEqual(t, fiber.MIMETextHTMLCharsetUTF8, resp.Header.Get(fiber.HeaderContentType))
	requireEqual(t, 0, len(body))
}

func TestHandlerStatusJSON(t *testing.T) {
	t.Parallel()

	_, app := newTestApp(t)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/uptime/api/status", nil))
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, resp.Body.Close()) })

	requireEqual(t, fiber.StatusOK, resp.StatusCode)
	requireEqual(t, fiber.MIMEApplicationJSONCharsetUTF8, resp.Header.Get(fiber.HeaderContentType))

	var status StatusResponse
	requireNoError(t, json.NewDecoder(resp.Body).Decode(&status))
	requireLen(t, status.Services, 1)
	requireEqual(t, "api", status.Services[0].ID)
	requireEqual(t, "ok", status.Storage.Status)
}

func TestHandlerStatusHead(t *testing.T) {
	t.Parallel()

	_, app := newTestApp(t)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodHead, "/uptime/api/status", nil))
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, resp.Body.Close()) })

	body, err := io.ReadAll(resp.Body)
	requireNoError(t, err)

	requireEqual(t, fiber.StatusOK, resp.StatusCode)
	requireEqual(t, fiber.MIMEApplicationJSONCharsetUTF8, resp.Header.Get(fiber.HeaderContentType))
	requireEqual(t, 0, len(body))
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	t.Parallel()

	_, app := newTestApp(t)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/uptime", nil))
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, resp.Body.Close()) })

	requireEqual(t, fiber.StatusMethodNotAllowed, resp.StatusCode)
	requireEqual(t, "GET, HEAD", resp.Header.Get(headerAllow))
}

func TestHandlerNotFound(t *testing.T) {
	t.Parallel()

	_, app := newTestApp(t)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/uptime/unknown", nil))
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, resp.Body.Close()) })

	requireEqual(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestHandlerNext(t *testing.T) {
	t.Parallel()

	up, err := New(Config{
		ServiceID:      "api",
		SampleInterval: time.Second,
		SQLite:         SQLiteConfig{Path: filepath.Join(t.TempDir(), "uptime.db")},
		Next: func(fiber.Ctx) bool {
			return true
		},
	})
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, up.Close()) })

	app := fiber.New()
	app.All("/uptime", up.Handler())

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/uptime", nil))
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, resp.Body.Close()) })

	requireEqual(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestCachedSnapshotReturnsClones(t *testing.T) {
	t.Parallel()

	store := newSnapshotStore()
	u := newSnapshotUptime(store)

	first, err := u.CachedSnapshot(context.Background())
	requireNoError(t, err)
	requireLen(t, first.Services, 1)
	first.Services[0].Name = "changed"
	first.Services[0].Daily[0].UpSlots = 99

	second, err := u.CachedSnapshot(context.Background())
	requireNoError(t, err)
	requireLen(t, second.Services, 1)
	requireEqual(t, "API", second.Services[0].Name)
	if second.Services[0].Daily[0].UpSlots == 99 {
		t.Fatalf("cached snapshot was mutated by caller")
	}
}

func TestCachedSnapshotReturnsStaleOnRefreshError(t *testing.T) {
	t.Parallel()

	store := newSnapshotStore()
	u := newSnapshotUptime(store)

	_, err := u.CachedSnapshot(context.Background())
	requireNoError(t, err)

	u.snapshotCachedAt = time.Now().Add(-2 * time.Minute)
	store.fail = true

	stale, err := u.CachedSnapshot(context.Background())
	requireNoError(t, err)
	requireEqual(t, "degraded", stale.Storage.Status)
	requireContains(t, stale.Storage.LastError, "store failed")
	if stale.Storage.LastErrorAt == nil {
		t.Fatal("last error time is nil")
	}
}

func TestSQLiteStoreRecordsHeartbeat(t *testing.T) {
	t.Parallel()

	up, err := New(Config{
		ServiceID:      "api",
		ServiceName:    "API",
		SampleInterval: time.Second,
		SQLite:         SQLiteConfig{Path: filepath.Join(t.TempDir(), "uptime.db")},
	})
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, up.Close()) })

	status, err := up.Snapshot(context.Background())
	requireNoError(t, err)
	requireLen(t, status.Services, 1)
	requireEqual(t, "api", status.Services[0].ID)
	requireEqual(t, "up", status.Services[0].CurrentStatus)
	if len(status.Services[0].Daily) == 0 {
		t.Fatal("daily status is empty")
	}
}

func TestCustomIDGenerator(t *testing.T) {
	t.Parallel()

	up, err := New(Config{
		ServiceID:      "api",
		SampleInterval: time.Second,
		IDGenerator:    staticIDGenerator{value: 42},
		SQLite:         SQLiteConfig{Path: filepath.Join(t.TempDir(), "uptime.db")},
	})
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, up.Close()) })

	requireEqual(t, int64(42), up.instance.ID)
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	up, err := New(Config{
		ServiceID:      "api",
		SampleInterval: time.Second,
		SQLite:         SQLiteConfig{Path: filepath.Join(t.TempDir(), "uptime.db")},
	})
	requireNoError(t, err)

	requireNoError(t, up.Close())
	requireNoError(t, up.Close())
}

func newTestApp(t *testing.T) (*Uptime, *fiber.App) {
	t.Helper()

	up, err := New(Config{
		ServiceID:      "api",
		ServiceName:    "API",
		SampleInterval: time.Second,
		SQLite:         SQLiteConfig{Path: filepath.Join(t.TempDir(), "uptime.db")},
	})
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, up.Close()) })

	app := fiber.New()
	app.All("/uptime", up.Handler())
	app.All("/uptime/*", up.Handler())
	return up, app
}

func newSnapshotUptime(store *snapshotStore) *Uptime {
	cfg, _ := Config{
		ServiceID:      "api",
		SampleInterval: time.Second,
		DaysToShow:     1,
		RetentionDays:  1,
		Timezone:       time.UTC,
		Snapshot:       SnapshotConfig{CacheTTL: time.Minute},
		UI: UIConfig{
			GreenThreshold:  defaultGreenThreshold,
			YellowThreshold: defaultYellowThreshold,
		},
	}.normalized()
	return &Uptime{
		config: cfg,
		store:  store,
	}
}

type snapshotStore struct {
	fail bool
	now  time.Time
}

func newSnapshotStore() *snapshotStore {
	return &snapshotStore{now: time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)}
}

func (s *snapshotStore) Init(context.Context) error                           { return nil }
func (s *snapshotStore) UpsertService(context.Context, storage.Service) error { return nil }
func (s *snapshotStore) UpsertInstance(context.Context, storage.Instance) error {
	return nil
}
func (s *snapshotStore) WriteHeartbeat(context.Context, storage.Heartbeat) error {
	return nil
}
func (s *snapshotStore) RollupDaily(context.Context, storage.RollupOptions) error { return nil }
func (s *snapshotStore) Cleanup(context.Context, storage.CleanupOptions) error    { return nil }
func (s *snapshotStore) ListServices(context.Context) ([]storage.Service, error) {
	if s.fail {
		return nil, errors.New("store failed")
	}
	return []storage.Service{
		{
			ID:             "api",
			Name:           "API",
			CreatedAt:      s.now.Add(-time.Hour),
			LastSeenAt:     s.now,
			SampleInterval: time.Second,
		},
	}, nil
}
func (s *snapshotStore) QueryDaily(context.Context, storage.QueryDailyOptions) ([]storage.DailyStatus, error) {
	return nil, nil
}
func (s *snapshotStore) QueryTodaySamples(context.Context, storage.QueryTodaySamplesOptions) ([]storage.TodaySampleStatus, error) {
	return []storage.TodaySampleStatus{{ServiceID: "api", Day: dayOf(time.Now(), time.UTC), UpSlots: 1}}, nil
}
func (s *snapshotStore) Close() error { return nil }

type staticIDGenerator struct {
	value int64
}

func (g staticIDGenerator) NextID() int64 {
	return g.value
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func requireError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
}

func requireEqual[T comparable](t *testing.T, want, got T) {
	t.Helper()
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func requireLen[T any](t *testing.T, values []T, want int) {
	t.Helper()
	if len(values) != want {
		t.Fatalf("len = %d, want %d", len(values), want)
	}
}

func requireContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("%q does not contain %q", got, want)
	}
}
