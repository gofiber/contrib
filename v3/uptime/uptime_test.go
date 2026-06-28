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
			name: "threshold order invalid",
			config: Config{
				ServiceID: "api",
				UI: UIConfig{
					GreenThreshold:  0.90,
					YellowThreshold: 0.95,
				},
			},
		},
		{
			name: "green threshold below range",
			config: Config{
				ServiceID: "api",
				UI: UIConfig{
					GreenThreshold:  -0.1,
					YellowThreshold: 0.9,
				},
			},
		},
		{
			name: "yellow threshold above range",
			config: Config{
				ServiceID: "api",
				UI: UIConfig{
					GreenThreshold:  1,
					YellowThreshold: 1.1,
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
	requireContains(t, bodyText, `id="summary-services"`)
	requireContains(t, bodyText, `id="summary-up"`)
	requireContains(t, bodyText, `id="summary-down"`)
	requireContains(t, bodyText, `const refreshMS =`)
	requireContains(t, bodyText, `10000`)
	requireContains(t, bodyText, `date.getFullYear() + "-" + pad(date.getMonth() + 1) + "-" + pad(date.getDate())`)
	requireContains(t, bodyText, `aria-label="Scroll up; scroll progress 0%"`)
	requireContains(t, bodyText, `let refreshInFlight = false;`)
	requireContains(t, bodyText, `signal: controller.signal`)
	requireContains(t, bodyText, `.bar:focus-visible`)
	requireContains(t, bodyText, `@media (prefers-color-scheme: dark)`)
	requireContains(t, bodyText, up.config.ServiceID)
	requireNotContains(t, bodyText, `data-theme=`)
	requireNotContains(t, bodyText, `data-background=`)
	requireNotContains(t, bodyText, `lang-toggle`)
	requireNotContains(t, bodyText, `theme-toggle`)
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

func TestHandlerHeadDashboardSurfacesSnapshotError(t *testing.T) {
	t.Parallel()

	store := newSnapshotStore()
	store.fail = true
	app := newSnapshotApp(newSnapshotUptime(store))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodHead, "/uptime", nil))
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, resp.Body.Close()) })

	body, err := io.ReadAll(resp.Body)
	requireNoError(t, err)

	requireEqual(t, fiber.StatusInternalServerError, resp.StatusCode)
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

func TestHandlerStatusHeadSurfacesSnapshotError(t *testing.T) {
	t.Parallel()

	store := newSnapshotStore()
	store.fail = true
	app := newSnapshotApp(newSnapshotUptime(store))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodHead, "/uptime/api/status", nil))
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, resp.Body.Close()) })

	body, err := io.ReadAll(resp.Body)
	requireNoError(t, err)

	requireEqual(t, fiber.StatusInternalServerError, resp.StatusCode)
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

	up, err := NewRuntime(Config{
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

func TestHandlerPassesThroughOutsideUIPath(t *testing.T) {
	t.Parallel()

	up, err := NewRuntime(Config{
		ServiceID:      "api",
		SampleInterval: time.Second,
		SQLite:         SQLiteConfig{Path: filepath.Join(t.TempDir(), "uptime.db")},
	})
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, up.Close()) })

	app := fiber.New()
	app.Use(up.Handler())
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, resp.Body.Close()) })

	body, err := io.ReadAll(resp.Body)
	requireNoError(t, err)

	requireEqual(t, fiber.StatusOK, resp.StatusCode)
	requireEqual(t, "ok", string(body))
}

func TestNewPanicsOnInvalidConfig(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = New(Config{})
}

func TestSnapshotBuildsFreshStatus(t *testing.T) {
	t.Parallel()

	store := newSnapshotStore()
	u := newSnapshotUptime(store)

	first, err := u.Snapshot(context.Background())
	requireNoError(t, err)
	requireLen(t, first.Services, 1)

	store.services[0].Name = "Updated API"
	second, err := u.Snapshot(context.Background())
	requireNoError(t, err)
	requireLen(t, second.Services, 1)
	requireEqual(t, "API", first.Services[0].Name)
	requireEqual(t, "Updated API", second.Services[0].Name)
}

func TestSnapshotReturnsStoreError(t *testing.T) {
	t.Parallel()

	store := newSnapshotStore()
	u := newSnapshotUptime(store)
	store.fail = true

	_, err := u.Snapshot(context.Background())
	requireError(t, err)
	requireContains(t, err.Error(), "store failed")
}

