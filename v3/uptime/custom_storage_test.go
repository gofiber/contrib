package uptime_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/uptime"
	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	"github.com/gofiber/fiber/v3"
)

var _ uptimestorage.Store = (*customStore)(nil)

func TestCustomStorageBackend(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	store := &closableCustomStore{customStore: newCustomStore(uptimestorage.Service{
		ID:             "z",
		Name:           "Z",
		CreatedAt:      now.Add(-time.Hour),
		LastSeenAt:     now,
		SampleInterval: time.Hour,
	})}
	app := fiber.New()
	app.Use(uptime.New(uptime.Config{
		App:              app,
		Storage:          store,
		ServiceID:        "a",
		ServiceName:      "A",
		SampleInterval:   time.Hour,
		RetentionDays:    1,
		DaysToShow:       1,
		Timezone:         time.UTC,
		StorageKeyPrefix: "ignored-by-custom-storage",
	}))
	t.Cleanup(func() { _ = app.Shutdown() })

	service, heartbeatSlots := store.serviceState("a")
	if service.ID != "a" {
		t.Fatalf("registered service = %+v, want a", service)
	}
	if heartbeatSlots != 1 {
		t.Fatalf("initial heartbeat slots = %d, want 1", heartbeatSlots)
	}

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/uptime/api/status", nil))
	if err != nil {
		t.Fatalf("status request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status code = %d, want %d", resp.StatusCode, fiber.StatusOK)
	}

	var status uptime.StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Storage.Driver != "custom-test" {
		t.Fatalf("storage driver = %q, want custom-test", status.Storage.Driver)
	}
	if len(status.Services) != 2 || status.Services[0].ID != "a" || status.Services[1].ID != "z" {
		t.Fatalf("services = %+v, want deterministic order [a z]", status.Services)
	}

	if err := app.Shutdown(); err != nil {
		t.Fatalf("shutdown app: %v", err)
	}
	if store.closeCount() != 0 {
		t.Fatal("uptime closed caller-owned custom storage")
	}
}

type sampleKey struct {
	serviceID string
	day       string
}

type customStore struct {
	mu        sync.Mutex
	services  map[string]uptimestorage.Service
	instances map[int64]uptimestorage.Instance
	slots     map[sampleKey]map[int64]struct{}
}

func newCustomStore(services ...uptimestorage.Service) *customStore {
	store := &customStore{
		services:  make(map[string]uptimestorage.Service, len(services)),
		instances: make(map[int64]uptimestorage.Instance),
		slots:     make(map[sampleKey]map[int64]struct{}),
	}
	for _, service := range services {
		store.services[service.ID] = service
	}
	return store
}

func (*customStore) Name() string {
	return "custom-test"
}

func (s *customStore) UpsertService(_ context.Context, service uptimestorage.Service) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.services[service.ID]
	if ok {
		if !current.CreatedAt.IsZero() {
			service.CreatedAt = current.CreatedAt
		}
		if current.LastSeenAt.After(service.LastSeenAt) {
			service.LastSeenAt = current.LastSeenAt
		}
	}
	s.services[service.ID] = service
	return nil
}

func (s *customStore) UpsertInstance(_ context.Context, instance uptimestorage.Instance) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.instances[instance.ID]
	if ok {
		if !current.StartedAt.IsZero() {
			instance.StartedAt = current.StartedAt
		}
		if current.LastSeenAt.After(instance.LastSeenAt) {
			instance.LastSeenAt = current.LastSeenAt
		}
	}
	s.instances[instance.ID] = instance
	return nil
}

func (s *customStore) WriteHeartbeat(_ context.Context, heartbeat uptimestorage.Heartbeat) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	service := s.services[heartbeat.ServiceID]
	if heartbeat.SeenAt.After(service.LastSeenAt) {
		service.LastSeenAt = heartbeat.SeenAt
		s.services[heartbeat.ServiceID] = service
	}
	instance := s.instances[heartbeat.InstanceID]
	if heartbeat.SeenAt.After(instance.LastSeenAt) {
		instance.LastSeenAt = heartbeat.SeenAt
		s.instances[heartbeat.InstanceID] = instance
	}
	key := sampleKey{serviceID: heartbeat.ServiceID, day: heartbeat.Day}
	if s.slots[key] == nil {
		s.slots[key] = make(map[int64]struct{})
	}
	s.slots[key][heartbeat.Slot] = struct{}{}
	return nil
}

func (*customStore) RollupDaily(context.Context, uptimestorage.RollupOptions) error {
	return nil
}

func (*customStore) Cleanup(context.Context, uptimestorage.CleanupOptions) error {
	return nil
}

func (s *customStore) ListServices(context.Context) ([]uptimestorage.Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	services := make([]uptimestorage.Service, 0, len(s.services))
	for _, service := range s.services {
		services = append(services, service)
	}
	sort.Slice(services, func(i, j int) bool {
		return services[i].ID > services[j].ID
	})
	return services, nil
}

func (*customStore) QueryDaily(context.Context, uptimestorage.QueryDailyOptions) ([]uptimestorage.DailyStatus, error) {
	return nil, nil
}

func (s *customStore) QueryTodaySamples(_ context.Context, options uptimestorage.QueryTodaySamplesOptions) ([]uptimestorage.TodaySampleStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var statuses []uptimestorage.TodaySampleStatus
	for key, slots := range s.slots {
		if key.day != options.Day || !serviceSelected(options.ServiceIDs, key.serviceID) {
			continue
		}
		statuses = append(statuses, uptimestorage.TodaySampleStatus{
			ServiceID: key.serviceID,
			Day:       key.day,
			UpSlots:   len(slots),
		})
	}
	return statuses, nil
}

func (s *customStore) serviceState(serviceID string) (uptimestorage.Service, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	service := s.services[serviceID]
	heartbeatSlots := 0
	for key, slots := range s.slots {
		if key.serviceID == serviceID {
			heartbeatSlots += len(slots)
		}
	}
	return service, heartbeatSlots
}

type closableCustomStore struct {
	*customStore
	closeMu sync.Mutex
	closes  int
}

func (s *closableCustomStore) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	s.closes++
	return nil
}

func (s *closableCustomStore) closeCount() int {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	return s.closes
}

func serviceSelected(serviceIDs []string, serviceID string) bool {
	if serviceIDs == nil {
		return true
	}
	for _, selected := range serviceIDs {
		if selected == serviceID {
			return true
		}
	}
	return false
}
