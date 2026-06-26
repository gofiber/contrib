package uptime

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"os"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	fiberlog "github.com/gofiber/fiber/v3/log"
)

// IDGenerator generates instance IDs.
type IDGenerator interface {
	NextID() int64
}

// Uptime records service heartbeats and serves uptime history.
type Uptime struct {
	config Config
	store  Store

	service  Service
	instance Instance

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	closeOnce sync.Once

	errMu     sync.RWMutex
	lastErr   error
	lastErrAt time.Time

	maintenanceMu      sync.Mutex
	lastMaintenance    time.Time
	lastMaintenanceDay string

	alertMu        sync.Mutex
	lastAlertCheck time.Time

	snapshotMu       sync.Mutex
	snapshotCache    Snapshot
	snapshotCachedAt time.Time
	snapshotHasCache bool
}

const (
	headerCacheControl        = "Cache-Control"
	headerXContentTypeOptions = "X-Content-Type-Options"
	headerAllow               = "Allow"
)

// New initializes the store, writes the first heartbeat, and starts recording.
func New(config ...Config) (*Uptime, error) {
	cfg, err := configDefault(config...).normalized()
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	if err := cfg.Store.Init(ctx); err != nil {
		return nil, fmt.Errorf("uptime: init store: %w", err)
	}

	now := time.Now()
	instanceID := cfg.InstanceID
	if instanceID == 0 && cfg.IDGenerator != nil {
		instanceID = cfg.IDGenerator.NextID()
	}
	if instanceID == 0 {
		instanceID = instanceIDForNode(cfg.NodeID)
	}

	hostname, _ := os.Hostname()
	service := Service{
		ID:             cfg.ServiceID,
		Name:           cfg.ServiceName,
		Description:    cfg.ServiceDescription,
		CreatedAt:      now,
		LastSeenAt:     now,
		SampleInterval: cfg.SampleInterval,
	}
	instance := Instance{
		ID:         instanceID,
		ServiceID:  cfg.ServiceID,
		Hostname:   hostname,
		PID:        os.Getpid(),
		StartedAt:  now,
		LastSeenAt: now,
	}

	if err := cfg.Store.UpsertService(ctx, service); err != nil {
		_ = cfg.Store.Close()
		return nil, fmt.Errorf("uptime: upsert service: %w", err)
	}
	if err := cfg.Store.UpsertInstance(ctx, instance); err != nil {
		_ = cfg.Store.Close()
		return nil, fmt.Errorf("uptime: upsert instance: %w", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	u := &Uptime{
		config:   cfg,
		store:    cfg.Store,
		service:  service,
		instance: instance,
		ctx:      runCtx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}

	if err := u.runMaintenance(ctx, now, true); err != nil {
		_ = cfg.Store.Close()
		return nil, fmt.Errorf("uptime: maintenance: %w", err)
	}
	_ = u.writeHeartbeat(ctx, now)

	go u.loop()
	return u, nil
}

// Close stops background work and closes the store.
func (u *Uptime) Close() error {
	if u == nil {
		return nil
	}
	var err error
	u.closeOnce.Do(func() {
		u.cancel()
		<-u.done
		err = u.store.Close()
	})
	return err
}

// LastError returns the most recent runtime store error, if any.
func (u *Uptime) LastError() (error, time.Time) {
	u.errMu.RLock()
	defer u.errMu.RUnlock()
	return u.lastErr, u.lastErrAt
}

// Handler returns a Fiber-native handler for the dashboard and JSON API.
func (u *Uptime) Handler() fiber.Handler {
	return func(c fiber.Ctx) error {
		if u == nil {
			return fiber.NewError(fiber.StatusInternalServerError, "uptime unavailable")
		}
		if u.config.Next != nil && u.config.Next(c) {
			return c.Next()
		}

		method := c.Method()
		if method != fiber.MethodGet && method != fiber.MethodHead {
			c.Set(headerAllow, "GET, HEAD")
			return fiber.ErrMethodNotAllowed
		}

		path := c.Path()
		uiPath := u.config.UI.Path
		switch path {
		case uiPath, uiPath + "/":
			return u.serveDashboard(c)
		case uiPath + "/api/status":
			return u.serveStatusJSON(c)
		default:
			return fiber.ErrNotFound
		}
	}
}

func (u *Uptime) serveStatusJSON(c fiber.Ctx) error {
	c.Set(headerCacheControl, "no-store")
	c.Set(headerXContentTypeOptions, "nosniff")
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSONCharsetUTF8)
	if c.Method() == fiber.MethodHead {
		c.Status(fiber.StatusOK)
		return nil
	}

	status, err := u.CachedSnapshot(c.Context())
	if err != nil {
		fiberlog.Errorf("uptime: status unavailable: %v", err)
		return fiber.NewError(fiber.StatusInternalServerError, "uptime status unavailable")
	}
	return c.Status(fiber.StatusOK).JSON(&status)
}

func (u *Uptime) serveDashboard(c fiber.Ctx) error {
	c.Set(headerCacheControl, "no-store")
	c.Set(headerXContentTypeOptions, "nosniff")
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	if c.Method() == fiber.MethodHead {
		c.Status(fiber.StatusOK)
		return nil
	}

	status, err := u.CachedSnapshot(c.Context())
	if err != nil {
		fiberlog.Errorf("uptime: dashboard unavailable: %v", err)
		return fiber.NewError(fiber.StatusInternalServerError, "uptime dashboard unavailable")
	}
	html, err := renderDashboardHTML(u.config, status)
	if err != nil {
		fiberlog.Errorf("uptime: dashboard render failed: %v", err)
		return fiber.NewError(fiber.StatusInternalServerError, "uptime dashboard render failed")
	}
	return c.Status(fiber.StatusOK).SendString(html)
}

func (u *Uptime) loop() {
	defer close(u.done)

	ticker := time.NewTicker(u.config.SampleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-u.ctx.Done():
			return
		case now := <-ticker.C:
			_ = u.writeHeartbeat(u.ctx, now)
		}
	}
}