func TestDayStatusUsesServiceCreatedAtForToday(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 16, 0, 0, 0, time.UTC)
	createdAt := time.Date(2026, 6, 26, 15, 0, 0, 0, time.UTC)
	store := newSnapshotStore()
	store.services = []storage.Service{
		{
			ID:             "api",
			Name:           "API",
			CreatedAt:      createdAt,
			LastSeenAt:     now,
			SampleInterval: time.Minute,
		},
	}
	store.today = []storage.TodaySampleStatus{
		{ServiceID: "api", Day: dayOf(now, time.UTC), UpSlots: 61},
	}
	u := newSnapshotUptimeWithConfig(store, Config{
		ServiceID:      "api",
		SampleInterval: time.Minute,
		DaysToShow:     1,
		RetentionDays:  1,
		Timezone:       time.UTC,
	})

	status, err := u.buildStatus(context.Background(), now)
	requireNoError(t, err)
	requireLen(t, status.Services, 1)
	requireLen(t, status.Services[0].Daily, 1)

	day := status.Services[0].Daily[0]
	requireEqual(t, 61, day.ExpectedSlots)
	requireEqual(t, 61, day.UpSlots)
	requireEqual(t, "green", day.Status)
}

func TestRollupExpectedSlotsUsesServiceCreatedAt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	createdAt := time.Date(2026, 6, 25, 15, 0, 0, 0, time.UTC)
	store := newSnapshotStore()
	store.services = []storage.Service{
		{
			ID:             "api",
			Name:           "API",
			CreatedAt:      createdAt,
			LastSeenAt:     now,
			SampleInterval: time.Minute,
		},
	}
	u := newSnapshotUptimeWithConfig(store, Config{
		ServiceID:      "api",
		SampleInterval: time.Minute,
		DaysToShow:     2,
		RetentionDays:  2,
		Timezone:       time.UTC,
	})

	requireNoError(t, u.runMaintenance(context.Background(), now, true))
	requireEqual(t, 1, store.rollupCalls)
	if store.rollupOptions.ExpectedSlotsForServiceDay == nil {
		t.Fatal("service-aware expected slots callback is nil")
	}

	requireEqual(t, 540, store.rollupOptions.ExpectedSlotsForServiceDay("api", "2026-06-25"))
	requireEqual(t, 0, store.rollupOptions.ExpectedSlotsForServiceDay("api", "2026-06-24"))
}

func TestBuildStatusDoesNotClearRuntimeError(t *testing.T) {
	t.Parallel()

	store := newSnapshotStore()
	u := newSnapshotUptime(store)
	runtimeErr := errors.New("write heartbeat failed")
	u.setLastError(runtimeErr)

	status, err := u.buildStatus(context.Background(), store.now)
	requireNoError(t, err)
	requireEqual(t, "degraded", status.Storage.Status)
	requireContains(t, status.Storage.LastError, "write heartbeat failed")

	_, lastErr := u.LastError()
	if !errors.Is(lastErr, runtimeErr) {
		t.Fatalf("last error = %v, want %v", lastErr, runtimeErr)
	}
}

func TestNewReturnsErrorWhenInitialHeartbeatFails(t *testing.T) {
	t.Parallel()

	store := newSnapshotStore()
	store.writeHeartbeatErr = errors.New("write heartbeat failed")
	cfg, err := Config{
		ServiceID:      "api",
		SampleInterval: time.Second,
		DaysToShow:     1,
		RetentionDays:  1,
		Timezone:       time.UTC,
	}.normalized()
	requireNoError(t, err)

	up, err := newWithStore(cfg, store, store.now)
	requireError(t, err)
	if up != nil {
		t.Fatal("uptime instance should be nil")
	}
	requireContains(t, err.Error(), "initial heartbeat")
	requireEqual(t, 1, store.closeCalls)
}

func TestNewClosesStoreWhenInitFails(t *testing.T) {
	t.Parallel()

	store := newSnapshotStore()
	store.initErr = errors.New("init failed")
	cfg, err := Config{
		ServiceID:      "api",
		SampleInterval: time.Second,
		DaysToShow:     1,
		RetentionDays:  1,
		Timezone:       time.UTC,
	}.normalized()
	requireNoError(t, err)

	up, err := newWithStore(cfg, store, store.now)
	requireError(t, err)
	if up != nil {
		t.Fatal("uptime instance should be nil")
	}
	requireContains(t, err.Error(), "init store")
	requireEqual(t, 1, store.closeCalls)
}