func (u *Uptime) writeHeartbeat(ctx context.Context, now time.Time) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	heartbeat := Heartbeat{
		ServiceID:  u.config.ServiceID,
		InstanceID: u.instance.ID,
		Day:        dayOf(now, u.config.Timezone),
		Slot:       slotOf(now, u.config.SampleInterval, u.config.Timezone),
		SeenAt:     now,
	}
	if err := u.store.WriteHeartbeat(ctx, heartbeat); err != nil {
		u.setLastError(err)
		fiberlog.Errorf("uptime: write heartbeat: %v", err)
		return err
	}
	if err := u.runMaintenance(ctx, now, false); err != nil {
		return err
	}
	u.evaluateAlerts(ctx, now)
	u.clearLastError()
	return nil
}

func (u *Uptime) runMaintenance(ctx context.Context, now time.Time, force bool) error {
	today := dayOf(now, u.config.Timezone)

	u.maintenanceMu.Lock()
	defer u.maintenanceMu.Unlock()

	if !force && u.lastMaintenanceDay == today && now.Sub(u.lastMaintenance) < maintenanceInterval {
		return nil
	}

	expected := func(day string) int {
		return expectedSlotsForDay(day, u.config.SampleInterval, u.config.Timezone)
	}
	serviceIntervals, err := u.serviceIntervals(ctx)
	if err != nil {
		u.setLastError(err)
		fiberlog.Errorf("uptime: list services for maintenance: %v", err)
		return err
	}
	expectedForService := func(serviceID, day string) int {
		interval := serviceIntervals[serviceID]
		if interval < time.Second {
			interval = u.config.SampleInterval
		}
		return expectedSlotsForDay(day, interval, u.config.Timezone)
	}
	if err := u.store.RollupDaily(ctx, RollupOptions{
		BeforeDay:                  today,
		ExpectedSlotsForDay:        expected,
		ExpectedSlotsForServiceDay: expectedForService,
	}); err != nil {
		u.setLastError(err)
		fiberlog.Errorf("uptime: rollup daily: %v", err)
		return err
	}

	dailyBefore := addDays(today, -u.config.RetentionDays, u.config.Timezone)
	samplesBefore := addDays(today, -1, u.config.Timezone)
	if err := u.store.Cleanup(ctx, CleanupOptions{
		DailyBeforeDay:   dailyBefore,
		SamplesBeforeDay: samplesBefore,
	}); err != nil {
		u.setLastError(err)
		fiberlog.Errorf("uptime: cleanup: %v", err)
		return err
	}

	u.lastMaintenance = now
	u.lastMaintenanceDay = today
	return nil
}

func (u *Uptime) serviceIntervals(ctx context.Context) (map[string]time.Duration, error) {
	services, err := u.store.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	intervals := make(map[string]time.Duration, len(services))
	for _, service := range services {
		intervals[service.ID] = u.serviceSampleInterval(service)
	}
	return intervals, nil
}

func (u *Uptime) evaluateAlerts(ctx context.Context, now time.Time) {
	if u.config.Alert.Hook == nil {
		return
	}

	u.alertMu.Lock()
	if !u.lastAlertCheck.IsZero() && now.Sub(u.lastAlertCheck) < u.config.Alert.CheckInterval {
		u.alertMu.Unlock()
		return
	}
	u.lastAlertCheck = now
	u.alertMu.Unlock()

	stateStore, ok := u.store.(AlertStateStore)
	if !ok {
		fiberlog.Errorf("uptime: alert hook requires alert state support")
		return
	}

	services, err := u.store.ListServices(ctx)
	if err != nil {
		fiberlog.Errorf("uptime: list services for alerts: %v", err)
		return
	}
	for _, service := range services {
		interval := u.serviceSampleInterval(service)
		status := currentStatus(now, service.LastSeenAt, interval)
		decision, err := stateStore.ClaimAlertEvent(ctx, AlertState{
			ServiceID:         service.ID,
			Status:            status,
			LastSeenAt:        service.LastSeenAt,
			CheckedAt:         now,
			NotifyOnFirstDown: u.config.Alert.NotifyOnFirstDown,
		})
		if err != nil {
			fiberlog.Errorf("uptime: claim alert event for %s: %v", service.ID, err)
			continue
		}
		if !decision.Notify {
			continue
		}
		event := AlertEvent{
			ServiceID:      service.ID,
			ServiceName:    service.Name,
			Description:    service.Description,
			PreviousStatus: decision.PreviousStatus,
			CurrentStatus:  status,
			LastSeenAt:     service.LastSeenAt.UTC(),
			DetectedAt:     now.UTC(),
			SampleInterval: interval,
		}
		if status == AlertStatusDown && !service.LastSeenAt.IsZero() {
			event.DownFor = now.Sub(service.LastSeenAt)
		}
		if err := u.config.Alert.Hook(ctx, event); err != nil {
			fiberlog.Errorf("uptime: alert hook for %s: %v", service.ID, err)
		}
	}
}

func (u *Uptime) setLastError(err error) {
	if err == nil {
		return
	}
	u.errMu.Lock()
	u.lastErr = err
	u.lastErrAt = time.Now()
	u.errMu.Unlock()
}

func (u *Uptime) clearLastError() {
	u.errMu.Lock()
	u.lastErr = nil
	u.lastErrAt = time.Time{}
	u.errMu.Unlock()
}

func instanceIDForNode(nodeID int64) int64 {
	host, _ := os.Hostname()
	h := fnv.New64a()
	_, _ = h.Write([]byte(host))
	var buf [32]byte
	binary.LittleEndian.PutUint64(buf[0:8], uint64(time.Now().UnixNano()))
	binary.LittleEndian.PutUint64(buf[8:16], uint64(os.Getpid()))
	binary.LittleEndian.PutUint64(buf[16:24], uint64(nodeID))
	if _, err := rand.Read(buf[24:32]); err != nil {
		binary.LittleEndian.PutUint64(buf[24:32], uint64(time.Now().UnixNano()<<7))
	}
	_, _ = h.Write(buf[:])
	return int64(h.Sum64() & 0x7fffffffffffffff)
}