func TestSQLiteStoreRecordsHeartbeat(t *testing.T) {
	t.Parallel()

	up, err := NewRuntime(Config{
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

	up, err := NewRuntime(Config{
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

	up, err := NewRuntime(Config{
		ServiceID:      "api",
		SampleInterval: time.Second,
		SQLite:         SQLiteConfig{Path: filepath.Join(t.TempDir(), "uptime.db")},
	})
	requireNoError(t, err)

	requireNoError(t, up.Close())
	requireNoError(t, up.Close())
}

func TestCloseAllowsNilAndZeroValue(t *testing.T) {
	t.Parallel()

	var nilUptime *Uptime
	requireNoError(t, nilUptime.Close())

	var zero Uptime
	requireNoError(t, zero.Close())
}

func TestLastErrorAllowsNilReceiver(t *testing.T) {
	t.Parallel()

	var nilUptime *Uptime
	at, err := nilUptime.LastError()
	if err != nil {
		t.Fatalf("last error = %v, want nil", err)
	}
	if !at.IsZero() {
		t.Fatalf("last error time = %v, want zero", at)
	}
}

func newTestApp(t *testing.T) (*Uptime, *fiber.App) {
	t.Helper()

	up, err := NewRuntime(Config{
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

func newSnapshotApp(up *Uptime) *fiber.App {
	app := fiber.New()
	app.All("/uptime", up.Handler())
	app.All("/uptime/*", up.Handler())
	return app
}

func newSnapshotUptime(store *snapshotStore) *Uptime {
	return newSnapshotUptimeWithConfig(store, Config{
		ServiceID:      "api",
		SampleInterval: time.Second,
		DaysToShow:     1,
		RetentionDays:  1,
		Timezone:       time.UTC,
	})
}

func newSnapshotUptimeWithConfig(store *snapshotStore, config Config) *Uptime {
	if config.UI.GreenThreshold == 0 {
		config.UI.GreenThreshold = defaultGreenThreshold
	}
	if config.UI.YellowThreshold == 0 {
		config.UI.YellowThreshold = defaultYellowThreshold
	}
	cfg, _ := config.normalized()
	return &Uptime{
		config: cfg,
		store:  store,
	}
}

type snapshotStore struct {
	fail              bool
	initErr           error
	now               time.Time
	services          []storage.Service
	daily             []storage.DailyStatus
	today             []storage.TodaySampleStatus
	writeHeartbeatErr error
	rollupCalls       int
	cleanupCalls      int
	rollupOptions     storage.RollupOptions
	closeCalls        int
}

func newSnapshotStore() *snapshotStore {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	return &snapshotStore{
		now: now,
		services: []storage.Service{
			{
				ID:             "api",
				Name:           "API",
				CreatedAt:      now.Add(-time.Hour),
				LastSeenAt:     now,
				SampleInterval: time.Second,
			},
		},
		today: []storage.TodaySampleStatus{
			{ServiceID: "api", Day: dayOf(now, time.UTC), UpSlots: 1},
		},
	}
}

func (s *snapshotStore) Init(context.Context) error { return s.initErr }
func (s *snapshotStore) UpsertService(_ context.Context, service storage.Service) error {
	for i := range s.services {
		if s.services[i].ID == service.ID {
			s.services[i] = service
			return nil
		}
	}
	s.services = append(s.services, service)
	return nil
}
func (s *snapshotStore) UpsertInstance(context.Context, storage.Instance) error {
	return nil
}
func (s *snapshotStore) WriteHeartbeat(context.Context, storage.Heartbeat) error {
	if s.writeHeartbeatErr != nil {
		return s.writeHeartbeatErr
	}
	return nil
}
func (s *snapshotStore) RollupDaily(_ context.Context, options storage.RollupOptions) error {
	s.rollupCalls++
	s.rollupOptions = options
	return nil
}
func (s *snapshotStore) Cleanup(context.Context, storage.CleanupOptions) error {
	s.cleanupCalls++
	return nil
}
func (s *snapshotStore) ListServices(context.Context) ([]storage.Service, error) {
	if s.fail {
		return nil, errors.New("store failed")
	}
	return append([]storage.Service(nil), s.services...), nil
}
func (s *snapshotStore) QueryDaily(_ context.Context, options storage.QueryDailyOptions) ([]storage.DailyStatus, error) {
	var statuses []storage.DailyStatus
	for _, status := range s.daily {
		if options.FromDay != "" && status.Day < options.FromDay {
			continue
		}
		if options.ToDay != "" && status.Day > options.ToDay {
			continue
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}
func (s *snapshotStore) QueryTodaySamples(_ context.Context, options storage.QueryTodaySamplesOptions) ([]storage.TodaySampleStatus, error) {
	var statuses []storage.TodaySampleStatus
	for _, status := range s.today {
		if options.Day != "" && status.Day != options.Day {
			continue
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}
func (s *snapshotStore) Close() error {
	s.closeCalls++
	return nil
}

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

func requireNotContains(t *testing.T, got, want string) {
	t.Helper()
	if strings.Contains(got, want) {
		t.Fatalf("%q contains %q", got, want)
	}
}
